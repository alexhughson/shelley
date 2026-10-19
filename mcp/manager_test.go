package mcp

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestManager returns a Manager whose sessions connect in memory to a
// server with no tools, and counts its connections.
func newTestManager(t *testing.T) (*Manager, *int) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "s"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "t"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{}, nil, nil
	})
	m := NewManager(nil, nil)
	t.Cleanup(m.Close)
	connects := 0
	m.transport = func(Server, auth.OAuthHandler) sdk.Transport {
		connects++
		st, ct := sdk.NewInMemoryTransports()
		if _, err := srv.Connect(context.Background(), st, nil); err != nil {
			t.Fatal(err)
		}
		return ct
	}
	return m, &connects
}

func use(t *testing.T, m *Manager, cfg Server) {
	if _, err := m.ListTools(t.Context(), cfg, "c"); err != nil {
		t.Fatal(err)
	}
}

func TestIdleSessionsClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestManager(t)
		sessions := func(wait time.Duration) int {
			time.Sleep(wait)
			synctest.Wait()
			return len(m.Snapshot().Sessions)
		}
		use(t, m, Server{Name: "s"})
		if n := sessions(idleTimeout - time.Second); n != 1 {
			t.Fatalf("%d sessions before the idle timeout", n)
		}
		use(t, m, Server{Name: "s"})
		if n := sessions(idleTimeout - time.Second); n != 1 {
			t.Fatalf("%d sessions after using it again", n)
		}
		if n := sessions(time.Second); n != 0 {
			t.Fatalf("%d sessions after the idle timeout", n)
		}
		if e := m.Snapshot().Events[0]; e.Kind != "session_closed" || e.Message != "idle" {
			t.Fatalf("last event %+v", e)
		}
	})
}

// A request with a server's new configuration doesn't use a session with
// the old one.
func TestReconfiguredSessionReconnects(t *testing.T) {
	m, connects := newTestManager(t)
	use(t, m, Server{Name: "s", URL: "http://a"})
	use(t, m, Server{Name: "s", URL: "http://a", Description: "edited"})
	use(t, m, Server{Name: "s", URL: "http://a", Headers: map[string]string{"K": "v"}})
	if *connects != 2 {
		t.Fatalf("%d connections, want 2", *connects)
	}
}
