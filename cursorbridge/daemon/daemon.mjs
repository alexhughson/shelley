#!/usr/bin/env node
/**
 * Shelley <-> Cursor SDK bridge daemon.
 *
 * Speaks JSON Lines over stdin/stdout (one JSON request per line, one JSON
 * response per line) and manages Cursor local agents via @cursor/sdk.
 *
 * Concept mapping (Shelley -> Cursor):
 *   llm.Request (full message list, tools, system)  -> Agent.create/send prompt
 *   llm.Content{Type: tool_use}                     -> SDKCustomTool.execute callback
 *   llm.Content{Type: tool_result}                  -> SDKCustomToolResult returned by execute
 *   llm.StreamDelta{text|thinking}                  -> onDelta text-delta / thinking-delta
 *   llm.Response (content, stop_reason, usage)      -> synthesized from final assistant SDKMessage
 *
 * Requests (from Shelley, one per line):
 *   {"id": "...", "op": "ping"}                                      -> {"id","ok":true,"node":"..."}
 *   {"id": "...", "op": "models"}                                    -> {"id","ok":true,"models":[{id,displayName,...}]}
 *   {"id": "...", "op": "prompt",
 *    "apiKey": "...", "model": {"id":"...","params":[...]},
 *    "cwd": "...", "message": "text...", "images": [{"data","mimeType"}...],
 *    "tools": [{"name","description","inputSchema"}...]}             -> streamed "events" lines then final result line
 *
 * Response lines for a prompt:
 *   {"id":..., "event":"delta", "kind":"text"|"thinking", "text":"..."}
 *   {"id":..., "event":"tool_call", "name":..., "status":"running"|"completed"|"error",
 *    "args": {...}, "result": "...", "isError": bool}
 *   {"id":..., "event":"result", "ok":true, "text":"...", "thinking":"...",
 *    "usage":{inputTokens,outputTokens,cacheReadTokens,cacheWriteTokens,reasoningTokens},
 *    "model":"..."}
 *   {"id":..., "event":"result", "ok":false, "error":"...", "retryable":bool,
 *    "status":401|429|..., "code":"..."}
 *
 * Errors are never thrown across the process boundary as stack traces; they are
 * always encoded as {"ok":false,...} lines tagged with the request id.
 *
 * Cancellation: when Shelley drops the request (its ctx dies), it sends
 *   {"id": "<requestId>", "op": "cancel"}
 * and we call run.cancel() on the in-flight Run. When Shelley's whole process
 * dies, stdin closes and we cancel everything and exit.
 *
 * Tool callbacks arrive as {"id": "<toolCallId>", "event":"tool_call", ...}
 * lines (the daemon asks Shelley to run a tool); Shelley replies with
 *   {"id": "<toolCallId>", "op":"tool_result", "content":[...], "isError":bool}
 * matched by id == the tool call_id. The daemon keeps a map of pending
 * tool-call promises and resolves them from stdin.
 */

import { createInterface } from "node:readline";
import { Agent, Cursor, JsonlLocalAgentStore } from "@cursor/sdk";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

// ---------- state ----------

const pendingToolResults = new Map(); // toolCallId -> {resolve, reject}
const activeRuns = new Map();         // requestId -> Run (for cancel)
const inflight = new Set();           // request ids currently executing

let sawStdinEnd = false;

// ---------- error normalization ----------

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

// Retryability heuristics for SDK errors that don't set isRetryable: network
// errors and rate limits are worth retrying; auth/config problems are not.
function isRetryableErr(e) {
  if (e && typeof e.isRetryable === "boolean") return e.isRetryable;
  const kind = e && e.constructor && e.constructor.name;
  return kind === "NetworkError" || kind === "RateLimitError";
}

function send(obj) {
  try {
    process.stdout.write(JSON.stringify(obj) + "\n");
  } catch {
    // stdout gone; nothing useful to do
  }
}

// ---------- tool plumbing ----------

/**
 * Ask Shelley to run one tool call. Returns a promise that resolves with
 *   { content: [{type:"text",text}...], isError: boolean }
 * Shelley replies on stdin with op=tool_result, id=toolCallId.
 */
function runToolOnShelley(toolCallId, name, args, reqId) {
  return new Promise((resolve) => {
    pendingToolResults.set(toolCallId, { resolve });
    send({
      id: toolCallId,
      req: reqId,
      event: "tool_call",
      name,
      status: "running",
      args,
    });
    // Safety net: if Shelley never answers (bug, crash mid-call), fail the
    // tool rather than hanging the agent forever. 10 minutes is generous for
    // long builds/tests a tool may run.
    const timeout = setTimeout(() => {
      if (pendingToolResults.has(toolCallId)) {
        pendingToolResults.delete(toolCallId);
        resolve({
          content: [{ type: "text", text: `Shelley bridge: tool result for ${name} never arrived (timeout).` }],
          isError: true,
        });
      }
    }, 10 * 60 * 1000);
    // Allow the process to exit even with a timer alive.
    if (timeout.unref) timeout.unref();
  });
}

function buildCustomTools(tools, reqId) {
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
        try {
          const result = await runToolOnShelley(toolCallId, t.name, args, reqId);
          send({
            id: toolCallId,
            event: "tool_call",
            name: t.name,
            status: result.isError ? "error" : "completed",
          });
          return result;
        } catch (e) {
          send({
            id: toolCallId,
            event: "tool_call",
            name: t.name,
            status: "error",
            result: String(e && e.message ? e.message : e),
          });
          throw e;
        }
      },
    };
  }
  return out;
}

// ---------- prompt execution ----------

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

function modelId(model) {
  return model && typeof model === "object" ? model.id : model;
}

function storeDirFor(model, cwd) {
  // One JSONL store directory per (model, cwd): agents must be durable across
  // sends so conversation state accumulates, but separate conversations must
  // not share an agent.
  const safe = String(model || "model").replace(/[^a-zA-Z0-9._-]+/g, "_");
  let cwdPart = "nocwd";
  if (cwd) {
    cwdPart = cwd.split(path.sep).filter(Boolean).join("_").slice(-80) || "nocwd";
    cwdPart = cwdPart.replace(/[^a-zA-Z0-9._-]+/g, "_");
  }
  const root = path.join(os.homedir(), ".cache", "shelley-cursor-agents", safe, cwdPart);
  fs.mkdirSync(root, { recursive: true });
  return root;
}

async function execPrompt(req) {
  const id = req.id;
  inflight.add(id);
  const store = new JsonlLocalAgentStore(storeDirFor(modelId(req.model), req.cwd));
  const customTools = buildCustomTools(req.tools, req.id);
  try {
    const agent = await Agent.create({
      apiKey: req.apiKey,
      model: req.model && typeof req.model === "object" ? req.model : { id: req.model },
      systemPrompt: undefined, // never used: gated per Cursor account; the prompt carries system text inline
      ...(customTools ? { tools: ["mcp"] } : { tools: [] }),
      local: {
        ...(req.cwd ? { cwd: req.cwd } : {}),
        store,
        ...(customTools ? { customTools } : {}),
      },
    });
    try {
      const sendMsg = {
        text: req.message,
        ...(req.images && req.images.length ? { images: req.images } : {}),
      };
      const run = await agent.send(sendMsg, {
        onDelta: ({ update }) => {
          if (update.type === "text-delta") {
            send({ id, event: "delta", kind: "text", text: update.text });
          } else if (update.type === "thinking-delta") {
            send({ id, event: "delta", kind: "thinking", text: update.text });
          }
        },
      });
      activeRuns.set(id, run);
      try {
        const result = await run.wait();
        if (result.status === "error") {
          const err = normalizeError(result.error || { message: "run failed" });
          send({ id, event: "result", ...err });
        } else if (result.status === "cancelled") {
          send({ id, event: "result", ok: false, error: "run cancelled", cancelled: true });
        } else {
          send({
            id, event: "result", ok: true,
            text: result.result ?? "",
            usage: tokenUsageToBridge(result.usage),
            model: result.model && result.model.id,
          });
        }
      } finally {
        activeRuns.delete(id);
      }
    } finally {
      try { await agent[Symbol.asyncDispose](); } catch {}
    }
  } catch (e) {
    if (activeRuns.has(id)) activeRuns.delete(id);
    const err = normalizeError(e);
    if (err.retryable === undefined) err.retryable = isRetryableErr(e);
    send({ id, event: "result", ...err });
  } finally {
    inflight.delete(id);
    // Shelley is gone (its process died or it hung up): exit promptly.
    if (sawStdinEnd && inflight.size === 0) {
      process.exit(0);
    }
  }
}

// ---------- request dispatch ----------

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
      pendingToolResults.delete(req.id);
      p.resolve({
        content: req.content || [{ type: "text", text: "" }],
        isError: !!req.isError,
      });
    }
    return; // no ack needed; Shelley fire-and-forgets
  }
  if (req.op === "cancel") {
    const run = activeRuns.get(req.id);
    if (run) {
      try { await run.cancel(); } catch {}
    }
    return;
  }
  if (req.op === "prompt") {
    // fire-and-forget; responses arrive as event lines
    execPrompt(req).catch((e) => send({ id: req.id, event: "result", ...normalizeError(e) }));
    return;
  }
  send({ id: req.id, ok: false, error: `unknown op ${req.op}` });
}

// ---------- main loop ----------

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
  // Shelley hung up. Cancel what we can and exit when quiet.
  for (const run of activeRuns.values()) {
    try { run.cancel(); } catch {}
  }
  if (inflight.size === 0) process.exit(0);
  // execPrompt's finally checks sawStdinEnd and exits after the last one.
  const t = setTimeout(() => process.exit(0), 30_000);
  if (t.unref) t.unref();
});
