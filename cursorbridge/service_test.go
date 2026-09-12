package cursorbridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/llmhttp"
	"shelley.exe.dev/loop"
)

// fakeDaemon implements the daemon protocol without the Cursor SDK.
const fakeDaemon = `#!/usr/bin/env node
const readline = require("node:readline");
const rl = readline.createInterface({ input: process.stdin });
const pending = new Map(); // toolCallId -> {resolve, session}
const agents = new Set();
const sessions = new Map(); // agentId -> {currentReqId, cancelled}
const send = (o) => process.stdout.write(JSON.stringify(o) + "\n");

function emit(session, obj) {
  send({ id: session.currentReqId, agent: session.agentId, ...obj });
}

rl.on("line", (line) => {
  const s = line.trim();
  if (!s) return;
  let req; try { req = JSON.parse(s); } catch { return; }
  if (req.op === "ping") { send({ id: req.id, ok: true, node: "fake" }); return; }
  if (req.op === "models") { send({ id: req.id, ok: true, models: [{ id: "fake-cursor", displayName: "Fake Cursor" }] }); return; }
  if (req.op === "tool_result") {
    const p = pending.get(req.id);
    if (p) {
      if (req.req && p.session) p.session.currentReqId = req.req;
      pending.delete(req.id);
      p.resolve(req);
    }
    return;
  }
  if (req.op === "steer") {
    if ((req.text || "").includes("STEER_FAIL")) { send({ id: req.id, ok: false, error: "steer refused" }); return; }
    send({ id: req.id, ok: true, outcome: "complete_delivered" }); return;
  }
  if (req.op === "reset") {
    if ((req.agentId || "").includes("resetfail")) { send({ id: req.id, ok: false, error: "reset refused" }); return; }
    agents.delete(req.agentId); sessions.delete(req.agentId); send({ id: req.id, ok: true }); return;
  }
  if (req.op === "cancel") {
    const sess = sessions.get(req.agentId);
    if (sess) sess.cancelled = true;
    const waiter = pending.get("__slow__" + (req.agentId || ""));
    if (waiter) { pending.delete("__slow__" + req.agentId); waiter.resolve({ cancelled: true }); }
    return;
  }
  if (req.op === "prompt") {
    (async () => {
      const id = req.id;
      if (req.oneshot) {
        const text = "SUMMARY: " + (req.message || req.seed || "").slice(0, 40);
        send({ id, event: "result", ok: true, text, usage: { inputTokens: 8, outputTokens: 3, cacheReadTokens: 0, cacheWriteTokens: 0, reasoningTokens: 0 }, created: true });
        return;
      }
      const agentId = req.agentId;
      const created = !agents.has(agentId);
      agents.add(agentId);
      const session = { agentId, currentReqId: id, cancelled: false };
      sessions.set(agentId, session);
      const text = created ? (req.seed || req.message || "") : (req.message || "");
      if (text.includes("SLOW")) {
        emit(session, { event: "delta", kind: "text", text: "SLOW_STARTED" });
        await new Promise((resolve) => pending.set("__slow__" + agentId, { resolve }));
        emit(session, { event: "result", ok: false, error: "run cancelled", cancelled: true });
        return;
      }
      emit(session, { event: "delta", kind: "text", text: "Hello " });
      if ((req.message || "").includes("BATCH")) {
        const t1 = req.tools[0];
        const t2 = req.tools[1];
        emit(session, { event: "tool_calls", calls: [
          { id: "call-batch-a", name: t1.name, args: { n: "a" } },
          { id: "call-batch-b", name: t2.name, args: { n: "b" } },
        ] });
        return;
      }
      if ((req.message || "").includes("LATE")) {
        // Two tool_calls events. collect yields the first; the second stays
        // on the persistent agent channel and must be re-yielded next Do.
        const t1 = req.tools[0];
        const t2 = req.tools[1];
        emit(session, { event: "tool_calls", calls: [{ id: "call-late-a", name: t1.name, args: { n: "a" } }] });
        emit(session, { event: "tool_calls", calls: [{ id: "call-late-b", name: t2.name, args: { n: "b" } }] });
        return;
      }
      emit(session, { event: "delta", kind: "text", text: "from bridge." });
      if (req.tools && req.tools.length > 0) {
        const t = req.tools[0];
        const callId = "call-1";
        const result = await new Promise((resolve) => {
          pending.set(callId, { resolve, session });
          emit(session, { event: "tool_calls", calls: [{ id: callId, name: t.name, args: { value: 42 } }] });
        });
        emit(session, { event: "delta", kind: "text", text: " Tool ran: " + JSON.stringify(result.content) });
      }
      if (session.cancelled) {
        emit(session, { event: "result", ok: false, error: "run cancelled", cancelled: true });
        return;
      }
      const reasoning = text.includes("UNDERFLOW") ? 10 : 1;
      emit(session, {
        event: "result", ok: true, text: "FINAL TEXT", created,
        usage: { inputTokens: 10, outputTokens: 5, cacheReadTokens: 2, cacheWriteTokens: 1, reasoningTokens: reasoning, totalTokens: 18 },
      });
    })();
    return;
  }
});
rl.on("close", () => process.exit(0));
`

func writeFakeDaemon(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/fake-daemon.cjs"
	if err := os.WriteFile(path, []byte(fakeDaemon), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testService(t *testing.T) *Service {
	t.Helper()
	return &Service{
		APIKey:       "fake-key",
		ModelID:      "fake-cursor",
		DisplayName:  "fake-cursor",
		DaemonScript: writeFakeDaemon(t),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func convCtx(id string) context.Context {
	return llmhttp.WithConversationID(context.Background(), id)
}

func TestDoSimplePrompt(t *testing.T) {
	svc := testService(t)
	req := &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("hello there")},
	}
	resp, err := svc.Do(convCtx("c-simple"), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := llm.FirstText(resp); got != "Hello from bridge." {
		t.Errorf("text = %q, want Hello from bridge.", got)
	}
	if resp.StopReason != llm.StopReasonEndTurn {
		t.Errorf("stop reason = %v", resp.StopReason)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 5 || resp.Usage.CacheReadInputTokens != 2 || resp.Usage.CacheCreationInputTokens != 1 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Usage.TotalInputTokens() != 10 {
		t.Errorf("total input = %d, want 10 (cache counted once)", resp.Usage.TotalInputTokens())
	}
	if resp.StartTime == nil || resp.EndTime == nil || !resp.EndTime.After(*resp.StartTime) && resp.EndTime.Equal(*resp.StartTime) {
		// zero duration is possible on a fast fake; just require both set
		if resp.StartTime == nil || resp.EndTime == nil {
			t.Errorf("missing timestamps start=%v end=%v", resp.StartTime, resp.EndTime)
		}
	}
	if svc.Provider() != "cursor" {
		t.Errorf("provider = %q", svc.Provider())
	}
}

func TestDoStreaming(t *testing.T) {
	svc := testService(t)
	var mu sync.Mutex
	var got strings.Builder
	req := &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("hello")},
		OnStream: func(d llm.StreamDelta) {
			mu.Lock()
			got.WriteString(d.Text)
			mu.Unlock()
		},
	}
	resp, err := svc.Do(convCtx("c-stream"), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp
	if got.String() != "Hello from bridge." {
		t.Errorf("streamed text = %q, want %q", got.String(), "Hello from bridge.")
	}
}

func TestDoToolUseThenResult(t *testing.T) {
	svc := testService(t)
	ctx := convCtx("c-tool")
	var ran atomic.Bool
	tool := &llm.Tool{
		Name:        "my_tool",
		Description: "A test tool",
		InputSchema: llm.EmptySchema(),
		Run: func(ctx context.Context, input json.RawMessage) llm.ToolOut {
			ran.Store(true)
			return llm.ToolOut{LLMContent: llm.TextContent("tool says hi")}
		},
	}
	resp, err := svc.Do(ctx, &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("use the tool")},
		Tools:    []*llm.Tool{tool},
	})
	if err != nil {
		t.Fatalf("Do #1: %v", err)
	}
	if ran.Load() {
		t.Fatal("bridge must not run the tool; the loop does")
	}
	if resp.StopReason != llm.StopReasonToolUse {
		t.Fatalf("stop reason = %v, want tool_use", resp.StopReason)
	}
	var call llm.Content
	for _, c := range resp.Content {
		if c.Type == llm.ContentTypeToolUse {
			call = c
		}
	}
	if call.ID == "" || call.ToolName != "my_tool" {
		t.Fatalf("tool_use = %+v", call)
	}

	out := tool.Run(ctx, call.ToolInput)
	history := []llm.Message{
		llm.UserStringMessage("use the tool"),
		resp.ToMessage(),
		{Role: llm.MessageRoleUser, Content: []llm.Content{{
			Type:       llm.ContentTypeToolResult,
			ToolUseID:  call.ID,
			ToolResult: out.LLMContent,
		}}},
	}
	resp2, err := svc.Do(ctx, &llm.Request{Messages: history, Tools: []*llm.Tool{tool}})
	if err != nil {
		t.Fatalf("Do #2: %v", err)
	}
	if resp2.StopReason != llm.StopReasonEndTurn {
		t.Errorf("stop reason #2 = %v", resp2.StopReason)
	}
	if !strings.Contains(llm.FirstText(resp2), "tool says hi") {
		t.Errorf("final text should include tool result, got %q", llm.FirstText(resp2))
	}
}

func TestUsageDoesNotUnderflow(t *testing.T) {
	svc := testService(t)
	resp, err := svc.Do(convCtx("c-under"), &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("UNDERFLOW please")},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Errorf("output tokens = %d, want 5 (must not subtract reasoning)", resp.Usage.OutputTokens)
	}
}

func cancelWhenStarted(t *testing.T, cancel context.CancelFunc) func(llm.StreamDelta) {
	t.Helper()
	started := make(chan struct{})
	go func() {
		select {
		case <-started:
			cancel()
		case <-t.Context().Done():
		}
	}()
	return func(d llm.StreamDelta) {
		if !strings.Contains(d.Text, "SLOW_STARTED") {
			return
		}
		select {
		case <-started:
		default:
			close(started)
		}
	}
}

func TestDoCancellation(t *testing.T) {
	svc := testService(t)
	ctx, cancel := context.WithCancel(convCtx("c-cancel"))
	_, err := svc.Do(ctx, &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("SLOW please")},
		OnStream: cancelWhenStarted(t, cancel),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDaemonSurvivesRequestCancel(t *testing.T) {
	svc := testService(t)
	ctx1, cancel := context.WithCancel(convCtx("c-die"))
	_, err := svc.Do(ctx1, &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("SLOW")},
		OnStream: cancelWhenStarted(t, cancel),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Do: %v", err)
	}
	resp, err := svc.Do(convCtx("c-live"), &llm.Request{Messages: []llm.Message{llm.UserStringMessage("hello")}})
	if err != nil {
		t.Fatalf("second Do after cancel: %v", err)
	}
	if llm.FirstText(resp) != "Hello from bridge." {
		t.Errorf("second Do text = %q", llm.FirstText(resp))
	}
}

func TestTwoConversationsIsolated(t *testing.T) {
	svc := testService(t)
	tool := &llm.Tool{
		Name:        "my_tool",
		Description: "A test tool",
		InputSchema: llm.EmptySchema(),
		Run: func(ctx context.Context, input json.RawMessage) llm.ToolOut {
			return llm.ToolOut{LLMContent: llm.TextContent("ok")}
		},
	}
	resp1, err := svc.Do(convCtx("c-a"), &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("use the tool")},
		Tools:    []*llm.Tool{tool},
	})
	if err != nil {
		t.Fatalf("conv a: %v", err)
	}
	if resp1.StopReason != llm.StopReasonToolUse {
		t.Fatalf("conv a stop = %v", resp1.StopReason)
	}
	resp2, err := svc.Do(convCtx("c-b"), &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("hello")},
	})
	if err != nil {
		t.Fatalf("conv b: %v", err)
	}
	if resp2.StopReason != llm.StopReasonEndTurn {
		t.Errorf("conv b stop = %v", resp2.StopReason)
	}
	if llm.FirstText(resp2) != "Hello from bridge." {
		t.Errorf("conv b text = %q", llm.FirstText(resp2))
	}
}

func TestCompactIsOneshotThenResets(t *testing.T) {
	svc := testService(t)
	// Establish a conversation agent first.
	if _, err := svc.Do(convCtx("c-compact"), &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("hello")},
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx := llm.WithPurpose(convCtx("c-compact"), "compaction")
	resp, err := svc.Do(ctx, &llm.Request{
		System:   []llm.SystemContent{{Text: "You are a context summarization assistant."}},
		Messages: []llm.Message{llm.UserStringMessage("The messages above are a conversation to summarize.")},
	})
	if err != nil {
		t.Fatalf("compact Do: %v", err)
	}
	if !strings.HasPrefix(llm.FirstText(resp), "SUMMARY:") {
		t.Errorf("compact text = %q, want SUMMARY: prefix", llm.FirstText(resp))
	}
	// Next chat turn still works after reset.
	resp2, err := svc.Do(convCtx("c-compact"), &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("continue after compact")},
	})
	if err != nil {
		t.Fatalf("post-compact Do: %v", err)
	}
	if llm.FirstText(resp2) != "Hello from bridge." {
		t.Errorf("post-compact text = %q", llm.FirstText(resp2))
	}
}

func TestBridgeErrorRetryable(t *testing.T) {
	e := &bridgeError{msg: "boom", retryable: true}
	info, ok := llm.RequestErrorInfoFromError(e)
	if !ok || !info.Retryable {
		t.Errorf("retryable classification failed: %+v ok=%v", info, ok)
	}
}

func TestSeedPromptNoReplayWrapper(t *testing.T) {
	req := &llm.Request{
		System: []llm.SystemContent{{Text: "You are Shelley."}},
		Messages: []llm.Message{
			llm.UserStringMessage("first question"),
			{
				Role: llm.MessageRoleAssistant,
				Content: []llm.Content{
					{Type: llm.ContentTypeText, Text: "first answer"},
				},
			},
			{
				Role: llm.MessageRoleUser,
				Content: []llm.Content{
					{Type: llm.ContentTypeToolResult, ToolUseID: "t1", ToolResult: llm.TextContent("tool output")},
				},
			},
		},
	}
	seed := seedPrompt(req)
	if strings.Contains(seed, "<replay>") || strings.Contains(seed, "<new_messages>") {
		t.Errorf("seed still uses replay wrappers: %s", seed)
	}
	if !strings.Contains(seed, "first question") || !strings.Contains(seed, "You are Shelley.") {
		t.Errorf("seed missing history: %s", seed)
	}
	text, _ := newUserPrompt(req)
	if strings.Contains(text, "first question") {
		t.Errorf("new user text should be the tail only, got %q", text)
	}
	if !strings.Contains(seed, "tool output") {
		t.Error("seed missing tool result")
	}
}

func TestLoopRecordsToolUse(t *testing.T) {
	svc := testService(t)
	var recorded []llm.Message
	toolRan := false
	l := loop.NewLoop(loop.Config{
		LLM: svc,
		Tools: []*llm.Tool{{
			Name:        "my_tool",
			Description: "A test tool",
			InputSchema: llm.EmptySchema(),
			Run: func(ctx context.Context, input json.RawMessage) llm.ToolOut {
				toolRan = true
				return llm.ToolOut{LLMContent: llm.TextContent("tool says hi")}
			},
		}},
		RecordMessage: func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
			recorded = append(recorded, message)
			return nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	l.QueueUserMessage(llm.UserStringMessage("use the tool"))
	if err := l.ProcessOneTurn(convCtx("c-loop")); err != nil {
		t.Fatal(err)
	}
	if !toolRan {
		t.Fatal("loop did not run the tool")
	}
	var sawUse, sawResult bool
	for _, m := range recorded {
		for _, c := range m.Content {
			if c.Type == llm.ContentTypeToolUse && c.ToolName == "my_tool" {
				sawUse = true
			}
			if c.Type == llm.ContentTypeToolResult && c.ToolUseID != "" {
				sawResult = true
			}
		}
	}
	if !sawUse || !sawResult {
		t.Fatalf("loop did not record tool_use/tool_result (use=%v result=%v, n=%d)", sawUse, sawResult, len(recorded))
	}
}

func TestLateToolCallNotDropped(t *testing.T) {
	svc := testService(t)
	mk := func(name string) *llm.Tool {
		return &llm.Tool{
			Name:        name,
			Description: "test tool " + name,
			InputSchema: llm.EmptySchema(),
			Run: func(ctx context.Context, input json.RawMessage) llm.ToolOut {
				return llm.ToolOut{LLMContent: llm.TextContent(name + " ran")}
			},
		}
	}
	tools := []*llm.Tool{mk("tool_a"), mk("tool_b")}
	ctx := convCtx("c-late")

	// First round: fake daemon emits two tool_calls events. collect yields
	// the first; the second stays on the persistent agent channel.
	resp, err := svc.Do(ctx, &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("LATE test")},
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("Do #1: %v", err)
	}
	if resp.StopReason != llm.StopReasonToolUse {
		t.Fatalf("stop reason #1 = %v", resp.StopReason)
	}
	ids := map[string]bool{}
	for _, c := range resp.Content {
		if c.Type == llm.ContentTypeToolUse {
			ids[c.ID] = true
		}
	}
	if !ids["call-late-a"] {
		t.Fatalf("first batch missing tool_a: %+v", resp.Content)
	}

	// Run tool_a, then continue: the deferred tool_b must be re-yielded as a
	// new tool_use batch, not lost.
	history := []llm.Message{
		llm.UserStringMessage("LATE test"),
		resp.ToMessage(),
		{Role: llm.MessageRoleUser, Content: []llm.Content{{
			Type:       llm.ContentTypeToolResult,
			ToolUseID:  "call-late-a",
			ToolResult: llm.TextContent("tool_a ran"),
		}}},
	}
	resp2, err := svc.Do(ctx, &llm.Request{Messages: history, Tools: tools})
	if err != nil {
		t.Fatalf("Do #2: %v", err)
	}
	sawB := false
	for _, c := range resp2.Content {
		if c.Type == llm.ContentTypeToolUse && c.ID == "call-late-b" {
			sawB = true
		}
	}
	if !sawB {
		t.Fatalf("deferred tool_b was dropped; content: %+v", resp2.Content)
	}
}

func TestUsageFromBridgeDoesNotRepeatCache(t *testing.T) {
	u := usageFromBridge(&BridgeUsage{InputTokens: 34363, CacheReadTokens: 22592, OutputTokens: 270})
	if u.InputTokens != 34363-22592 {
		t.Fatalf("uncached input = %d, want %d", u.InputTokens, 34363-22592)
	}
	if u.CacheReadInputTokens != 22592 {
		t.Fatalf("cache read = %d", u.CacheReadInputTokens)
	}
	if u.TotalInputTokens() != 34363 {
		t.Fatalf("TotalInputTokens = %d, want 34363 (cache counted once)", u.TotalInputTokens())
	}
	if u.ContextWindowUsed() != 34363+270 {
		t.Fatalf("ContextWindowUsed = %d, want %d", u.ContextWindowUsed(), 34363+270)
	}
}

func TestUsageFromBridgeKeepsSeparateWhenInputSmaller(t *testing.T) {
	u := usageFromBridge(&BridgeUsage{InputTokens: 50, CacheReadTokens: 80, OutputTokens: 5})
	if u.InputTokens != 50 {
		t.Fatalf("input = %d, want 50 (do not subtract when input < cache)", u.InputTokens)
	}
	if u.TotalInputTokens() != 130 {
		t.Fatalf("TotalInputTokens = %d, want 130", u.TotalInputTokens())
	}
}

func TestToolCallsBatchYieldsTogether(t *testing.T) {
	svc := testService(t)
	mk := func(name string) *llm.Tool {
		return &llm.Tool{
			Name: name, Description: name, InputSchema: llm.EmptySchema(),
			Run: func(ctx context.Context, input json.RawMessage) llm.ToolOut {
				return llm.ToolOut{LLMContent: llm.TextContent("ok")}
			},
		}
	}
	resp, err := svc.Do(convCtx("c-batch"), &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("BATCH please")},
		Tools:    []*llm.Tool{mk("tool_a"), mk("tool_b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range resp.Content {
		if c.Type == llm.ContentTypeToolUse {
			ids[c.ID] = true
		}
	}
	if !ids["call-batch-a"] || !ids["call-batch-b"] {
		t.Fatalf("batch missing calls: %+v", resp.Content)
	}
}

func TestSteerErrorSurfaces(t *testing.T) {
	svc := testService(t)
	ctx := convCtx("c-steer")
	tool := &llm.Tool{
		Name: "my_tool", Description: "t", InputSchema: llm.EmptySchema(),
		Run: func(ctx context.Context, input json.RawMessage) llm.ToolOut {
			return llm.ToolOut{LLMContent: llm.TextContent("ok")}
		},
	}
	resp, err := svc.Do(ctx, &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("use the tool")},
		Tools:    []*llm.Tool{tool},
	})
	if err != nil {
		t.Fatal(err)
	}
	var callID string
	for _, c := range resp.Content {
		if c.Type == llm.ContentTypeToolUse {
			callID = c.ID
		}
	}
	history := []llm.Message{
		llm.UserStringMessage("use the tool"),
		resp.ToMessage(),
		{Role: llm.MessageRoleUser, Content: []llm.Content{
			{Type: llm.ContentTypeToolResult, ToolUseID: callID, ToolResult: llm.TextContent("ok")},
			{Type: llm.ContentTypeText, Text: "STEER_FAIL please"},
		}},
	}
	_, err = svc.Do(ctx, &llm.Request{Messages: history, Tools: []*llm.Tool{tool}})
	if err == nil || !strings.Contains(err.Error(), "steer refused") {
		t.Fatalf("want steer error, got %v", err)
	}
}

func TestCompactResetFailure(t *testing.T) {
	svc := testService(t)
	if _, err := svc.Do(convCtx("c-resetfail"), &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("hello")},
	}); err != nil {
		t.Fatal(err)
	}
	ctx := llm.WithPurpose(convCtx("c-resetfail"), "compaction")
	_, err := svc.Do(ctx, &llm.Request{
		Messages: []llm.Message{llm.UserStringMessage("summarize")},
	})
	if err == nil || !strings.Contains(err.Error(), "reset refused") {
		t.Fatalf("want reset error, got %v", err)
	}
}
