package cursorbridge

import (
	"context"
	"encoding/json"
	"errors"
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
	events  chan *daemonLine // lives until the Cursor run ends
}

func conversationAgentID(ctx context.Context, modelID string) string {
	id := llmhttp.ConversationIDFromContext(ctx)
	if id == "" {
		return newRequestID() + ":" + modelID
	}
	return id + ":" + modelID
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

func (p *daemonProcess) attach(id string, ch chan *daemonLine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs[id] = ch
}

func (p *daemonProcess) detach(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.reqs, id)
}

func (p *daemonProcess) endRun(reqID, agentID string) {
	p.mu.Lock()
	delete(p.reqs, reqID)
	if agentID != "" {
		delete(p.reqs, agentID)
		delete(p.sessions, agentID)
	}
	p.mu.Unlock()
}

// do runs one Shelley LLM round. Cursor owns the durable agent; this returns
// at the next native loop boundary: tool_use (loop runs the tool) or end_turn.
func (p *daemonProcess) do(ctx context.Context, svc *Service, req *llm.Request) (*llm.Response, error) {
	purpose := llm.PurposeFromContext(ctx)
	if purpose != "" {
		return p.oneshot(ctx, svc, req, purpose)
	}
	key := conversationAgentID(ctx, svc.ModelID)
	if st := p.sessionOf(key); st != nil && len(st.pending) > 0 {
		return p.continueTurn(ctx, svc, req, key, st)
	}
	return p.startTurn(ctx, svc, req, key)
}

func (p *daemonProcess) oneshot(ctx context.Context, svc *Service, req *llm.Request, purpose string) (*llm.Response, error) {
	reqID := newRequestID()
	events := make(chan *daemonLine, 256)
	p.attach(reqID, events)
	defer p.detach(reqID)

	cwd, err := workingDirFor(ctx)
	if err != nil {
		return nil, err
	}
	tools, err := toolDescriptors(req)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"id":      reqID,
		"op":      "prompt",
		"oneshot": true,
		"model":   modelSelection(svc),
		"cwd":     cwd,
		"seed":    seedPrompt(req),
		"message": oneshotMessage(req),
		"tools":   tools,
	}
	if imgs := allImages(req); len(imgs) > 0 {
		payload["seedImages"] = imgs
	}
	if err := p.send(payload); err != nil {
		return nil, &bridgeError{msg: err.Error(), retryable: true}
	}
	resp, err := p.collect(ctx, svc, req, reqID, events, "", nil)
	if err != nil {
		return nil, err
	}
	if purpose == "compaction" {
		if id := llmhttp.ConversationIDFromContext(ctx); id != "" {
			if err := p.resetAgent(ctx, id+":"+svc.ModelID); err != nil {
				return nil, err
			}
		}
	}
	return resp, nil
}

func (p *daemonProcess) resetAgent(ctx context.Context, agentID string) error {
	reqID := newRequestID()
	ch := make(chan *daemonLine, 4)
	p.attach(reqID, ch)
	defer p.detach(reqID)
	if err := p.send(map[string]any{"id": reqID, "op": "reset", "agentId": agentID}); err != nil {
		return &bridgeError{msg: err.Error(), retryable: true}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		return fmt.Errorf("cursor bridge: reset %s: %w", agentID, ctx.Err())
	case dl := <-ch:
		if !dl.OK {
			return fmt.Errorf("cursor bridge: reset %s: %s", agentID, dl.ErrorOrErr())
		}
		p.clearSession(agentID)
		return nil
	}
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
	st := &liveSession{agentID: agentID, events: events}
	p.attach(reqID, events)
	p.attach(agentID, events)
	defer p.detach(reqID)

	cwd, err := workingDirFor(ctx)
	if err != nil {
		return nil, err
	}
	tools, err := toolDescriptors(req)
	if err != nil {
		return nil, err
	}
	message, images := newUserPrompt(req)
	payload := map[string]any{
		"id":      reqID,
		"op":      "prompt",
		"agentId": agentID,
		"model":   modelSelection(svc),
		"cwd":     cwd,
		"seed":    seedPrompt(req),
		"message": message,
		"tools":   tools,
	}
	if seedImgs := allImages(req); len(seedImgs) > 0 {
		payload["seedImages"] = seedImgs
	}
	if len(images) > 0 {
		payload["images"] = images
	}
	if err := p.send(payload); err != nil {
		return nil, &bridgeError{msg: err.Error(), retryable: true}
	}
	return p.collect(ctx, svc, req, reqID, events, key, st)
}

func (p *daemonProcess) continueTurn(ctx context.Context, svc *Service, req *llm.Request, key string, st *liveSession) (*llm.Response, error) {
	reqID := newRequestID()
	events := st.events
	if events == nil {
		return nil, errors.New("cursor bridge: session has no event channel")
	}
	p.attach(reqID, events)
	p.attach(st.agentID, events)
	defer p.detach(reqID)

	results, extra, err := matchToolResults(req, st.pending)
	if err != nil {
		return nil, &bridgeError{msg: err.Error(), retryable: false}
	}
	sent := make(map[string]bool, len(results))
	for _, r := range results {
		if err := p.sendToolResult(r.id, reqID, r.out); err != nil {
			var keep []pendingTool
			for _, pt := range st.pending {
				if !sent[pt.id] {
					keep = append(keep, pt)
				}
			}
			st.pending = keep
			p.putSession(key, st)
			return nil, &bridgeError{msg: err.Error(), retryable: true}
		}
		sent[r.id] = true
	}
	st.pending = nil
	if extra != "" {
		if err := p.steer(ctx, st.agentID, extra); err != nil {
			return nil, err
		}
	}
	return p.collect(ctx, svc, req, reqID, events, key, st)
}

func (p *daemonProcess) steer(ctx context.Context, agentID, text string) error {
	id := newRequestID()
	ch := make(chan *daemonLine, 4)
	p.attach(id, ch)
	defer p.detach(id)
	if err := p.send(map[string]any{
		"id":      id,
		"op":      "steer",
		"agentId": agentID,
		"text":    text,
	}); err != nil {
		return &bridgeError{msg: err.Error(), retryable: true}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case dl := <-ch:
		if !dl.OK {
			return fmt.Errorf("cursor bridge: steer: %s", dl.ErrorOrErr())
		}
		return nil
	}
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

func (p *daemonProcess) collect(
	ctx context.Context,
	svc *Service,
	req *llm.Request,
	reqID string,
	events chan *daemonLine,
	key string,
	st *liveSession,
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

	addCalls := func(dl *daemonLine) {
		calls := dl.Calls
		if len(calls) == 0 && (dl.Name != "" || dl.Event == "tool_call") {
			args := dl.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			calls = []bridgeCall{{ID: dl.ID, Name: dl.Name, Args: args}}
		}
		for _, c := range calls {
			args := c.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			id := c.ID
			if id == "" {
				id = dl.ID
			}
			toolUses = append(toolUses, llm.Content{
				Type:      llm.ContentTypeToolUse,
				ID:        id,
				ToolName:  c.Name,
				ToolInput: args,
			})
		}
	}

	yieldNow := func() *llm.Response {
		if st != nil {
			st.pending = pendingFrom(toolUses)
			p.putSession(key, st)
		}
		return finishResponse(svc, text.String(), thinking.String(), toolUses, usage, llm.StopReasonToolUse, start)
	}

	endFailed := func(err error) (*llm.Response, error) {
		if st != nil {
			p.endRun(reqID, st.agentID)
		} else if key != "" {
			p.clearSession(key)
		}
		return nil, err
	}

	for {
		select {
		case <-ctx.Done():
			return endFailed(ctx.Err())
		case dl, ok := <-events:
			if !ok {
				return endFailed(&bridgeError{msg: "cursor bridge: event channel closed", retryable: true})
			}
			switch dl.Event {
			case "":
				// sync ack for an op multiplexed on this channel; ignore
			case "delta":
				handleDelta(dl)
			case "tool_call", "tool_calls":
				addCalls(dl)
				return yieldNow(), nil
			case "result":
				if dl.Cancelled {
					if err := ctx.Err(); err != nil {
						return endFailed(err)
					}
					return endFailed(context.Canceled)
				}
				if !dl.OK {
					retryable := dl.Retryable != nil && *dl.Retryable
					return endFailed(&bridgeError{msg: fmt.Sprintf("cursor agent run failed: %s", dl.ErrorOrErr()), retryable: retryable})
				}
				if dl.Usage != nil {
					usage = usageFromBridge(dl.Usage)
				}
				final := text.String()
				if strings.TrimSpace(final) == "" {
					final = dl.Text
				}
				if st != nil {
					p.endRun(reqID, st.agentID)
				} else if key != "" {
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

// usageFromBridge maps Cursor TokenUsage onto llm.Usage.
// Cursor's inputTokens is the full prompt. cacheRead/cacheWrite are a
// breakdown of that prompt, not extra tokens. Subtract them so
// TotalInputTokens() does not count the cached span twice.
func usageFromBridge(u *BridgeUsage) llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	input := u.InputTokens
	cached := u.CacheReadTokens + u.CacheWriteTokens
	if cached > 0 && input >= cached {
		input -= cached
	}
	return llm.Usage{
		InputTokens:              input,
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

func workingDirFor(ctx context.Context) (string, error) {
	if wd := llm.WorkingDir(ctx); wd != "" {
		return wd, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cursor bridge: working directory: %w", err)
	}
	return wd, nil
}

func toolDescriptors(req *llm.Request) ([]map[string]any, error) {
	if len(req.Tools) == 0 {
		return nil, nil
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
			return nil, fmt.Errorf("cursor bridge: tool %s schema: %w", t.Name, err)
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
		return nil, nil
	}
	return out, nil
}
