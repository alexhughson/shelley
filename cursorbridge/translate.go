package cursorbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"shelley.exe.dev/llm"
)

// ModelParam is one Cursor model parameter (e.g. {ID: "effort", Value: "high"}).
type ModelParam struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// modelSelection builds the SDK ModelSelection payload: {id, params?}.
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

// bridgeError carries retryability across the process boundary.
type bridgeError struct {
	msg       string
	retryable bool
}

func (e *bridgeError) Error() string { return e.msg }

// RequestErrorInfo implements llm.RequestError so the loop's retry logic can
// classify bridge failures.
func (e *bridgeError) RequestErrorInfo() llm.RequestErrorInfo {
	return llm.RequestErrorInfo{Retryable: e.retryable}
}

// do runs one full agent turn against the daemon: sends the prompt, executes
// every tool the Cursor agent requests (streaming progress and text deltas
// back), and returns the synthesized llm.Response.
func (p *daemonProcess) do(ctx context.Context, svc *Service, req *llm.Request) (*llm.Response, error) {
	reqID := newRequestID()

	// Channels: events (deltas, tool calls, result) all arrive tagged with
	// reqID on reqs; tool_call events are redirected to per-call channels via
	// p.tools so concurrent tool calls multiplex cleanly.
	events := make(chan *daemonLine, 256)
	p.mu.Lock()
	p.reqs[reqID] = events
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.reqs, reqID)
		p.mu.Unlock()
	}()

	prompt := buildPrompt(req)
	payload := map[string]any{
		"id":      reqID,
		"op":      "prompt",
		"apiKey":  svc.APIKey,
		"model":   modelSelection(svc),
		"cwd":     workingDirFor(ctx, req),
		"message": promptText(req),
		"tools":   toolDescriptors(req),
	}
	if len(prompt.images) > 0 {
		payload["images"] = prompt.images
	}
	if err := p.send(payload); err != nil {
		return nil, &bridgeError{msg: err.Error(), retryable: true}
	}

	// Cancellation: tell the daemon to cancel the run; the run's event loop
	// below observes ctx.Done and returns.
	stopCancel := context.AfterFunc(ctx, func() {
		_ = p.send(map[string]any{"id": reqID, "op": "cancel"})
	})
	defer stopCancel()

	registerToolChan := func(id string, ch chan *daemonLine) {
		p.mu.Lock()
		p.tools[id] = ch
		p.mu.Unlock()
	}
	clearToolChan := func(id string) {
		p.mu.Lock()
		delete(p.tools, id)
		p.mu.Unlock()
	}

	var text strings.Builder
	var thinking strings.Builder
	var usage llm.Usage
	var toolRuns int

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case dl, ok := <-events:
			if !ok {
				return nil, &bridgeError{msg: "cursor bridge: event channel closed", retryable: true}
			}
			switch dl.Event {
			case "delta":
				if dl.Kind == "thinking" {
					thinking.WriteString(dl.Text)
					if req.OnStream != nil {
						req.OnStream(llm.StreamDelta{Type: "thinking", Text: dl.Text})
					}
				} else {
					text.WriteString(dl.Text)
					if req.OnStream != nil {
						req.OnStream(llm.StreamDelta{Type: "text", Text: dl.Text})
					}
				}
			case "tool_call":
				// Shelley executes the tool and replies directly to the daemon.
				toolRuns++
				go p.runTool(ctx, req, dl, registerToolChan, clearToolChan)
			case "result":
				if dl.OK {
					if dl.Usage != nil {
						usage = llm.Usage{
							InputTokens:              dl.Usage.InputTokens,
							OutputTokens:             dl.Usage.OutputTokens - dl.Usage.ReasoningTokens,
							CacheCreationInputTokens: dl.Usage.CacheWriteTokens,
							CacheReadInputTokens:     dl.Usage.CacheReadTokens,
						}
					}
					return finishResponse(svc, text.String(), thinking.String(), usage, toolRuns), nil
				}
				retryable := dl.Retryable != nil && *dl.Retryable
				return nil, &bridgeError{msg: fmt.Sprintf("cursor agent run failed: %s", dl.ErrorOrErr()), retryable: retryable}
			default:
				// Unknown event kind; ignore.
			}
		}
	}
}

// runTool executes one tool call requested by the Cursor agent. It never
// blocks the event loop: the daemon's custom-tool promise is answered as soon
// as the Shelley tool's Run returns (or the request context dies).
func (p *daemonProcess) runTool(
	ctx context.Context,
	req *llm.Request,
	dl *daemonLine,
	register func(string, chan *daemonLine),
	clear func(string),
) {
	toolCallID := dl.ID
	ch := make(chan *daemonLine, 4)
	register(toolCallID, ch)
	defer clear(toolCallID)

	name := dl.Name
	tool := findTool(req.Tools, name)
	if tool == nil {
		_ = p.sendToolResult(toolCallID, toolOutcome{
			Content: llm.TextContent(fmt.Sprintf("Tool '%s' not found", name)),
			IsError: true,
		})
		return
	}

	input := dl.Args
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}

	p.svc.logger().Info("cursor bridge: tool call", "tool", name, "id", toolCallID)

	toolCtx := ctx
	if wd := llm.WorkingDir(ctx); wd != "" {
		toolCtx = llm.WithWorkingDir(toolCtx, wd)
	}
	if req.OnStream != nil {
		// Tools may report progress through the same OnStream sink the
		// service uses for deltas is not the progress channel; skip.
		_ = req.OnStream
	}
	toolCtx = llm.WithToolUseID(toolCtx, toolCallID)
	toolCtx = llm.WithLLMService(toolCtx, p.svc)

	result := tool.Run(toolCtx, input)

	out := toolOutcome{IsError: result.Error != nil}
	if result.Error != nil {
		out.Content = llm.TextContent(fmt.Sprintf("Tool '%s' failed: %v", name, result.Error))
	} else {
		out.Content = result.LLMContent
		if len(out.Content) == 0 {
			out.Content = llm.TextContent("")
		}
	}
	if err := p.sendToolResult(toolCallID, out); err != nil {
		p.svc.logger().Warn("cursor bridge: failed to deliver tool result", "tool", name, "error", err)
	}
}

func findTool(tools []*llm.Tool, name string) *llm.Tool {
	for _, t := range tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// finishResponse assembles the llm.Response for a completed turn.
func finishResponse(svc *Service, text, thinkingText string, usage llm.Usage, toolRuns int) *llm.Response {
	now := time.Now()
	content := []llm.Content{}
	if strings.TrimSpace(thinkingText) != "" {
		content = append(content, llm.Content{Type: llm.ContentTypeThinking, Thinking: thinkingText})
	}
	if strings.TrimSpace(text) != "" || len(content) == 0 {
		content = append(content, llm.Content{Type: llm.ContentTypeText, Text: text})
	}
	stopReason := llm.StopReasonEndTurn
	if toolRuns > 0 {
		// The turn is complete from Shelley's perspective; tools already ran
		// inside it. End turn cleanly.
		stopReason = llm.StopReasonEndTurn
	}
	return &llm.Response{
		ID:         "cursor-" + newRequestID(),
		Type:       "message",
		Role:       llm.MessageRoleAssistant,
		Model:      cmpOr(svc.DisplayName, svc.ModelID),
		Content:    content,
		StopReason: stopReason,
		Usage:      usage,
		StartTime:  &now,
		EndTime:    &now,
		URL:        "cursor-sdk://" + svc.ModelID,
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// workingDirFor picks the cursor agent working directory: the loop attaches
// it to ctx via llm.WithWorkingDir; fall back to the last tool-result path or
// the process CWD.
func workingDirFor(ctx context.Context, req *llm.Request) string {
	if wd := llm.WorkingDir(ctx); wd != "" {
		return wd
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "/"
}

// promptText combines Shelley's system prompt and the rendered prompt into the
// single user message the Cursor agent receives. The SDK's systemPrompt option
// replaces Cursor's own agent-loop prompt and is gated per account, so the
// bridge never uses it; instead the identity is stated inline, where the
// Cursor agent treats it as part of the user's instructions.
func promptText(req *llm.Request) string {
	sys := systemPromptText(req)
	if strings.TrimSpace(sys) == "" {
		return buildPrompt(req).text
	}
	return "<system_instructions>\n" + strings.TrimSpace(sys) + "\n</system_instructions>\n\n" + buildPrompt(req).text
}

// systemPromptText concatenates Shelley's system prompt entries.
func systemPromptText(req *llm.Request) string {
	var b strings.Builder
	for i, sc := range req.System {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(sc.Text)
	}
	return b.String()
}

// toolDescriptors converts Shelley tools to the daemon's custom-tool list.
func toolDescriptors(req *llm.Request) []map[string]any {
	if len(req.Tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(req.Tools))
	for _, t := range req.Tools {
		if t.ServerSide {
			continue // never offered to the Cursor agent
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
