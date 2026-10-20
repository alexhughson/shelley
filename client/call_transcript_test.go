package client

import (
	"testing"

	"shelley.exe.dev/db"
	"shelley.exe.dev/db/generated"
)

func TestCallTranscriptAgainstRealServer(t *testing.T) {
	ctx := t.Context()
	database, cc, httpClient, baseURL := newRealServer(t)
	conv, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = database.QueriesTx(ctx, func(q *generated.Queries) error {
		call, err := q.CreateLiveCall(ctx, conv.ConversationID)
		if err != nil {
			return err
		}
		for i, d := range []struct {
			user bool
			text string
		}{{false, "Hi!"}, {true, "Run the"}, {true, " tests."}} {
			err := q.AppendLiveCallTranscript(ctx, generated.AppendLiveCallTranscriptParams{
				ConversationID: conv.ConversationID, Call: call, N: int64(i + 1), User: d.user, Text: d.text,
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fetchCallTranscript(cc, httpClient, baseURL, conv.ConversationID, "1")
	if want := "1 Live: Hi!\n3 User: Run the tests.\n"; err != nil || got != want {
		t.Errorf("transcript = %q, %v; want %q", got, err, want)
	}
}
