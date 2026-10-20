package test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"shelley.exe.dev/claudetool"
	"shelley.exe.dev/db"
	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/predictable"
	"shelley.exe.dev/models"
	"shelley.exe.dev/server"
	"shelley.exe.dev/server/notifications"
)

type fakeLLMManager struct {
	service *predictable.Service
}

func (m *fakeLLMManager) GetService(modelID string) (llm.Service, error) {
	return m.service, nil
}

func (m *fakeLLMManager) GetAvailableModels() []string {
	return []string{"predictable"}
}

func (m *fakeLLMManager) GetWorkhorseService(modelID string) (llm.Service, error) {
	return m.GetService(modelID)
}

func (m *fakeLLMManager) HasModel(modelID string) bool {
	return modelID == "predictable"
}

func (m *fakeLLMManager) GetModelInfo(modelID string) *models.ModelInfo {
	return nil
}

func (m *fakeLLMManager) RefreshCustomModels() error {
	return nil
}

// recordingChannel is a notifications.Channel that records all events sent to it.
type recordingChannel struct {
	mu     sync.Mutex
	events []notifications.Event
}

func (r *recordingChannel) Name() string { return "test-recorder" }

func (r *recordingChannel) Send(_ context.Context, event notifications.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *recordingChannel) eventsForConversation(conversationID string) []notifications.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notifications.Event
	for _, e := range r.events {
		if e.ConversationID == conversationID {
			out = append(out, e)
		}
	}
	return out
}

func setupTestServerForSubagent(t *testing.T) (*server.Server, *db.DB, *httptest.Server, *predictable.Service) {
	t.Helper()

	// Create temporary database
	tempDB := t.TempDir() + "/test.db"
	database, err := db.New(db.Config{DSN: tempDB})
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	// Run migrations
	if err := database.Migrate(t.Context()); err != nil {
		t.Fatalf("Failed to migrate database: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Use predictable model
	predictableService := predictable.NewService()
	llmManager := &fakeLLMManager{service: predictableService}

	toolSetConfig := claudetool.ToolSetConfig{
		WorkingDir:    t.TempDir(),
		EnableBrowser: false,
	}

	svr := server.NewServer(database, llmManager, toolSetConfig, logger, true, "predictable", "")

	mux := http.NewServeMux()
	svr.RegisterRoutes(mux)
	testServer := httptest.NewServer(mux)
	t.Cleanup(testServer.Close)

	return svr, database, testServer, predictableService
}

// TestSubagentWorkingStateNotification tests that subagent working state changes
// are properly notified via the SSE stream.
func TestSubagentWorkingStateNotification(t *testing.T) {
	// This test would verify that when a subagent starts/stops working,
	// the parent conversation's stream receives a ConversationState update.
	// Currently we just document this should work via publishConversationState.
	t.Skip("Skipping - requires more infrastructure to trigger working state changes")
}

// TestSubagentNoExternalNotification verifies that subagent conversations do NOT
// trigger external notification channels (email/Discord/ntfy) when they finish,
// while regular conversations DO.
func TestSubagentNoExternalNotification(t *testing.T) {
	svr, database, testServer, _ := setupTestServerForSubagent(t)
	defer testServer.Close()

	ctx := t.Context()

	// Register a recording notification channel
	recorder := &recordingChannel{}
	svr.RegisterNotificationChannel(recorder)

	// Create parent conversation and run it to completion
	parentSlug := "parent-convo"
	parentConv, err := database.CreateConversation(ctx, &parentSlug, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("Failed to create parent conversation: %v", err)
	}

	// Create subagent conversation
	subSlug := "sub-worker"
	subConv, err := database.CreateSubagentConversation(ctx, subSlug, parentConv.ConversationID, nil)
	if err != nil {
		t.Fatalf("Failed to create subagent conversation: %v", err)
	}

	// Run the subagent to completion
	subagentRunner := server.NewSubagentRunner(svr)
	_, err = subagentRunner.RunSubagent(ctx, subConv.ConversationID, "Test prompt", "predictable", "")
	if err != nil {
		t.Fatalf("RunSubagent failed: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for svr.IsAgentWorking(subConv.ConversationID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if svr.IsAgentWorking(subConv.ConversationID) {
		t.Fatal("subagent did not finish")
	}

	// Subagent finished — should NOT have dispatched to external channels
	subagentEvents := recorder.eventsForConversation(subConv.ConversationID)
	if len(subagentEvents) != 0 {
		t.Errorf("Expected 0 external notifications for subagent, got %d", len(subagentEvents))
	}

	// Now run the parent conversation (a regular, non-subagent conversation)
	parentURL := testServer.URL + "/api/conversation/" + parentConv.ConversationID + "/chat"
	req, _ := http.NewRequest("POST", parentURL, strings.NewReader(`{"message":"hello","model":"predictable"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}
	resp.Body.Close()

	// Wait for the parent conversation to finish working
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		parentEvents := recorder.eventsForConversation(parentConv.ConversationID)
		if len(parentEvents) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	parentEvents := recorder.eventsForConversation(parentConv.ConversationID)
	if len(parentEvents) == 0 {
		t.Error("Expected external notification for regular (non-subagent) conversation, got none")
	}

	// Double-check: subagent still has zero
	subagentEvents = recorder.eventsForConversation(subConv.ConversationID)
	if len(subagentEvents) != 0 {
		t.Errorf("Subagent should still have 0 external notifications, got %d", len(subagentEvents))
	}
}
