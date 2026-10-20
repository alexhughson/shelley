package server

import (
	"context"
	"fmt"

	"shelley.exe.dev/db/generated"
)

const (
	liveContextMessages  = 6   // user requests and final assistant replies kept
	liveContextTailRows  = 100 // rows scanned (tool rows included) to find them
	liveContextItemRunes = 800 // per-item cap; keeps well under Live's input limit
)

// liveHistoryItem is one prior text turn in Live's session.input format.
type liveHistoryItem struct {
	Type    string            `json:"type"`
	Role    string            `json:"role"`
	Content []liveHistoryText `json:"content"`
}

type liveHistoryText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// liveContext returns only the user's typed messages and final backend
// replies. Tool results, subagent reports, background jobs and other machine
// notices must not enter the Live session.
func (s *Server) liveContext(ctx context.Context, conversationID string) (items []liveHistoryItem, lastSeq int64, err error) {
	var rows []generated.Message
	err = s.db.Queries(ctx, func(q *generated.Queries) error {
		var err error
		rows, err = q.ListMessagesTail(ctx, generated.ListMessagesTailParams{ConversationID: conversationID, Limit: liveContextTailRows})
		return err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("load live context: %w", err)
	}
	items = make([]liveHistoryItem, 0, liveContextMessages)
	for _, row := range rows {
		lastSeq = row.SequenceID
		if row.ExcludedFromContext {
			continue
		}
		user, text, _, err := liveDialogue(row)
		if err != nil {
			return nil, 0, err
		}
		if text == "" {
			continue
		}
		role, contentType := "assistant", "output_text"
		if user {
			role, contentType = "user", "input_text"
		}
		items = append(items, liveHistoryItem{Type: "message", Role: role, Content: []liveHistoryText{{Type: contentType, Text: truncateRunes(text, liveContextItemRunes)}}})
	}
	return items[max(0, len(items)-liveContextMessages):], lastSeq, nil
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// liveSituation is the bounded current state line for Live's instructions.
func (s *Server) liveSituation(conversation *generated.Conversation) string {
	state := "idle"
	if conversation.AgentWorking {
		state = "working"
	}
	situation := "Backend state: " + state + "."
	if cwd := derefString(conversation.Cwd); cwd != "" {
		situation += " Working directory: " + truncateRunes(cwd, 300) + "."
	}
	model := derefString(conversation.Model)
	if model == "" {
		model = s.effectiveDefaultModel(s.getModelList())
	}
	return situation + " Backend model: " + truncateRunes(model, 100) + "."
}
