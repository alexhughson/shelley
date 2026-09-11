package cursorbridge

import (
	"bufio"
	"strconv"
	"strings"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"shelley.exe.dev/llm"
)

// daemonProcess wraps one running daemon.mjs child. It is shared by all
// concurrent Do calls on a Service; requests are multiplexed by id.
type daemonProcess struct {
	svc    *Service
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu    sync.Mutex
	reqs  map[string]chan *daemonLine // request id -> response line channel
	tools map[string]chan *daemonLine // tool call id -> tool_call event channel
	dead  bool
}

// flexStatus decodes a JSON field that may be a number (HTTP status on
// error results) or a string (tool_call run status).
type flexStatus json.RawMessage

func (f *flexStatus) UnmarshalJSON(b []byte) error {
	*f = flexStatus(append([]byte(nil), b...))
	return nil
}

func (f flexStatus) String() string {
	return strings.Trim(string(f), `"`)
}

func (f flexStatus) Int() int {
	n, _ := strconv.Atoi(strings.Trim(string(f), `"`))
	return n
}

// daemonLine is one decoded JSON line from the daemon.
type daemonLine struct {
	ID        string `json:"id"`
	Op        string `json:"op"`
	Event     string `json:"event"`
	Req       string `json:"req"` // owning request id on tool_call events
	OK        bool   `json:"ok"`
	Node      string `json:"node"`
	Error     string `json:"error"`
	Err       string `json:"err"`
	Retryable *bool  `json:"retryable"`
	Status    flexStatus `json:"status"`
	Code      string `json:"code"`
	Cancelled bool   `json:"cancelled"`

	// prompt events
	Kind   string          `json:"kind"`   // "text" | "thinking" (delta)
	Text   string          `json:"text"`   // delta text or final result text
	Name   string          `json:"name"`   // tool_call name
	Result json.RawMessage `json:"result"` // tool_call completion payload
	Args   json.RawMessage `json:"args"`   // tool_call args

	// models op
	Models []CursorModel `json:"models"`

	// result event
	Usage *BridgeUsage `json:"usage"`
	Model string       `json:"model"`
}

// CursorModel is one entry of the daemon's models op response.
type CursorModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

// BridgeUsage is the daemon's token usage report for one run.
type BridgeUsage struct {
	InputTokens      uint64 `json:"inputTokens"`
	OutputTokens     uint64 `json:"outputTokens"`
	CacheReadTokens  uint64 `json:"cacheReadTokens"`
	CacheWriteTokens uint64 `json:"cacheWriteTokens"`
	ReasoningTokens  uint64 `json:"reasoningTokens"`
	TotalTokens      uint64 `json:"totalTokens"`
}

// toolOutcome is Shelley's answer to a daemon tool_call event.
type toolOutcome struct {
	Content []llm.Content
	IsError bool
}

func (p *daemonProcess) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.dead
}

func (p *daemonProcess) kill() {
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		return
	}
	p.dead = true
	p.mu.Unlock()
	_ = p.cmd.Process.Kill()
	_ = p.stdin.Close()
}

func (p *daemonProcess) readLoop() {
	scanner := bufio.NewScanner(p.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var dl daemonLine
		if err := json.Unmarshal(line, &dl); err != nil {
			p.svc.logger().Warn("cursor bridge: malformed daemon line", "error", err)
			continue
		}
		switch dl.Event {
		case "":
			// Response to a sync op (ping/models) — route by id.
			p.routeReq(dl)
		case "tool_call":
			// The daemon wants Shelley to run a tool. Tool calls are tagged
			// with the owning request id (daemon threads it through the
			// custom tool closure), so they ride the request's event channel;
			// do() spawns runTool which answers via op=tool_result keyed by
			// the tool call id in p.tools (for late/routing edge cases).
			p.routeReq(dl)
		default:
			// delta / result for an in-flight prompt; routed via reqs.
			p.routeReq(dl)
		}
	}
	// stdout closed: daemon is gone. Fail every waiter.
	p.mu.Lock()
	p.dead = true
	for id, ch := range p.reqs {
		select {
		case ch <- &daemonLine{ID: id, Event: "result", Error: "cursor bridge: daemon exited"}:
		default:
		}
		delete(p.reqs, id)
	}
	for id, ch := range p.tools {
		select {
		case ch <- &daemonLine{ID: id}:
		default:
		}
		delete(p.tools, id)
	}
	p.mu.Unlock()
}

func (p *daemonProcess) routeReq(dl daemonLine) {
	key := dl.ID
	if dl.Event == "tool_call" && dl.Req != "" {
		key = dl.Req
	}
	p.mu.Lock()
	ch := p.reqs[key]
	p.mu.Unlock()
	if ch != nil {
		cl := dl
		ch <- &cl // buffered (256); do() drains continuously so this never blocks long
	}
}

func (p *daemonProcess) stderrLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		p.svc.logger().Debug("cursor daemon stderr", "line", scanner.Text())
	}
}

// send writes one JSON line to the daemon's stdin.
func (p *daemonProcess) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead {
		return errors.New("cursor bridge: daemon is down")
	}
	_, err = p.stdin.Write(append(data, '\n'))
	return err
}

func (p *daemonProcess) sendToolResult(toolCallID string, out toolOutcome) error {
	content := make([]map[string]any, 0, len(out.Content))
	for _, c := range out.Content {
		switch c.Type {
		case llm.ContentTypeText:
			content = append(content, map[string]any{"type": "text", "text": c.Text})
		default:
			// Best-effort JSON dump for anything richer (images, etc.).
			if b, err := json.Marshal(c); err == nil {
				var m map[string]any
				if json.Unmarshal(b, &m) == nil {
					content = append(content, m)
				}
			}
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	return p.send(map[string]any{
		"id":      toolCallID,
		"op":      "tool_result",
		"content": content,
		"isError": out.IsError,
	})
}

// ping performs the startup handshake.
func (p *daemonProcess) ping(ctx context.Context) error {
	id := newRequestID()
	ch := make(chan *daemonLine, 4)
	p.mu.Lock()
	p.reqs[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.reqs, id)
		p.mu.Unlock()
	}()
	if err := p.send(map[string]any{"id": id, "op": "ping"}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case dl := <-ch:
		if dl.OK {
			return nil
		}
		return fmt.Errorf("ping failed: %s", dl.ErrorOrErr())
	}
}

func (dl *daemonLine) ErrorOrErr() string {
	if dl.Error != "" {
		return dl.Error
	}
	return dl.Err
}
