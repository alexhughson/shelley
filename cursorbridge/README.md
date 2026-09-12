# Shelley ↔ Cursor TypeScript SDK bridge

This package adapts [Cursor's TypeScript agent SDK](https://cursor.com/docs/sdk/typescript)
(`@cursor/sdk`) to Shelley's `llm.Service` interface.

Cursor runs a durable `Agent` (`create` / `resume` / `send` / `steer`).
The bridge translates Cursor events into native Shelley types and returns at
Shelley's usual loop boundaries:

- `onDelta` → `Request.OnStream`
- custom-tool `execute()` → `StopReasonToolUse` (the loop records the call, runs the tool, and calls `Do` again)
- `tool_result` from the next `Do` resolves the pending `execute()`
- extra user text after a tool result → `Run.steer`
- `run.wait()` text + usage → `llm.Response` / `StopReasonEndTurn`

One Cursor agent per Shelley conversation id (from `llmhttp.WithConversationID`).
Indirect calls (`PurposeFromContext` = `compaction`, `slug`, …) are a throwaway
agent. Successful compaction resets the conversation agent so the next turn
seeds from the compacted Shelley history.

## Architecture

```
loop.Do
  → cursorbridge.Service.Do
       → daemon.mjs  (long-lived Node child; not bound to the request ctx)
            → Agent.resume(id) ?? Agent.create({ agentId: id })
            → agent.send(new user text)   // or seed transcript on create
                 → onDelta → OnStream
                 → customTools.execute → tool_call event → StopReasonToolUse
                 → next Do sends tool_result → execute() resolves
            → result.text + usage → llm.Response
```

- `service.go` — `llm.Service` + daemon lifetime (process outlives each `Do`).
- `process.go` — JSONL multiplexing by request / agent id.
- `translate.go` — start / continue / oneshot, usage, `tool_use` yield.
- `history.go` — seed transcript for `create`; tail text for `resume`.
- `daemon/daemon.mjs` — Agent map, pending `execute()`, steer, reset.

## Setup

1. Node ≥ 22.13 on PATH (on exe.dev VMs: `uvx nodeenv -n lts ~/node`).
2. `cd cursorbridge/daemon && npm install`
3. Set `CURSOR_API_KEY`. Models:
   - `cursor-composer-2.5`
   - `cursor-grok-4.6-high-fast`
   - `cursor-auto-smart`

## Protocol

Ops: `ping`, `models`, `prompt`, `tool_result`, `steer`, `reset`, `cancel`.
Prompt events: `delta`, `tool_call` (execute is waiting), `result`.
