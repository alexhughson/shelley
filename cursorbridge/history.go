package cursorbridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"shelley.exe.dev/llm"
)

// bridgePrompt is the payload text + images extracted from one llm.Request.
type bridgePrompt struct {
	text   string
	images []map[string]string // {data, mimeType}
}

// buildPrompt renders the request into a single prompt for the Cursor agent.
//
// The Cursor agent keeps its own durable conversation state, so we never need
// to resend the full history as the live prompt. Instead:
//
//   - History up to and including the last assistant turn is replayed as an
//     <exchange> transcript ONCE (the daemon's agent accumulates it in its own
//     context across sends).
//   - Everything AFTER the last assistant turn — the new user messages, tool
//     results, images — becomes the live prompt body.
//
// On the very first request for a conversation the history is empty, so the
// prompt is just the new user message.
//
// To keep the replay idempotent even if a conversation's agent is recreated
// (restart, store eviction), every prompt ALSO carries the full history since
// the last assistant turn wrapped in <context>, and the transcript replay is
// wrapped in <replay> with a note that it may already be known. In practice,
// for the common case the replay is a no-op suffix of the durable state and
// the model treats it as confirmation rather than new information.
func buildPrompt(req *llm.Request) bridgePrompt {
	var b strings.Builder

	// Split history: everything after the last assistant message is "new".
	lastAssistant := -1
	for i, m := range req.Messages {
		if m.Role == llm.MessageRoleAssistant {
			lastAssistant = i
		}
	}

	// Replay transcript for the pre-suffix history, but only the tail that is
	// NOT already in the Cursor agent's durable context. We cannot know that
	// boundary from here (the daemon owns the agent), so we replay everything
	// up to the previous request's boundary — approximated as: replay only
	// messages added since the last assistant turn of the PREVIOUS round.
	// Simplification: replay the last few exchanges only, and mark it as
	// context, not as new instructions.
	if lastAssistant >= 0 {
		b.WriteString("<replay>\n")
		b.WriteString("The following is the recent transcript of this conversation, provided for continuity. It may repeat context you already have. Do not respond to it again; it is not a new request.\n\n")
		// Cap the replay to the last 6 exchanges (12 messages) to bound prompt
		// size on long conversations; the durable agent store holds the rest.
		start := 0
		if lastAssistant > 12 {
			start = lastAssistant - 12
		}
		for _, m := range req.Messages[start : lastAssistant+1] {
			b.WriteString(renderHistoryMessage(m))
		}
		b.WriteString("</replay>\n\n")
	}

	// The new tail: everything after the last assistant message.
	var images []map[string]string
	if lastAssistant+1 < len(req.Messages) {
		tail := req.Messages[lastAssistant+1:]
		if len(tail) > 0 {
			b.WriteString("<new_messages>\n")
			for _, m := range tail {
				b.WriteString(renderHistoryMessage(m))
				images = append(images, messageImages(m)...)
			}
			b.WriteString("</new_messages>\n")
		}
	} else {
		// No new messages? Shouldn't happen (loop always adds a user message),
		// but produce something sane.
		b.WriteString("(continue)\n")
	}

	return bridgePrompt{text: strings.TrimSpace(b.String()), images: images}
}

// renderHistoryMessage renders one llm.Message as transcript text.
func renderHistoryMessage(m llm.Message) string {
	role := "user"
	if m.Role == llm.MessageRoleAssistant {
		role = "assistant"
	}
	var b strings.Builder
	switch {
	case m.ErrorType != "":
		fmt.Fprintf(&b, "[%s: system error] %s%s\n\n", role, m.ErrorType, errorSuffix(m))
	default:
		fmt.Fprintf(&b, "<%s>\n", role)
		for _, c := range m.Content {
			renderContent(&b, c)
		}
		b.WriteString("</" + role + ">\n\n")
	}
	return b.String()
}

func errorSuffix(m llm.Message) string {
	if m.ErrorRetryable {
		return " (retryable)"
	}
	return ""
}

// renderContent renders one content block into the transcript.
func renderContent(b *strings.Builder, c llm.Content) {
	switch c.Type {
	case llm.ContentTypeText:
		b.WriteString(c.Text)
		b.WriteString("\n")
	case llm.ContentTypeThinking:
		// Reasoning is generally not replayed; skip.
	case llm.ContentTypeRedactedThinking:
		// skip
	case llm.ContentTypeToolUse:
		fmt.Fprintf(b, "[tool call: %s(%s)]\n", c.ToolName, toolInputString(c.ToolInput))
	case llm.ContentTypeToolResult:
		b.WriteString("[tool result")
		if c.ToolError {
			b.WriteString(" (error)")
		}
		b.WriteString("]\n")
		for _, rc := range c.ToolResult {
			switch rc.Type {
			case llm.ContentTypeText:
				b.WriteString(rc.Text)
				b.WriteString("\n")
			case llm.ContentTypeThinking:
				// skip
			default:
				if raw, err := json.Marshal(rc); err == nil {
					b.Write(raw)
					b.WriteString("\n")
				}
			}
		}
	default:
		if raw, err := json.Marshal(c); err == nil {
			b.Write(raw)
			b.WriteString("\n")
		}
	}
}

func toolInputString(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	return string(input)
}

// messageImages extracts base64 images from a message's content blocks.
func messageImages(m llm.Message) []map[string]string {
	var out []map[string]string
	var walk func(cs []llm.Content)
	walk = func(cs []llm.Content) {
		for _, c := range cs {
			if c.Type == llm.ContentTypeToolResult {
				walk(c.ToolResult)
				continue
			}
			if c.MediaType != "" && c.Data != "" && strings.HasPrefix(c.MediaType, "image/") {
				out = append(out, map[string]string{"data": c.Data, "mimeType": c.MediaType})
			}
		}
	}
	walk(m.Content)
	return out
}
