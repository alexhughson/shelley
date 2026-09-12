package cursorbridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"shelley.exe.dev/llm"
)

// seedPrompt is the full transcript sent on Agent.create (first turn of a
// conversation or the first turn after compaction reset).
func seedPrompt(req *llm.Request) string {
	var b strings.Builder
	if sys := systemPromptText(req); sys != "" {
		b.WriteString("<system_instructions>\n")
		b.WriteString(sys)
		b.WriteString("\n</system_instructions>\n\n")
	}
	for _, m := range req.Messages {
		b.WriteString(renderHistoryMessage(m))
	}
	return strings.TrimSpace(b.String())
}

// newUserPrompt is the text + images after the last assistant message.
// Agent.resume / a follow-up send() gets only this tail.
func newUserPrompt(req *llm.Request) (text string, images []map[string]string) {
	lastAssistant := lastAssistantIndex(req.Messages)
	var b strings.Builder
	for _, m := range req.Messages[lastAssistant+1:] {
		for _, c := range m.Content {
			if c.Type == llm.ContentTypeText && c.Text != "" {
				b.WriteString(c.Text)
				b.WriteString("\n")
			}
		}
		images = append(images, messageImages(m)...)
	}
	return strings.TrimSpace(b.String()), images
}

func lastAssistantIndex(msgs []llm.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.MessageRoleAssistant {
			return i
		}
	}
	return -1
}

func renderHistoryMessage(m llm.Message) string {
	role := "user"
	if m.Role == llm.MessageRoleAssistant {
		role = "assistant"
	}
	var b strings.Builder
	switch {
	case m.ErrorType != "":
		fmt.Fprintf(&b, "[%s: system error] %s\n\n", role, m.ErrorType)
	default:
		fmt.Fprintf(&b, "<%s>\n", role)
		for _, c := range m.Content {
			renderContent(&b, c)
		}
		b.WriteString("</" + role + ">\n\n")
	}
	return b.String()
}

func renderContent(b *strings.Builder, c llm.Content) {
	switch c.Type {
	case llm.ContentTypeText:
		b.WriteString(c.Text)
		b.WriteString("\n")
	case llm.ContentTypeThinking, llm.ContentTypeRedactedThinking:
		return
	case llm.ContentTypeToolUse:
		fmt.Fprintf(b, "[tool call: %s(%s)]\n", c.ToolName, toolInputString(c.ToolInput))
	case llm.ContentTypeToolResult:
		b.WriteString("[tool result")
		if c.ToolError {
			b.WriteString(" (error)")
		}
		b.WriteString("]\n")
		for _, rc := range c.ToolResult {
			if rc.Type == llm.ContentTypeText {
				b.WriteString(rc.Text)
				b.WriteString("\n")
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
