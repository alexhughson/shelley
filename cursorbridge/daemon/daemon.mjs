#!/usr/bin/env node
/**
 * Shelley <-> Cursor SDK bridge daemon.
 *
 * JSONL over stdin/stdout. One Cursor Agent per Shelley conversation+model.
 * Tool calls stay pending until Shelley’s loop runs them and sends tool_result.
 *
 * Requests:
 *   {id, op:"ping"}
 *   {id, op:"models"}
 *   {id, op:"prompt", agentId, oneshot?,
 *    model, cwd, seed, seedImages, message, images, tools}
 *   {id, op:"tool_result", req?, content, isError}
 *   {id, op:"steer", agentId, text}
 *   {id, op:"reset", agentId}
 *   {id, op:"cancel", agentId?}
 *
 * Prompt events (id = current request):
 *   {id, agent, event:"delta", kind:"text"|"thinking", text}
 *   {id, agent, event:"tool_calls", calls:[{id,name,args}]}  // execute() waiting
 *   {id, agent, event:"result", ok, text?, usage?, created?, error?}
 */

import { createInterface } from "node:readline";
import {
  Agent,
  AgentNotFoundError,
  Cursor,
  JsonlLocalAgentStore,
} from "@cursor/sdk";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const pendingToolResults = new Map(); // toolCallId -> {resolve, session}
const sessions = new Map();           // agentId -> session
const inflight = new Set();

let sawStdinEnd = false;

const storeRoot = path.join(os.homedir(), ".cache", "shelley-cursor-agents");
fs.mkdirSync(storeRoot, { recursive: true });
const store = new JsonlLocalAgentStore(storeRoot);

function isMissingAgent(e) {
  return e instanceof AgentNotFoundError || (e && e.code === "agent_not_found");
}

function normalizeError(e) {
  const info = {
    ok: false,
    error: String(e && e.message ? e.message : e),
  };
  if (e && typeof e.isRetryable === "boolean") info.retryable = e.isRetryable;
  if (e && typeof e.status === "number") info.status = e.status;
  if (e && typeof e.code === "string") info.code = e.code;
  if (e && e.constructor && e.constructor.name) info.kind = e.constructor.name;
  return info;
}

function isRetryableErr(e) {
  if (e && typeof e.isRetryable === "boolean") return e.isRetryable;
  const kind = e && e.constructor && e.constructor.name;
  return kind === "NetworkError" || kind === "RateLimitError";
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

function emit(session, obj) {
  send({ id: session.currentReqId, agent: session.agentId, ...obj });
}

function tokenUsageToBridge(usage) {
  if (!usage) return undefined;
  const u = {
    inputTokens: usage.inputTokens ?? 0,
    outputTokens: usage.outputTokens ?? 0,
    cacheReadTokens: usage.cacheReadTokens ?? 0,
    cacheWriteTokens: usage.cacheWriteTokens ?? 0,
    reasoningTokens: usage.reasoningTokens ?? 0,
  };
  u.totalTokens =
    usage.totalTokens ??
    u.inputTokens + u.outputTokens + u.cacheReadTokens + u.cacheWriteTokens;
  return u;
}

function modelSelection(model) {
  return model && typeof model === "object" ? model : { id: model };
}

function flushToolCalls(session) {
  session.flushScheduled = false;
  const calls = session.pendingEmits;
  session.pendingEmits = [];
  if (calls.length === 0) return;
  emit(session, { event: "tool_calls", calls });
}

function runToolOnShelley(session, toolCallId, name, args) {
  return new Promise((resolve) => {
    pendingToolResults.set(toolCallId, { resolve, session });
    session.pendingEmits.push({ id: toolCallId, name, args });
    if (!session.flushScheduled) {
      session.flushScheduled = true;
      setImmediate(() => flushToolCalls(session));
    }
    const timeout = setTimeout(() => {
      if (pendingToolResults.has(toolCallId)) {
        pendingToolResults.delete(toolCallId);
        resolve({
          content: [{ type: "text", text: `Shelley bridge: tool result for ${name} never arrived (timeout).` }],
          isError: true,
        });
      }
    }, 10 * 60 * 1000);
    if (timeout.unref) timeout.unref();
  });
}

function buildCustomTools(tools, session) {
  if (!tools || tools.length === 0) return undefined;
  const out = {};
  for (const t of tools) {
    out[t.name] = {
      description: t.description || "",
      inputSchema: t.inputSchema || { type: "object", properties: {} },
      async execute(args, context) {
        const toolCallId =
          (context && context.toolCallId) ||
          `tool-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
        return await runToolOnShelley(session, toolCallId, t.name, args);
      },
    };
  }
  return out;
}

function newSession(req, agentId) {
  return {
    agent: null,
    run: null,
    currentReqId: req.id,
    agentId,
    pendingEmits: [],
    flushScheduled: false,
    needForce: false,
  };
}

function agentOptions(req, session) {
  const customTools = buildCustomTools(req.tools, session);
  return {
    model: modelSelection(req.model),
    ...(customTools ? { tools: ["mcp"], mcpServers: [] } : { tools: [] }),
    local: {
      ...(req.cwd ? { cwd: req.cwd } : {}),
      store,
      ...(customTools ? { customTools } : {}),
    },
  };
}

function onDelta(session) {
  return ({ update }) => {
    if (update.type === "text-delta") {
      emit(session, { event: "delta", kind: "text", text: update.text });
    } else if (update.type === "thinking-delta") {
      emit(session, { event: "delta", kind: "thinking", text: update.text });
    }
  };
}

async function resetAgent(agentId) {
  const s = sessions.get(agentId);
  if (s) {
    if (s.run) {
      await s.run.cancel();
      s.run = null;
    }
    if (s.agent) {
      await s.agent[Symbol.asyncDispose]();
    }
    sessions.delete(agentId);
  }
  try {
    await Agent.delete(agentId);
  } catch (e) {
    if (!isMissingAgent(e)) throw e;
  }
}

async function getOrCreateAgent(agentId, req) {
  const existing = sessions.get(agentId);
  if (existing) {
    existing.currentReqId = req.id;
    return { session: existing, created: false };
  }
  const session = newSession(req, agentId);
  const opts = agentOptions(req, session);
  try {
    session.agent = await Agent.resume(agentId, opts);
    sessions.set(agentId, session);
    return { session, created: false };
  } catch (e) {
    if (!isMissingAgent(e)) throw e;
  }
  session.agent = await Agent.create({ ...opts, agentId });
  sessions.set(agentId, session);
  return { session, created: true };
}

function sendPayload(req, created) {
  const text = created ? (req.seed || req.message || "") : (req.message || "");
  const images = created ? (req.seedImages || req.images) : req.images;
  return {
    text,
    ...(images && images.length ? { images } : {}),
  };
}

function sendOptions(session, customTools) {
  const local = {};
  if (session.needForce) {
    local.force = true;
    session.needForce = false;
  }
  if (customTools) local.customTools = customTools;
  return {
    onDelta: onDelta(session),
    ...(Object.keys(local).length ? { local } : {}),
  };
}

async function finishRun(session, run, created) {
  session.run = run;
  try {
    const result = await run.wait();
    if (result.status === "error") {
      const err = normalizeError(result.error || { message: "run failed" });
      emit(session, { event: "result", ...err });
    } else if (result.status === "cancelled") {
      emit(session, { event: "result", ok: false, error: "run cancelled", cancelled: true });
    } else {
      emit(session, {
        event: "result",
        ok: true,
        text: result.result ?? "",
        usage: tokenUsageToBridge(result.usage),
        model: result.model && result.model.id,
        created: !!created,
      });
    }
  } finally {
    if (session.run === run) session.run = null;
  }
}

async function execOneshot(req) {
  const session = newSession(req, req.agentId || `oneshot-${req.id}`);
  const opts = agentOptions(req, session);
  const agent = await Agent.create(opts);
  session.agent = agent;
  try {
    const customTools = buildCustomTools(req.tools, session);
    const run = await agent.send(sendPayload(req, true), sendOptions(session, customTools));
    await finishRun(session, run, true);
  } finally {
    await agent[Symbol.asyncDispose]();
  }
}

async function execPrompt(req) {
  const id = req.id;
  inflight.add(id);
  try {
    if (req.oneshot) {
      await execOneshot(req);
      return;
    }
    const agentId = req.agentId;
    if (!agentId) {
      send({ id, event: "result", ok: false, error: "cursor bridge: prompt missing agentId" });
      return;
    }
    const { session, created } = await getOrCreateAgent(agentId, req);
    session.currentReqId = id;
    if (session.run) {
      await session.run.cancel();
      session.run = null;
      session.needForce = true;
    }
    const customTools = buildCustomTools(req.tools, session);
    const run = await session.agent.send(sendPayload(req, created), sendOptions(session, customTools));
    await finishRun(session, run, created);
  } catch (e) {
    const err = normalizeError(e);
    if (err.retryable === undefined) err.retryable = isRetryableErr(e);
    send({ id, event: "result", ...err });
  } finally {
    inflight.delete(id);
    if (sawStdinEnd && inflight.size === 0) {
      process.exit(0);
    }
  }
}

async function dispatch(req) {
  if (req.op === "ping") {
    send({ id: req.id, ok: true, node: process.version });
    return;
  }
  if (req.op === "models") {
    const models = await Cursor.models.list();
    send({ id: req.id, ok: true, models });
    return;
  }
  if (req.op === "tool_result") {
    const p = pendingToolResults.get(req.id);
    if (!p) {
      send({ id: req.id, ok: false, error: `cursor bridge: no pending tool ${req.id}` });
      return;
    }
    if (req.req && p.session) p.session.currentReqId = req.req;
    pendingToolResults.delete(req.id);
    p.resolve({
      content: req.content || [{ type: "text", text: "" }],
      isError: !!req.isError,
    });
    return;
  }
  if (req.op === "steer") {
    const s = sessions.get(req.agentId);
    if (!s || !s.run || typeof s.run.steer !== "function") {
      send({ id: req.id, ok: false, error: "no active run to steer" });
      return;
    }
    const outcome = await s.run.steer(req.text || "");
    send({ id: req.id, ok: true, outcome });
    return;
  }
  if (req.op === "reset") {
    await resetAgent(req.agentId);
    send({ id: req.id, ok: true });
    return;
  }
  if (req.op === "cancel") {
    const s = req.agentId ? sessions.get(req.agentId) : null;
    if (s) {
      s.needForce = true;
      if (s.run) {
        await s.run.cancel();
      }
    }
    send({ id: req.id, ok: true });
    return;
  }
  if (req.op === "prompt") {
    execPrompt(req).catch((e) => send({ id: req.id, event: "result", ...normalizeError(e) }));
    return;
  }
  send({ id: req.id, ok: false, error: `unknown op ${req.op}` });
}

const rl = createInterface({ input: process.stdin });
rl.on("line", (line) => {
  const trimmed = line.trim();
  if (!trimmed) return;
  let req;
  try {
    req = JSON.parse(trimmed);
  } catch {
    send({ ok: false, error: "malformed JSON line" });
    return;
  }
  dispatch(req).catch((e) => send({ id: req.id, ...normalizeError(e) }));
});
rl.on("close", () => {
  sawStdinEnd = true;
  for (const s of sessions.values()) {
    if (s.run) {
      s.run.cancel();
    }
  }
  if (inflight.size === 0) process.exit(0);
  const t = setTimeout(() => process.exit(0), 30_000);
  if (t.unref) t.unref();
});
