package cursorbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/llmhttp"
)

// ModelParam is one Cursor model parameter (e.g. {ID: "effort", Value: "high"}).
type ModelParam struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

func modelSelection(svc *Service) map[string]any {
	m := map[string]any{"id": svc.ModelID}
	if len(svc.ModelParams) > 0 {
		params := make([]map[string]string, len(svc.ModelParams))
		for i, p := range svc.ModelParams {
			params[i] = map[string]string{"id": p.ID, "value": p.Value}
		}
		m["params"] = params
	}
	return m
}

type bridgeError struct {
	msg       string
	retryable bool
}

func (e *bridgeError) Error() string { return e.msg }

func (e *bridgeError) RequestErrorInfo() llm.RequestErrorInfo {
	return llm.RequestErrorInfo{Retryable: e.retryable}
}

type pendingTool struct {
	id   string
	name string
	args json.RawMessage
}

type liveSession struct {
	agentID string
	pending []pendingTool
}

func conversationKey(ctx context.Context) string {
	return llmhttp.ConversationIDFromContext(ctx)
}

func agentIDFor(ctx context.Context) string {
	if id := conversationKey(ctx); id != "" {
		return id
	}
	return newRequestID()
}

func (p *daemonProcess) sessionOf(key string) *liveSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[key]
}

func (p *daemonProcess) putSession(key string, st *liveSession) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = make(map[string]*liveSession)
	}
	p.sessions[key] = st
}

func (p *daemonProcess) clearSession(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, key)
}

// do runs one Shelley LLM round. Cursor owns the durable agent; this returns
// at the next native loop boundary: tool_use (loop runs the tool) or end_turn.
func (p *daemonProcess) do(ctx context.Context, svc *Service, req *llm.Request) (*llm.Response, error) {
	purpose := llm.PurposeFromContext(ctx)
	if purpose != "" {
		return p.oneshot(ctx, svc, req, purpose)
	}
	key := conversationKey(ctx)
	if key == "" {
		key = agentIDFor(ctx)
	}
	if st := p.sessionOf(key); st != nil && len(st.pending) > 0 {
		return p.continueTurn(ctx, svc, req, key, st)
	}
	return p.startTurn(ctx, svc, req, key)
}

func (p *daemonProcess) oneshot(ctx context.Context, svc *Service, req *llm.Request, purpose string) (*llm.Response, error) {
	reqID := newRequestID()
	events := make(chan *daemonLine, 256)
	p.mu.Lock()
	p.reqs[reqID] = events
	p.mu.Unlock()
	defer p.dropReq(reqID)

	payload := map[string]any{
		"id":      reqID,
		"op":      "prompt",
		"oneshot": true,
		"apiKey":  svc.APIKey,
		"model":   modelSelection(svc),
		"cwd":     workingDirFor(ctx),
		"seed":    seedPrompt(req),
		"message": oneshotMessage(req),
		"tools":   toolDescriptors(req),
	}
	if err := p.send(payload); err != nil {
		return nil, &bridgeError{msg: err.Error(), retryable: true}
	}
	resp, err := p.collect(ctx, svc, req, reqID, events, "", nil, nil)
	if err == nil && purpose == "compaction" {
		if id := conversationKey(ctx); id != "" {
			p.resetAgent(id)
		}
	}
	return resp, err
}

func (p *daemonProcess) resetAgent(agentID string) {
	reqID := newRequestID()
	ch := make(chan *daemonLine, 4)
	p.mu.Lock()
	p.reqs[reqID] = ch
	p.mu.Unlock()
	defer p.dropReq(reqID)
	if err := p.send(map[string]any{"id": reqID, "op": "reset", "agentId": agentID}); err != nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
	}
	p.clearSession(agentID)
}

func oneshotMessage(req *llm.Request) string {
	text, _ := newUserPrompt(req)
	if text != "" {
		return text
	}
	return seedPrompt(req)
}

func (p *daemonProcess) startTurn(ctx context.Context, svc *Service, req *llm.Request, key string) (*llm.Response, error) {
	reqID := newRequestID()
	events := make(chan *daemonLine, 256)
	agentID := key
	p.mu.Lock()
	p.reqs[reqID] = events
	p.reqs[agentID] = events
	p.mu.Unlock()
	defer func() {
		p.dropReq(reqID)
		p.dropReq(agentID)
	}()

	message, images := newUserPrompt(req)
	payload := map[string]any{
		"id":      reqID,
		"op":      "prompt",
		"agentId": agentID,
		"apiKey":  svc.APIKey,
		"model":   modelSelection(svc),
		"cwd":     workingDirFor(ctx),
		"seed":    seedPrompt(req),
		"message": message,
		"tools":   toolDescriptors(req),
	}
	if len(images) > 0 {
		payload["images"] = images
	}
	if err := p.send(payload); err != nil {
		return nil, &bridgeError{msg: err.Error(), retryable: true}
	}
	st := &liveSession{agentID: agentID}
	return p.collect(ctx, svc, req, reqID, events, key, st, nil)
}

func (p *daemonProcess) continueTurn(ctx context.Context, svc *Service, req *llm.Request, key string, st *liveSession) (*llm.Response, error) {
	// Tool calls that missed the previous batch window were stashed by
	// routeReq. Re-yield them as tool_use blocks so Shelley executes them
	// normally this round instead of wedging the Cursor run on a result
	// that would never arrive.
	late := p.takeLateTools(st.agentID)
	reqID := newRequestID()
	events := make(chan *daemonLine, 256)
	p.mu.Lock()
	p.reqs[reqID] = events
	p.reqs[st.agentID] = events
	p.mu.Unlock()
	defer func() {
		p.dropReq(reqID)
		p.dropReq(st.agentID)
	}()

	results, extra, err := matchToolResults(req, st.pending)
	if err != nil {
		return nil, &bridgeError{msg: err.Error(), retryable: false}
	}
	for _, r := range results {
		if err := p.sendToolResult(r.id, reqID, r.out); err != nil {
			return nil, &bridgeError{msg: err.Error(), retryable: true}
		}
	}
	st.pending = nil
	if extra != "" {
		_ = p.send(map[string]any{
			"id":      reqID,
			"op":      "steer",
			"agentId": st.agentID,
			"text":    extra,
		})
	}
	return p.collect(ctx, svc, req, reqID, events, key, st, late)
}

type namedResult struct {
	id  string
	out toolOutcome
}

func matchToolResults(req *llm.Request, pending []pendingTool) ([]namedResult, string, error) {
	want := make(map[string]pendingTool, len(pending))
	for _, pt := range pending {
		want[pt.id] = pt
	}
	var results []namedResult
	var extra strings.Builder
	last := lastAssistantIndex(req.Messages)
	for _, m := range req.Messages[last+1:] {
		for _, c := range m.Content {
			switch c.Type {
			case llm.ContentTypeToolResult:
				if _, ok := want[c.ToolUseID]; ok {
					results = append(results, namedResult{
						id:  c.ToolUseID,
						out: toolOutcome{Content: c.ToolResult, IsError: c.ToolError},
					})
					delete(want, c.ToolUseID)
				}
			case llm.ContentTypeText:
				if strings.TrimSpace(c.Text) != "" {
					extra.WriteString(c.Text)
					extra.WriteString("\n")
				}
			}
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for id := range want {
			missing = append(missing, id)
		}
		return nil, "", fmt.Errorf("cursor bridge: missing tool results for %s", strings.Join(missing, ", "))
	}
	return results, strings.TrimSpace(extra.String()), nil
}

func (p *daemonProcess) dropReq(id string) {
	p.mu.Lock()
	delete(p.reqs, id)
	p.mu.Unlock()
}

// takeLateTools pops the deferred tool calls stashed for one agent.
func (p *daemonProcess) takeLateTools(agentID string) []*daemonLine {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.lateTools[agentID]
	delete(p.lateTools, agentID)
	return out
}

func (p *daemonProcess) collect(
	ctx context.Context,
	svc *Service,
	req *llm.Request,
	reqID string,
	events chan *daemonLine,
	key string,
	st *liveSession,
	late []*daemonLine, // tool_calls deferred from the previous batch window
) (*llm.Response, error) {
	start := time.Now()
	stopCancel := context.AfterFunc(ctx, func() {
		payload := map[string]any{"id": reqID, "op": "cancel"}
		if st != nil {
			payload["agentId"] = st.agentID
		}
		_ = p.send(payload)
	})
	defer stopCancel()

	var text strings.Builder
	var thinking strings.Builder
	var usage llm.Usage
	var toolUses []llm.Content
	// Deferred tool calls from the previous window are yielded immediately:
	// Shelley must run them before the Cursor run can make progress.
	deferred := len(late) > 0
	for _, dl := range late {
		args := dl.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		toolUses = append(toolUses, llm.Content{
			Type:      llm.ContentTypeToolUse,
			ID:        dl.ID,
			ToolName:  dl.Name,
			ToolInput: args,
		})
	}

	handleDelta := func(dl *daemonLine) {
		if dl.Kind == "thinking" {
			thinking.WriteString(dl.Text)
			if req.OnStream != nil {
				req.OnStream(llm.StreamDelta{Type: "thinking", Text: dl.Text})
			}
			return
		}
		text.WriteString(dl.Text)
		if req.OnStream != nil {
			req.OnStream(llm.StreamDelta{Type: "text", Text: dl.Text})
		}
	}

	addTool := func(dl *daemonLine) {
		args := dl.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		toolUses = append(toolUses, llm.Content{
			Type:      llm.ContentTypeToolUse,
			ID:        dl.ID,
			ToolName:  dl.Name,
			ToolInput: args,
		})
	}

	const batchGrace = 300 * time.Millisecond
	// yieldTools flushes the collected tool batch to Shelley: records it as
	// the session's pending set and returns a tool_use response.
	yieldNow := func() *llm.Response {
		if st != nil {
			st.pending = pendingFrom(toolUses)
			p.putSession(key, st)
		}
		return finishResponse(svc, text.String(), thinking.String(), toolUses, usage, llm.StopReasonToolUse, start)
	}

	for {
		if deferred {
			// Late tools from the previous window: skip waiting for new
			// events and yield them (plus anything already queued) now.
			for {
				select {
				case more := <-events:
					switch more.Event {
					case "tool_call":
						addTool(more)
					case "delta":
						handleDelta(more)
					default:
					}
				default:
					return yieldNow(), nil
				}
			}
		}
		select {
		case <-ctx.Done():
			if key != "" {
				p.clearSession(key)
			}
			return nil, ctx.Err()
		case dl, ok := <-events:
			if !ok {
				return nil, &bridgeError{msg: "cursor bridge: event channel closed", retryable: true}
			}
			switch dl.Event {
			case "":
				// sync ack (steer/reset); ignore
			case "delta":
				handleDelta(dl)
			case "tool_call":
				addTool(dl)
				// The Cursor agent may emit several custom-tool calls in one
				// burst; there is no explicit batch-end signal in the SDK.
				// Collect siblings until a short grace period passes with no
				// new call (or the run ends), then yield the batch to Shelley.
				for {
					select {
					case more := <-events:
						switch more.Event {
						case "tool_call":
							addTool(more)
						case "delta":
							handleDelta(more)
						case "result":
							// The run ended while tools were pending (results already
							// delivered by a previous round). Yield anyway so the
							// tool_use blocks are still persisted.
							return yieldNow(), nil
						default:
						}
					case <-time.After(batchGrace):
						return yieldNow(), nil
					case <-ctx.Done():
						return yieldNow(), nil
					}
				}
			case "result":
				if !dl.OK {
					if key != "" {
						p.clearSession(key)
					}
					retryable := dl.Retryable != nil && *dl.Retryable
					return nil, &bridgeError{msg: fmt.Sprintf("cursor agent run failed: %s", dl.ErrorOrErr()), retryable: retryable}
				}
				if dl.Usage != nil {
					usage = usageFromBridge(dl.Usage)
				}
				// Deltas this round only. result.text is the whole Cursor run.
				final := text.String()
				if strings.TrimSpace(final) == "" {
					final = dl.Text
				}
				if key != "" {
					p.clearSession(key)
				}
				return finishResponse(svc, final, thinking.String(), nil, usage, llm.StopReasonEndTurn, start), nil
			default:
			}
		}
	}
}

func pendingFrom(tools []llm.Content) []pendingTool {
	out := make([]pendingTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, pendingTool{id: t.ID, name: t.ToolName, args: t.ToolInput})
	}
	return out
}

func usageFromBridge(u *BridgeUsage) llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	return llm.Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
	}
}

func finishResponse(svc *Service, text, thinkingText string, toolUses []llm.Content, usage llm.Usage, stop llm.StopReason, start time.Time) *llm.Response {
	end := time.Now()
	content := []llm.Content{}
	if strings.TrimSpace(thinkingText) != "" {
		content = append(content, llm.Content{Type: llm.ContentTypeThinking, Thinking: thinkingText})
	}
	if strings.TrimSpace(text) != "" {
		content = append(content, llm.Content{Type: llm.ContentTypeText, Text: text})
	}
	content = append(content, toolUses...)
	if len(content) == 0 {
		content = append(content, llm.Content{Type: llm.ContentTypeText, Text: ""})
	}
	return &llm.Response{
		ID:         "cursor-" + newRequestID(),
		Type:       "message",
		Role:       llm.MessageRoleAssistant,
		Model:      cmpOr(svc.DisplayName, svc.ModelID),
		Content:    content,
		StopReason: stop,
		Usage:      usage,
		StartTime:  &start,
		EndTime:    &end,
		URL:        "cursor-sdk://" + svc.ModelID,
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func workingDirFor(ctx context.Context) string {
	if wd := llm.WorkingDir(ctx); wd != "" {
		return wd
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "/"
}

func toolDescriptors(req *llm.Request) []map[string]any {
	if len(req.Tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(req.Tools))
	for _, t := range req.Tools {
		if t.ServerSide {
			continue
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = llm.EmptySchema()
		}
		var schemaObj map[string]any
		if err := json.Unmarshal(schema, &schemaObj); err != nil {
			schemaObj = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		desc := t.Description
		if t.CustomGrammar != "" {
			desc += "\n\nInput must match this Lark grammar:\n" + t.CustomGrammar
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": desc,
			"inputSchema": schemaObj,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
