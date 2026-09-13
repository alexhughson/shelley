package cursorbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
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

	mu       sync.Mutex
	reqs     map[string]*eventQueue  // request/agent id -> event queue
	sessions map[string]*liveSession // agent id -> in-flight run
	dead     bool
}

// eventQueue is an unbounded mailbox. routeReq never blocks the stdout
// reader: a full channel would stall every conversation on this daemon.
type eventQueue struct {
	ch chan *daemonLine
}

func newEventQueue() *eventQueue {
	return &eventQueue{ch: make(chan *daemonLine, 256)}
}

func (q *eventQueue) recv() <-chan *daemonLine {
	return q.ch
}

func (q *eventQueue) send(dl *daemonLine) {
	select {
	case q.ch <- dl:
	default:
		go func() { q.ch <- dl }()
	}
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
	ID        string     `json:"id"`
	Op        string     `json:"op"`
	Event     string     `json:"event"`
	Req       string     `json:"req"` // owning request id on tool_call events
	Agent     string     `json:"agent"`
	OK        bool       `json:"ok"`
	Node      string     `json:"node"`
	Error     string     `json:"error"`
	Err       string     `json:"err"`
	Retryable *bool      `json:"retryable"`
	Status    flexStatus `json:"status"`
	Code      string     `json:"code"`
	Cancelled bool       `json:"cancelled"`

	// prompt events
	Kind   string          `json:"kind"`   // "text" | "thinking" (delta)
	Text   string          `json:"text"`   // delta text or final result text
	Name   string          `json:"name"`   // single-call fallback name
	Result json.RawMessage `json:"result"` // unused; kept for older daemon lines
	Args   json.RawMessage `json:"args"`   // single-call fallback args
	Calls  []bridgeCall    `json:"calls"`  // tool_calls batch

	// models op
	Models []CursorModel `json:"models"`

	// result event
	Usage *BridgeUsage `json:"usage"`
	Model string       `json:"model"`
}

// bridgeCall is one custom-tool invocation in a tool_calls event.
type bridgeCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
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
			// Tagged with req (current Do id). collect() yields StopReasonToolUse.
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
		ch.send(&daemonLine{ID: id, Event: "result", Error: "cursor bridge: daemon exited"})
		delete(p.reqs, id)
	}
	p.sessions = nil
	p.mu.Unlock()
}

func (p *daemonProcess) routeReq(dl daemonLine) {
	p.mu.Lock()
	q := p.reqs[dl.ID]
	if q == nil && dl.Req != "" {
		q = p.reqs[dl.Req]
	}
	if q == nil && dl.Agent != "" {
		q = p.reqs[dl.Agent]
	}
	p.mu.Unlock()
	if q == nil {
		p.svc.logger().Error("cursor bridge: event with no listener", "event", dl.Event, "id", dl.ID, "req", dl.Req, "agent", dl.Agent)
		return
	}
	cl := dl
	q.send(&cl)
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

func (p *daemonProcess) sendToolResult(toolCallID, reqID string, out toolOutcome) error {
	content := make([]map[string]any, 0, len(out.Content))
	for _, c := range out.Content {
		switch c.Type {
		case llm.ContentTypeText:
			content = append(content, map[string]any{"type": "text", "text": c.Text})
		default:
			b, err := json.Marshal(c)
			if err != nil {
				return fmt.Errorf("cursor bridge: encode tool result: %w", err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				return fmt.Errorf("cursor bridge: encode tool result: %w", err)
			}
			content = append(content, m)
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	msg := map[string]any{
		"id":      toolCallID,
		"op":      "tool_result",
		"content": content,
		"isError": out.IsError,
	}
	if reqID != "" {
		msg["req"] = reqID
	}
	return p.send(msg)
}

// ping performs the startup handshake.
func (p *daemonProcess) ping(ctx context.Context) error {
	id := newRequestID()
	q := newEventQueue()
	p.mu.Lock()
	p.reqs[id] = q
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
	case dl := <-q.recv():
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
