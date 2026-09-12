# Shelley ↔ Cursor TypeScript SDK bridge

This package adapts [Cursor's TypeScript agent SDK](https://cursor.com/docs/sdk/typescript)
(`@cursor/sdk`) to Shelley's `llm.Service` interface.

Cursor runs a durable `Agent` (`create` / `resume` / `send` / `steer`).
The bridge translates Cursor events into native Shelley types and returns at
Shelley's usual loop boundaries:

- `onDelta` → `Request.OnStream`
- custom-tool `execute()` batch → `StopReasonToolUse` (the loop records the
  calls, runs the tools, and calls `Do` again)
- `tool_result` from the next `Do` resolves the pending `execute()`
- extra user text after a tool result → `Run.steer` (errors fail the `Do`)
- `run.wait()` text + usage → `llm.Response` / `StopReasonEndTurn`

One Cursor agent per Shelley conversation id **and** model id
(`{conversationID}:{modelID}`). A mid-conversation model switch creates a new
agent and seeds it from Shelley history.

`Agent.resume` failure is an error unless the SDK reports `AgentNotFoundError`.
Indirect calls (`PurposeFromContext` = `compaction`, `slug`, …) are a throwaway
agent. Successful compaction **must** reset the conversation agent; a reset
failure fails the compaction `Do`.

The event channel for a conversation stays registered until the Cursor run
ends. Deltas and leftover `tool_calls` that arrive while Shelley runs tools
stay on that channel for the next `Do`.

Cursor `inputTokens` is the full prompt. `cacheRead` / `cacheWrite` are a
breakdown of that prompt. The bridge subtracts them so Shelley's
`TotalInputTokens()` does not count the cached span twice.

## Architecture

```
loop.Do
  → cursorbridge.Service.Do
       → daemon.mjs  (long-lived Node child; not bound to the request ctx)
            → Agent.resume(id) or Agent.create({ agentId: id }) if not found
            → agent.send(new user text)   // or seed transcript on create
                 → onDelta → OnStream
                 → customTools.execute → tool_calls event → StopReasonToolUse
                 → next Do sends tool_result → execute() resolves
            → result.text + usage → llm.Response
```

- `service.go` — `llm.Service` + daemon lifetime (process outlives each `Do`).
- `process.go` — JSONL multiplexing by request / agent id.
- `translate.go` — start / continue / oneshot, usage, `tool_use` yield.
- `history.go` — seed transcript for `create`; tail text for `resume`.
- `daemon/daemon.mjs` — Agent map, pending `execute()`, steer, reset.

The daemon script is resolved from this package directory via `runtime.Caller`.
`npm install` in `cursorbridge/daemon` must have been run on this machine.

## Setup

1. Node ≥ 22.13 on PATH (on exe.dev VMs: `uvx nodeenv -n lts ~/node`).
2. `cd cursorbridge/daemon && npm install`
3. Set `CURSOR_API_KEY`. Models:
   - `cursor-composer-2.5`
   - `cursor-grok-4.6-high-fast`
   - `cursor-auto-smart`

## Protocol

Ops: `ping`, `models`, `prompt`, `tool_result`, `steer`, `reset`, `cancel`.
Prompt events: `delta`, `tool_calls` (one `setImmediate` burst of `execute()`),
`result`.
The daemon enables only the `mcp` capability group and passes `mcpServers: []`,
so only Shelley custom tools are offered.
