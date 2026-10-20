package claudetool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

type lockCheckingSubagentRunner struct {
	tool *SubagentTool
	slug string
}

func (r *lockCheckingSubagentRunner) RunSubagent(context.Context, string, string, string, string) (string, error) {
	value, ok := r.tool.slugLocks.Load(r.slug)
	if !ok {
		return "", errors.New("slug lock was not created")
	}
	lock := value.(*sync.Mutex)
	if lock.TryLock() {
		lock.Unlock()
		return "", errors.New("slug lock was not held while running subagent")
	}
	return "ok", nil
}

func TestSubagentToolRunHoldsSlugLock(t *testing.T) {
	tool := &SubagentTool{
		DB:                   newMockSubagentDB(),
		ParentConversationID: "parent-123",
		WorkingDir:           NewMutableWorkingDir("/tmp"),
	}
	tool.Runner = &lockCheckingSubagentRunner{tool: tool, slug: "same-slug"}

	defined := tool.Tool()
	input, err := json.Marshal(subagentInput{Slug: "same-slug", Prompt: "do something"})
	if err != nil {
		t.Fatal(err)
	}
	if result := defined.Run(t.Context(), input); result.Error != nil {
		t.Fatalf("subagent run failed: %v", result.Error)
	}
}

// mockSubagentDB implements SubagentDB for testing.
type mockSubagentDB struct {
	conversations map[string]string // slug -> conversationID
}

func newMockSubagentDB() *mockSubagentDB {
	return &mockSubagentDB{
		conversations: make(map[string]string),
	}
}

func (m *mockSubagentDB) GetOrCreateSubagentConversation(ctx context.Context, slug, parentID, cwd string) (string, string, error) {
	key := parentID + ":" + slug
	if id, ok := m.conversations[key]; ok {
		return id, slug, nil
	}
	id := "subagent-" + slug
	m.conversations[key] = id
	return id, slug, nil
}

// mockSubagentRunner implements SubagentRunner for testing.
type mockSubagentRunner struct {
	response      string
	err           error
	lastModelID   string // Capture for assertions
	lastReasoning string // Capture for assertions
}

func (m *mockSubagentRunner) RunSubagent(ctx context.Context, conversationID, prompt, modelID, reasoning string) (string, error) {
	m.lastModelID = modelID
	m.lastReasoning = reasoning
	if m.err != nil {
		return "", m.err
	}
	return m.response, nil
}

func TestSubagentTool_SanitizeSlug(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"test-slug", "test-slug"},
		{"Test Slug", "test-slug"},
		{"test_slug", "test-slug"},
		{"test--slug", "test-slug"},
		{"-test-slug-", "test-slug"},
		{"test@slug!", "testslug"},
		{"123-abc", "123-abc"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := sanitizeSlug(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeSlug(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSubagentTool_Run(t *testing.T) {
	wd := NewMutableWorkingDir("/tmp")
	db := newMockSubagentDB()
	runner := &mockSubagentRunner{response: "Task completed successfully"}

	tool := &SubagentTool{
		DB:                   db,
		ParentConversationID: "parent-123",
		WorkingDir:           wd,
		Runner:               runner,
	}

	input := subagentInput{
		Slug:   "test-task",
		Prompt: "Do something useful",
	}
	inputJSON, _ := json.Marshal(input)

	result := tool.Tool().Run(t.Context(), inputJSON)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	if len(result.LLMContent) == 0 {
		t.Fatal("expected LLM content")
	}

	if result.LLMContent[0].Text == "" {
		t.Error("expected non-empty response text")
	}

	// Check display data
	if result.Display == nil {
		t.Error("expected display data")
	}
	displayData, ok := result.Display.(SubagentDisplayData)
	if !ok {
		t.Error("display data should be SubagentDisplayData")
	}
	if displayData.Slug != "test-task" {
		t.Errorf("expected slug 'test-task', got %q", displayData.Slug)
	}
}

func TestSubagentTool_Validation(t *testing.T) {
	wd := NewMutableWorkingDir("/tmp")
	db := newMockSubagentDB()
	runner := &mockSubagentRunner{response: "OK"}

	tool := &SubagentTool{
		DB:                   db,
		ParentConversationID: "parent-123",
		WorkingDir:           wd,
		Runner:               runner,
	}

	// Test empty slug
	t.Run("empty slug", func(t *testing.T) {
		input := subagentInput{Slug: "", Prompt: "test"}
		inputJSON, _ := json.Marshal(input)
		result := tool.Tool().Run(t.Context(), inputJSON)
		if result.Error == nil {
			t.Error("expected error for empty slug")
		}
	})

	// Test empty prompt
	t.Run("empty prompt", func(t *testing.T) {
		input := subagentInput{Slug: "test", Prompt: ""}
		inputJSON, _ := json.Marshal(input)
		result := tool.Tool().Run(t.Context(), inputJSON)
		if result.Error == nil {
			t.Error("expected error for empty prompt")
		}
	})

	// Test invalid slug (only special chars)
	t.Run("invalid slug", func(t *testing.T) {
		input := subagentInput{Slug: "@#$%", Prompt: "test"}
		inputJSON, _ := json.Marshal(input)
		result := tool.Tool().Run(t.Context(), inputJSON)
		if result.Error == nil {
			t.Error("expected error for invalid slug")
		}
	})
}

// An omitted model reaches the runner empty, so the runner can keep an
// existing subagent's model rather than force the parent's onto it.
func TestSubagentTool_OmittedModelLeftToRunner(t *testing.T) {
	wd := NewMutableWorkingDir("/tmp")
	db := newMockSubagentDB()
	runner := &mockSubagentRunner{response: "OK"}

	tool := &SubagentTool{
		DB:                   db,
		ParentConversationID: "parent-123",
		WorkingDir:           wd,
		Runner:               runner,
	}

	input := subagentInput{Slug: "test", Prompt: "do something"}
	inputJSON, _ := json.Marshal(input)
	tool.Tool().Run(t.Context(), inputJSON)

	if runner.lastModelID != "" {
		t.Errorf("expected no model, got %q", runner.lastModelID)
	}
}

func TestSubagentTool_ModelOverride(t *testing.T) {
	wd := NewMutableWorkingDir("/tmp")
	db := newMockSubagentDB()
	runner := &mockSubagentRunner{response: "OK"}

	tool := &SubagentTool{
		DB:                   db,
		ParentConversationID: "parent-123",
		WorkingDir:           wd,
		Runner:               runner,
		AvailableModels: []AvailableModel{
			{Name: "claude-sonnet-4-6", ID: "claude-sonnet-4-6"},
			{Name: "claude-haiku-4.5", ID: "claude-haiku-4.5@subscription"},
		},
	}

	// The schema exposes short names only; route-specific IDs stay internal.
	llmTool := tool.Tool()
	schemaJSON, _ := json.Marshal(llmTool.InputSchema)
	schemaStr := string(schemaJSON)
	if strings.Contains(schemaStr, "claude-haiku-4.5@subscription") {
		t.Errorf("schema leaks route-specific model ID: %s", schemaStr)
	}
	for _, model := range tool.AvailableModels {
		if !strings.Contains(schemaStr, fmt.Sprintf("%q", model.Name)) {
			t.Errorf("model %q missing from schema", model.Name)
		}
		if strings.Contains(llmTool.Description, model.Name) {
			t.Errorf("description duplicates model %q from schema", model.Name)
		}
	}

	// The model parameter must stay optional so subagents inherit the parent's
	// model by default.
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(llmTool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(schema.Required, "model") {
		t.Errorf("model must not be required, got required=%v", schema.Required)
	}

	// Choosing a short name runs the model ID it maps to.
	input := subagentInput{Slug: "test", Prompt: "do something", Model: "claude-haiku-4.5"}
	inputJSON, _ := json.Marshal(input)
	tool.Tool().Run(t.Context(), inputJSON)

	if runner.lastModelID != "claude-haiku-4.5@subscription" {
		t.Errorf("expected model 'claude-haiku-4.5@subscription', got %q", runner.lastModelID)
	}
}

func TestSubagentTool_ModelOverride_InvalidModel(t *testing.T) {
	wd := NewMutableWorkingDir("/tmp")
	db := newMockSubagentDB()
	runner := &mockSubagentRunner{response: "OK"}

	tool := &SubagentTool{
		DB:                   db,
		ParentConversationID: "parent-123",
		WorkingDir:           wd,
		Runner:               runner,
		AvailableModels: []AvailableModel{
			{Name: "claude-sonnet-4-6", ID: "claude-sonnet-4-6@subscription"},
			{Name: "claude-haiku-4.5", ID: "claude-haiku-4.5"},
		},
	}

	// Route-specific IDs are not accepted; only enumerated names are.
	for _, model := range []string{"nonexistent-model", "claude-sonnet-4-6@subscription"} {
		input := subagentInput{Slug: "test", Prompt: "do something", Model: model}
		inputJSON, _ := json.Marshal(input)
		result := tool.Tool().Run(t.Context(), inputJSON)
		if result.Error == nil {
			t.Fatalf("expected error for model %q", model)
		}
		msg := result.Error.Error()
		if !strings.Contains(msg, model) {
			t.Errorf("expected error to mention %q, got %v", model, result.Error)
		}
		if !strings.Contains(msg, "available: claude-sonnet-4-6, claude-haiku-4.5") {
			t.Errorf("expected error to list model names, got %v", result.Error)
		}
	}
}

func TestSubagentTool_NoModels(t *testing.T) {
	// When no available models, schema should not have model enum
	tool := &SubagentTool{
		DB:                   newMockSubagentDB(),
		ParentConversationID: "parent-123",
		WorkingDir:           NewMutableWorkingDir("/tmp"),
		Runner:               &mockSubagentRunner{response: "OK"},
	}

	llmTool := tool.Tool()
	schemaJSON, _ := json.Marshal(llmTool.InputSchema)
	schemaStr := string(schemaJSON)
	// The model property is only present when models are available; make sure
	// the model enum specifically is absent (the reasoning enum is always
	// present, so we can't just check for the substring "enum").
	if strings.Contains(schemaStr, `"model"`) {
		t.Errorf("expected no model property in schema when no available models, got %s", schemaStr)
	}
	if strings.Contains(llmTool.Description, "Available models") {
		t.Errorf("expected no model list in description when no available models")
	}
}

func TestSubagentTool_InheritsReasoning(t *testing.T) {
	tool := &SubagentTool{
		DB:                   newMockSubagentDB(),
		ParentConversationID: "parent-123",
		WorkingDir:           NewMutableWorkingDir("/tmp"),
		Runner:               &mockSubagentRunner{response: "OK"},
		ParentReasoning:      "high",
	}

	runner := tool.Runner.(*mockSubagentRunner)
	input := subagentInput{Slug: "test", Prompt: "do something"}
	inputJSON, _ := json.Marshal(input)
	tool.Tool().Run(t.Context(), inputJSON)

	if runner.lastReasoning != "high" {
		t.Errorf("expected inherited reasoning 'high', got %q", runner.lastReasoning)
	}
}

func TestSubagentTool_ReasoningOverride(t *testing.T) {
	tool := &SubagentTool{
		DB:                   newMockSubagentDB(),
		ParentConversationID: "parent-123",
		WorkingDir:           NewMutableWorkingDir("/tmp"),
		Runner:               &mockSubagentRunner{response: "OK"},
		ParentReasoning:      "high",
	}

	// The reasoning enum is always present in the schema.
	schemaJSON, _ := json.Marshal(tool.Tool().InputSchema)
	if !strings.Contains(string(schemaJSON), `"reasoning"`) {
		t.Errorf("expected reasoning property in schema, got %s", schemaJSON)
	}

	runner := tool.Runner.(*mockSubagentRunner)
	input := subagentInput{Slug: "test", Prompt: "do something", Reasoning: "max"}
	inputJSON, _ := json.Marshal(input)
	tool.Tool().Run(t.Context(), inputJSON)

	if runner.lastReasoning != "max" {
		t.Errorf("expected reasoning override 'max', got %q", runner.lastReasoning)
	}
}

func TestSubagentTool_ReasoningOverride_Invalid(t *testing.T) {
	tool := &SubagentTool{
		DB:                   newMockSubagentDB(),
		ParentConversationID: "parent-123",
		WorkingDir:           NewMutableWorkingDir("/tmp"),
		Runner:               &mockSubagentRunner{response: "OK"},
	}

	input := subagentInput{Slug: "test", Prompt: "do something", Reasoning: "turbo"}
	inputJSON, _ := json.Marshal(input)
	result := tool.Tool().Run(t.Context(), inputJSON)
	if result.Error == nil {
		t.Fatal("expected error for invalid reasoning level")
	}
	if !strings.Contains(result.Error.Error(), "turbo") {
		t.Errorf("expected error to mention invalid level, got %v", result.Error)
	}
}
