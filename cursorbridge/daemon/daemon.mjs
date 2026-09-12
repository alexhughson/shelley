#!/usr/bin/env node
/**
 * Shelley <-> Cursor SDK bridge daemon.
 *
 * JSONL over stdin/stdout. One Cursor Agent per Shelley conversation.
 * Tool calls stay pending until Shelley’s loop runs them and sends tool_result.
 *
 * Requests:
 *   {id, op:"ping"}
 *   {id, op:"models", apiKey?}
 *   {id, op:"prompt", agentId, oneshot?, resetAgentId?,
 *    apiKey, model, cwd, seed, message, images, tools}
 *   {id, op:"tool_result", req?, content, isError}
 *   {id, op:"steer", agentId, text}
 *   {id, op:"reset", agentId}
 *   {id, op:"cancel", agentId?}
 *
 * Prompt events (id = current request):
 *   {id, agent, event:"delta", kind:"text"|"thinking", text}
 *   {id, agent, event:"tool_call", name, args}     // execute() is waiting
 *   {id, agent, event:"result", ok, text?, usage?, created?, error?}
 */

import { createInterface } from "node:readline";
import { Agent, Cursor, JsonlLocalAgentStore } from "@cursor/sdk";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const pendingToolResults = new Map(); // toolCallId -> {resolve, session}
const sessions = new Map();           // agentId -> {agent, run, currentReqId, agentId}
const inflight = new Set();

let sawStdinEnd = false;

const storeRoot = path.join(os.homedir(), ".cache", "shelley-cursor-agents");
fs.mkdirSync(storeRoot, { recursive: true });
const store = new JsonlLocalAgentStore(storeRoot);

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
  try {
    process.stdout.write(JSON.stringify(obj) + "\n");
  } catch {
    // stdout gone
  }
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

function runToolOnShelley(session, toolCallId, name, args) {
  return new Promise((resolve) => {
    pendingToolResults.set(toolCallId, { resolve, session });
    emit(session, {
      id: toolCallId,
      req: session.currentReqId,
      event: "tool_call",
      name,
      args,
    });
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

function agentOptions(req, session) {
  const customTools = buildCustomTools(req.tools, session);
  return {
    apiKey: req.apiKey,
    model: modelSelection(req.model),
    ...(customTools ? { tools: ["mcp"] } : { tools: [] }),
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
      try { await s.run.cancel(); } catch {}
      s.run = null;
    }
    try { await s.agent[Symbol.asyncDispose](); } catch {}
    sessions.delete(agentId);
  }
  try { await Agent.delete(agentId); } catch {}
}

async function getOrCreateAgent(agentId, req) {
  const existing = sessions.get(agentId);
  if (existing) {
    existing.currentReqId = req.id;
    return { session: existing, created: false };
  }
  const session = {
    agent: null,
    run: null,
    currentReqId: req.id,
    agentId,
  };
  const opts = agentOptions(req, session);
  let created = false;
  try {
    session.agent = await Agent.resume(agentId, opts);
  } catch {
    session.agent = await Agent.create({ ...opts, agentId });
    created = true;
  }
  sessions.set(agentId, session);
  return { session, created };
}

function sendPayload(req, created) {
  const text = created ? (req.seed || req.message || "") : (req.message || "");
  return {
    text,
    ...(req.images && req.images.length ? { images: req.images } : {}),
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
  const session = {
    agent: null,
    run: null,
    currentReqId: req.id,
    agentId: req.agentId || `oneshot-${req.id}`,
  };
  const opts = agentOptions(req, session);
  const agent = await Agent.create(opts);
  session.agent = agent;
  try {
    const run = await agent.send(sendPayload(req, true), {
      onDelta: onDelta(session),
      ...(opts.local.customTools ? { customTools: opts.local.customTools } : {}),
    });
    await finishRun(session, run, true);
  } finally {
    try { await agent[Symbol.asyncDispose](); } catch {}
  }
}

async function execPrompt(req) {
  const id = req.id;
  inflight.add(id);
  try {
    if (req.resetAgentId) {
      await resetAgent(req.resetAgentId);
    }
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
      try { await session.run.cancel(); } catch {}
      session.run = null;
    }
    const customTools = buildCustomTools(req.tools, session);
    const run = await session.agent.send(sendPayload(req, created), {
      force: true,
      onDelta: onDelta(session),
      ...(customTools ? { customTools } : {}),
    });
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
    try {
      const models = await Cursor.models.list(req.apiKey ? { apiKey: req.apiKey } : {});
      send({ id: req.id, ok: true, models });
    } catch (e) {
      send({ id: req.id, ...normalizeError(e) });
    }
    return;
  }
  if (req.op === "tool_result") {
    const p = pendingToolResults.get(req.id);
    if (p) {
      if (req.req && p.session) p.session.currentReqId = req.req;
      pendingToolResults.delete(req.id);
      p.resolve({
        content: req.content || [{ type: "text", text: "" }],
        isError: !!req.isError,
      });
    }
    return;
  }
  if (req.op === "steer") {
    const s = sessions.get(req.agentId);
    if (s && s.run && typeof s.run.steer === "function") {
      try {
        const outcome = await s.run.steer(req.text || "");
        send({ id: req.id, ok: true, outcome });
      } catch (e) {
        send({ id: req.id, ...normalizeError(e) });
      }
    } else {
      send({ id: req.id, ok: false, error: "no active run to steer" });
    }
    return;
  }
  if (req.op === "reset") {
    await resetAgent(req.agentId);
    send({ id: req.id, ok: true });
    return;
  }
  if (req.op === "cancel") {
    const s = req.agentId ? sessions.get(req.agentId) : null;
    const run = s && s.run;
    if (run) {
      try { await run.cancel(); } catch {}
    }
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
      try { s.run.cancel(); } catch {}
    }
  }
  if (inflight.size === 0) process.exit(0);
  const t = setTimeout(() => process.exit(0), 30_000);
  if (t.unref) t.unref();
});
