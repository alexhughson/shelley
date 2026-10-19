package claudetool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/predictable"
	"shelley.exe.dev/loop"
)

type changeDirBatchService struct {
	*predictable.Service
	calls []llm.Content
}

func (s *changeDirBatchService) Do(context.Context, *llm.Request) (*llm.Response, error) {
	if s.calls == nil {
		return &llm.Response{Role: llm.MessageRoleAssistant, StopReason: llm.StopReasonEndTurn}, nil
	}
	calls := s.calls
	s.calls = nil
	return &llm.Response{Role: llm.MessageRoleAssistant, StopReason: llm.StopReasonToolUse, Content: calls}, nil
}

func TestChangeDirBatchWithBash(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	call := func(id, name, key, value string) llm.Content {
		input, err := json.Marshal(map[string]string{key: value})
		if err != nil {
			t.Fatal(err)
		}
		return llm.Content{ID: id, Type: llm.ContentTypeToolUse, ToolName: name, ToolInput: input}
	}
	calls := []llm.Content{
		call("initial", "bash", "command", "pwd"),
		call("to-a", "change_dir", "path", "a"),
		call("a-first", "bash", "command", "pwd"),
		call("a-second", "bash", "command", "pwd"),
		call("missing", "change_dir", "path", filepath.Join(root, "missing")),
		call("blocked-write", "bash", "command", "touch must-not-exist"),
		call("blocked-relative", "change_dir", "path", "../b"),
		call("blocked-read", "bash", "command", "pwd"),
		call("recover", "change_dir", "path", filepath.Join(root, "b")),
		call("b", "bash", "command", "pwd"),
	}
	wd := NewMutableWorkingDir(root)
	var recorded []llm.Message
	err := loop.Run(t.Context(), loop.RunConfig{
		LLM: &changeDirBatchService{Service: predictable.NewService(), calls: calls},
		Tools: []*llm.Tool{
			(&ChangeDirTool{WorkingDir: wd}).Tool(),
			(&BashTool{WorkingDir: wd}).Tool(),
		},
		Hooks: loop.Hooks{OnToolResponse: func(_ context.Context, response loop.ToolResponse) error {
			recorded = append(recorded, response.Message)
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || len(recorded[0].Content) != len(calls) {
		t.Fatalf("want one result message with %d results, got %+v", len(calls), recorded)
	}
	wantDirs := map[string]string{
		"initial": root, "a-first": filepath.Join(root, "a"),
		"a-second": filepath.Join(root, "a"), "b": filepath.Join(root, "b"),
	}
	for i, result := range recorded[0].Content {
		if result.ToolUseID != calls[i].ID {
			t.Fatalf("result %d ID = %q, want %q", i, result.ToolUseID, calls[i].ID)
		}
		wantError := i >= 4 && i <= 7
		if result.ToolError != wantError {
			t.Errorf("%s error = %v, want %v: %+v", result.ToolUseID, result.ToolError, wantError, result.ToolResult)
		}
		if dir, ok := wantDirs[result.ToolUseID]; ok {
			if len(result.ToolResult) != 1 || strings.TrimSpace(result.ToolResult[0].Text) != dir {
				t.Errorf("%s output = %+v, want %q", result.ToolUseID, result.ToolResult, dir)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "a", "must-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("dependent write was not skipped: stat error = %v", err)
	}
	if got := wd.Get(); got != filepath.Join(root, "b") {
		t.Errorf("final directory = %q, want b", got)
	}
}

func TestChangeDirBatchRecoversFromDeletedWorkingDir(t *testing.T) {
	root := t.TempDir()
	deleted := filepath.Join(root, "deleted")
	if err := os.Mkdir(deleted, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := NewMutableWorkingDir(deleted)
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "must-not-exist")
	encode := func(input any) json.RawMessage {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	calls := []llm.Content{
		{
			ID: "failed-write", Type: llm.ContentTypeToolUse, ToolName: "bash",
			ToolInput: encode(bashInput{Command: fmt.Sprintf("touch %q", sentinel)}),
		},
		{
			ID: "recover", Type: llm.ContentTypeToolUse, ToolName: "change_dir",
			ToolInput: encode(changeDirInput{Path: root}),
		},
		{
			ID: "retry", Type: llm.ContentTypeToolUse, ToolName: "bash",
			ToolInput: encode(bashInput{Command: "pwd"}),
		},
	}
	changeDir := (&ChangeDirTool{WorkingDir: wd}).Tool()
	runChangeDir := changeDir.Run
	var beforeRecovery string
	changeDir.Run = func(ctx context.Context, input json.RawMessage) llm.ToolOut {
		beforeRecovery = wd.Get()
		return runChangeDir(ctx, input)
	}
	var recorded []llm.Message
	err := loop.Run(t.Context(), loop.RunConfig{
		LLM:   &changeDirBatchService{Service: predictable.NewService(), calls: calls},
		Tools: []*llm.Tool{changeDir, (&BashTool{WorkingDir: wd}).Tool()},
		Hooks: loop.Hooks{OnToolResponse: func(_ context.Context, response loop.ToolResponse) error {
			recorded = append(recorded, response.Message)
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || len(recorded[0].Content) != len(calls) {
		t.Fatalf("want one result message with %d results, got %+v", len(calls), recorded)
	}
	results := recorded[0].Content
	for i, result := range results {
		if result.ToolUseID != calls[i].ID || result.ToolError != (i == 0) {
			t.Errorf("result %d = %+v, want ID %q and error %v", i, result, calls[i].ID, i == 0)
		}
	}
	if len(results[0].ToolResult) != 1 || !strings.Contains(results[0].ToolResult[0].Text, "working directory does not exist: "+deleted) {
		t.Errorf("missing-directory error = %+v", results[0].ToolResult)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("failed command had a side effect: sentinel stat error = %v", err)
	}
	if beforeRecovery != deleted {
		t.Errorf("cwd changed before explicit recovery: got %q, want %q", beforeRecovery, deleted)
	}
	if len(results[2].ToolResult) != 1 || strings.TrimSpace(results[2].ToolResult[0].Text) != root {
		t.Errorf("retry output = %+v, want %q", results[2].ToolResult, root)
	}
	if got := wd.Get(); got != root {
		t.Errorf("final directory = %q, want %q", got, root)
	}
}
