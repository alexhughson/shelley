# Shelley ↔ Cursor TypeScript SDK bridge

This package adapts [Cursor's TypeScript agent SDK](https://cursor.com/docs/sdk/typescript)
(`@cursor/sdk`) to Shelley's `llm.Service` interface, so a Cursor agent
(Composer, Router) can serve as the LLM backend of a Shelley conversation.

The Cursor SDK is not a model API — it's a complete agent harness (its own
tools, shell, system prompt, durable conversation store) running against
Cursor's hosted models. Shelley is also a complete agent harness. The bridge
therefore runs the Cursor agent "hollow":

- every Shelley tool is exposed to the Cursor agent as a **Cursor custom tool**
  (in-process MCP server `custom-user-tools`),
- Cursor's own built-in tools are removed (`tools: []` or `["mcp"]`),
- Shelley's system prompt replaces Cursor's (`systemPrompt`),
- the Cursor agent's durable conversation store accumulates a replayed
  transcript, so each Shelley `Do` sends only the new messages.

The Cursor agent then behaves as the model+policy layer: it decides what to
say and which Shelley tool to call; Shelley executes tools and owns the
user-visible conversation, persistence, and UI.

## Architecture

```
Shelley Go server
  └─ cursorbridge.Service (llm.Service)
       └─ daemon.mjs (Node child, JSON Lines over stdin/stdout)
            └─ @cursor/sdk Agent.create/send (local runtime)
                 └─ Cursor hosted models (composer-2.5, auto-smart, ...)
```

- `service.go` — the `llm.Service` implementation + lazy daemon lifecycle.
- `process.go` — child-process management, line multiplexing by request id.
- `translate.go` — request → prompt translation, tool execution, response
  synthesis, streaming.
- `history.go` — conversation history replay (`<replay>`/`<new_messages>`
  transcript rendering) and image extraction.
- `daemon/daemon.mjs` — the Node side: agent lifecycle, custom tool callbacks,
  deltas, usage, error normalization.
- `daemon/fake-daemon` (in `service_test.go`) — canned-protocol stand-in used
  by the Go tests so they run without a Cursor API key.

## Setup

1. Node ≥ 22.13 on PATH (on exe.dev VMs: `uvx nodeenv -n lts ~/node`).
2. Install the SDK once:

   ```
   cd cursorbridge/daemon && npm install
   ```

3. Set `CURSOR_API_KEY` (Cursor Dashboard → API Keys; user or service-account
   key) and start Shelley. Two models appear, gated on the key:

   - `cursor-composer-2.5` — Composer 2.5
   - `cursor-auto-smart` — Cursor Router (needs `optimize_for`; the daemon
     sends `balanced` — see TODO in daemon.mjs)

## Protocol (daemon.mjs)

One JSON object per line each way. Requests: `ping`, `models`, `prompt`,
`tool_result`, `cancel`. Responses/events: `delta` (text/thinking),
`tool_call` (daemon asks Shelley to run a tool; tagged with the owning request
id in `req`), and `result` (ok/final text/usage, or error with
`retryable`/`status`/`code`). See the header comment in `daemon.mjs`.

## Known limitations

- Reasoning levels are not mapped (Cursor picks its own); the service reports
  `SupportsReasoning() == false`.
- Each `Do` spins a fresh `Agent.create` + send against a durable JSONL store
  keyed by (model, cwd); the transcript replay in `history.go` gives the agent
  conversational continuity. Multi-turn tool loops are collapsed into the
  single run: Shelley-side tool results are delivered to the Cursor agent
  mid-run via the custom tool callbacks, and the final text becomes the
  assistant message.
- Tool progress reporting (partial output) is not bridged yet.
- `maxTurnDuration`/idle-stall semantics are approximated by the Go context
  deadline; the SDK's own retries (`enableAgentRetries`) handle transport
  hiccups.
