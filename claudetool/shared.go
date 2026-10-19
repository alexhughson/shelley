// Package claudetool provides tools for Claude AI models.
//
// When adding, removing, or modifying tools in this package,
// remember to update the tool display template in termui/termui.go
// to ensure proper tool output formatting.
package claudetool

import (
	"context"

	"shelley.exe.dev/llm"
)

// GetToolProgress retrieves the ToolProgressFunc from the context, or nil.
func GetToolProgress(ctx context.Context) llm.ToolProgressFunc {
	return llm.GetToolProgress(ctx)
}

// WithToolUseID returns a context with the given tool use ID.
func WithToolUseID(ctx context.Context, id string) context.Context {
	return llm.WithToolUseID(ctx, id)
}

func ToolUseID(ctx context.Context) string {
	return llm.ToolUseID(ctx)
}
