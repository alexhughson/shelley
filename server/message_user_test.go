package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"shelley.exe.dev/claudetool"
	"shelley.exe.dev/db"
	"shelley.exe.dev/llm"
)

func TestMessageUserToolIsOptIn(t *testing.T) {
	t.Parallel()
	c := newCompactTestConversation(t, db.ConversationOptions{})
	c.turn("hello")
	if hasTool(c.ps.GetLastRequest(), claudetool.MessageUserName) || c.systemCardHasTool(claudetool.MessageUserName) {
		t.Fatal("message_user offered without being enabled")
	}
}

func TestMessageUserOnlyForTopLevel(t *testing.T) {
	t.Parallel()
	for _, role := range []conversationRole{roleSubagent, roleBtwReader, roleWorker} {
		if f := (&ConversationManager{role: role}).userMessageFinder(); f != nil {
			t.Errorf("role %d gets a user message finder", role)
		}
	}
}

// lastMessageUserResult returns the newest tool result and the id of its row.
func (c *compactTestConversation) lastMessageUserResult() (rowID string, result llm.Content) {
	c.t.Helper()
	for _, m := range listMessages(c.t, c.database, c.id) {
		if m.LlmData == nil {
			continue
		}
		var msg llm.Message
		if err := json.Unmarshal([]byte(*m.LlmData), &msg); err != nil {
			c.t.Fatal(err)
		}
		for _, ct := range msg.Content {
			if ct.Type == llm.ContentTypeToolResult {
				rowID, result = m.MessageID, ct
			}
		}
	}
	if rowID == "" {
		c.t.Fatal("no tool result")
	}
	return rowID, result
}

func messageUserDisplay(t *testing.T, result llm.Content) claudetool.MessageUserDisplay {
	t.Helper()
	b, err := json.Marshal(result.Display)
	if err != nil {
		t.Fatal(err)
	}
	var d claudetool.MessageUserDisplay
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMessageUserEndToEnd(t *testing.T) {
	t.Parallel()
	c := newCompactTestConversation(t, db.ConversationOptions{ToolOverrides: map[string]string{claudetool.MessageUserName: "on"}})
	c.turn("echo: first   message\nfrom the user")
	c.turn("echo: second message")
	if !hasTool(c.ps.GetLastRequest(), claudetool.MessageUserName) || !c.systemCardHasTool(claudetool.MessageUserName) {
		t.Fatal("message_user not offered, or missing from the system prompt card")
	}
	var firstID string
	var firstSeq, secondSeq int64
	for _, m := range listMessages(t, c.database, c.id) {
		if m.Type == string(db.MessageTypeUser) && m.LlmData != nil && strings.Contains(*m.LlmData, "first   message") {
			firstID = m.MessageID
			firstSeq = m.SequenceID
		}
		if m.Type == string(db.MessageTypeUser) && m.LlmData != nil && strings.Contains(*m.LlmData, "second message") {
			secondSeq = m.SequenceID
		}
	}

	// A reply and reaction resolve the sequence ID to the message's ID.
	c.turn(fmt.Sprintf(`message_user: {"text":"Got it","reply_to":%d,"reaction":"👍"}`, firstSeq))
	_, result := c.lastMessageUserResult()
	if result.ToolError {
		t.Fatalf("reply failed: %s", result.ToolResult[0].Text)
	}
	if want := `Sent, in response to the user's message "echo: first message from the user", and reacted 👍 to it.`; result.ToolResult[0].Text != want {
		t.Fatalf("result = %q, want %q", result.ToolResult[0].Text, want)
	}
	if d := messageUserDisplay(t, result); d.TargetMessageID != firstID || d.TargetSequenceID != firstSeq || d.TargetExcerpt != "echo: first message from the user" {
		t.Fatalf("display = %+v, want target %s", d, firstID)
	}

	// Unknown sequence IDs and the agent's own messages are refused.
	for _, seq := range []int64{1 << 62, firstSeq + 1} {
		in, _ := json.Marshal(claudetool.MessageUserInput{Text: "x", ReplyTo: seq})
		c.turn("message_user: " + string(in))
		if _, result := c.lastMessageUserResult(); !result.ToolError || !strings.Contains(result.ToolResult[0].Text, "no user message has sequence_id") {
			t.Fatalf("sequence %d: want refusal, got %+v", seq, result)
		}
	}
	c.turn(fmt.Sprintf(`message_user: {"reply_to":%d,"reaction":"🎉"}`, secondSeq))
	if _, result := c.lastMessageUserResult(); result.ToolError || result.ToolResult[0].Text != `Reacted 🎉 to the user's message "echo: second message".` {
		t.Fatalf("reaction: %+v", result)
	}
	c.turn(`message_user: {"reaction":"👍"}`)
	if _, result := c.lastMessageUserResult(); !result.ToolError {
		t.Fatal("reaction without a target accepted")
	}
	c.turn(fmt.Sprintf(`message_user: {"reply_to":%d,"reaction":"ok"}`, secondSeq))
	if _, result := c.lastMessageUserResult(); !result.ToolError {
		t.Fatal("non-emoji reaction accepted")
	}

	// Attachments are validated, listed in the display, and served by the
	// attachment endpoint; nothing else is.
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.txt")
	png := filepath.Join(dir, "pic.png")
	other := filepath.Join(dir, "other.txt")
	for path, data := range map[string]string{notes: "hello notes", png: "\x89PNG\r\n\x1a\n0000", other: "secret"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in, _ := json.Marshal(claudetool.MessageUserInput{Attachments: []string{filepath.Join(dir, "missing.txt")}})
	c.turn("message_user: " + string(in))
	if _, result := c.lastMessageUserResult(); !result.ToolError {
		t.Fatal("missing attachment accepted")
	}
	in, _ = json.Marshal(claudetool.MessageUserInput{Text: "files", Attachments: []string{notes, png, notes}})
	c.turn("message_user: " + string(in))
	rowID, result := c.lastMessageUserResult()
	if result.ToolError {
		t.Fatalf("attachments failed: %s", result.ToolResult[0].Text)
	}
	d := messageUserDisplay(t, result)
	if len(d.Attachments) != 2 || d.Attachments[0].Path != notes || d.Attachments[0].Name != "notes.txt" || d.Attachments[0].Size != 11 || d.Attachments[1].Path != png {
		t.Fatalf("attachments = %+v", d.Attachments)
	}

	mux := http.NewServeMux()
	c.srv.RegisterRoutes(mux)
	get := func(messageID, path, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/message/"+messageID+"/attachment?path="+url.QueryEscape(path)+query, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	if w := get(rowID, notes, ""); w.Code != http.StatusOK || w.Body.String() != "hello notes" ||
		w.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("notes: %d %v %q", w.Code, w.Header(), w.Body.String())
	}
	if w := get(rowID, png, ""); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("png: %d %v", w.Code, w.Header())
	}
	if w := get(rowID, png, "&download=1"); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("png download: %d %v", w.Code, w.Header())
	}
	if w := get(rowID, other, ""); w.Code != http.StatusNotFound {
		t.Fatalf("unlisted file served: %d", w.Code)
	}
	if w := get(firstID, notes, ""); w.Code != http.StatusNotFound {
		t.Fatalf("attachment served for another message: %d", w.Code)
	}

	// end_turn ends the turn without another LLM request.
	c.turn(`message_user: {"text":"bye","end_turn":true}`)
	if _, result := c.lastMessageUserResult(); result.ToolError {
		t.Fatalf("end_turn failed: %s", result.ToolResult[0].Text)
	}
	last := c.ps.GetLastRequest().Messages
	if m := last[len(last)-1]; m.Role != llm.MessageRoleUser || !strings.Contains(m.Content[0].Text, `"bye"`) {
		t.Fatalf("the model was called after end_turn:\n%s", requestDump(c.ps.GetLastRequest()))
	}
}

// Sequence lookup matches only typed messages, including compaction copies.
func TestFindUserMessageBySequence(t *testing.T) {
	database, _ := setupTestDB(t)
	ctx := t.Context()
	conv, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	carried := map[string]string{"compaction_carried": "true"}
	add := func(typ db.MessageType, userData any, content ...llm.Content) string {
		m, err := database.CreateMessage(ctx, db.CreateMessageParams{
			ConversationID: conv.ConversationID,
			Type:           typ,
			LLMData:        llm.Message{Role: llm.MessageRoleUser, Content: content},
			UserData:       userData,
		})
		if err != nil {
			t.Fatal(err)
		}
		return m.MessageID
	}
	text := func(s string) llm.Content { return llm.Content{Type: llm.ContentTypeText, Text: s} }
	original := add(db.MessageTypeUser, nil, text("Deploy the app"))
	add(db.MessageTypeUser, map[string]string{"sender_conversation_id": "child"}, text("Deploy report"))
	add(db.MessageTypeUser, nil, llm.Content{Type: llm.ContentTypeToolResult, ToolUseID: "t1"}, text("Deploy result"))
	add(db.MessageTypeUser, carried, text("Deploy the app"))
	carriedOnly := add(db.MessageTypeUser, carried, text("Carried only"))
	add(db.MessageTypeAgent, nil, text("Deploy agent text"))

	f := userMessageFinder{db: database, conversationID: conv.ConversationID}
	typed, err := database.ListTypedUserMessages(ctx, conv.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(typed) != 3 {
		t.Fatalf("typed user messages = %d, want original, carried copy, and carried-only", len(typed))
	}
	seen := make(map[string]bool)
	for _, row := range typed {
		seen[row.MessageID] = true
		m, ok, err := f.FindUserMessageBySequence(ctx, row.SequenceID)
		if err != nil || !ok || m.ID != row.MessageID {
			t.Fatalf("FindUserMessageBySequence(%d) = %q, %v, %v; want %q", row.SequenceID, m.ID, ok, err, row.MessageID)
		}
	}
	if !seen[original] || !seen[carriedOnly] {
		t.Fatalf("typed user messages omitted original or carried-only: %v", seen)
	}
	for _, row := range listMessages(t, database, conv.ConversationID) {
		if seen[row.MessageID] {
			continue
		}
		if m, ok, err := f.FindUserMessageBySequence(ctx, row.SequenceID); err != nil || ok {
			t.Fatalf("machine/agent sequence %d matched %q: %v", row.SequenceID, m.ID, err)
		}
	}
	if m, ok, err := f.FindUserMessageBySequence(ctx, 1<<62); err != nil || ok {
		t.Fatalf("unknown sequence matched %q: %v", m.ID, err)
	}
}

// After a compaction and a fork, which copies only the compacted generation,
// FindUserMessageBySequence finds the copy of a typed message, under its original's
// sequence_id, and not the carried squish summary nor a carried notice.
func TestFindUserMessageAcrossCompactionAndFork(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := NewTestHarness(t)
		defer stopActiveConversationLoops(h.server)
		h.NewConversation("echo: ALPHA_TURN", "")
		h.WaitResponse()
		synctest.Wait()
		h.Chat("echo: BETA_TURN")
		h.WaitResponse()
		synctest.Wait()
		squishFirstTurn(t, h.server, h.db, h.convID, "SQUISHED_ALPHA")
		ctx := t.Context()
		if _, err := h.db.CreateMessage(ctx, db.CreateMessageParams{
			ConversationID: h.convID,
			Type:           db.MessageTypeUser,
			LLMData:        llm.UserStringMessage("JOB_DONE"),
			UserData:       map[string]any{"background_job_id": "job1", "command": "true"},
		}); err != nil {
			t.Fatal(err)
		}
		rows := compactGeneration(t, h)
		if !strings.Contains(llmDataText(rows), "JOB_DONE") {
			t.Fatal("the notice was not carried")
		}

		f := userMessageFinder{db: h.db, conversationID: h.convID}
		var betaSeq int64
		typed, err := h.db.ListTypedUserMessages(ctx, h.convID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range typed {
			if m.LlmData != nil && strings.Contains(*m.LlmData, "BETA_TURN") && m.UserData == nil {
				betaSeq = m.SequenceID
			}
		}
		if betaSeq == 0 {
			t.Fatal("missing BETA user message")
		}
		beta, ok, err := f.FindUserMessageBySequence(ctx, betaSeq)
		if err != nil || !ok {
			t.Fatalf("FindUserMessageBySequence(BETA) = %v, %v", ok, err)
		}
		var note string
		for _, m := range rows {
			if m.UserData != nil && strings.Contains(*m.UserData, `"squish_note"`) {
				msg, err := parseLLMData(m.LlmData)
				if err != nil {
					t.Fatal(err)
				}
				note = userMessageText(*msg)
			}
		}
		if !strings.Contains(note, "SQUISHED_ALPHA") {
			t.Fatal("no carried squish note")
		}

		latest, err := h.db.GetLatestActionableMessage(ctx, h.convID)
		if err != nil {
			t.Fatal(err)
		}
		fork, err := h.db.ForkConversation(ctx, h.convID, latest.SequenceID)
		if err != nil {
			t.Fatal(err)
		}
		for _, convID := range []string{h.convID, fork.ConversationID} {
			f := userMessageFinder{db: h.db, conversationID: convID}
			for _, row := range listMessages(t, h.db, convID) {
				if row.UserData == nil || (!strings.Contains(*row.UserData, `"squish_note"`) && !strings.Contains(*row.UserData, `"background_job_id"`)) {
					continue
				}
				if m, ok, err := f.FindUserMessageBySequence(ctx, row.SequenceID); ok || err != nil {
					t.Errorf("%s: machine notice %d matched %s (err %v)", convID, row.SequenceID, m.ID, err)
				}
			}
			m, ok, err := f.FindUserMessageBySequence(ctx, beta.SequenceID)
			if err != nil || !ok {
				t.Fatalf("%s: FindUserMessageBySequence(BETA) = %v, %v", convID, ok, err)
			}
			if m.SequenceID != beta.SequenceID || (convID == fork.ConversationID) == (m.ID == beta.ID) {
				t.Errorf("%s: found %s at %d; original %s at %d", convID, m.ID, m.SequenceID, beta.ID, beta.SequenceID)
			}
		}
	})
}
