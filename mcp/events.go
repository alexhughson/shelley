package mcp

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"
)

const maxEvents = 200

// Event is something that happened to a server, a session or a login, for
// /debug/mcp. It never holds arguments, results, tokens or header values.
type Event struct {
	Time    time.Time
	Server  string
	Scope   string
	Kind    string // connected, connect_failed, session_closed, login_*, token_refreshed, refresh_failed, logged_out
	Message string
}

// events is a ring of the last maxEvents events.
type events struct {
	mu   sync.Mutex
	ring []Event
}

func (e *events) add(server, scope, kind, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ring = append(e.ring, Event{time.Now(), server, scope, kind, message})
	if len(e.ring) > maxEvents {
		e.ring = e.ring[1:]
	}
}

// Snapshot is the Manager's state, for /debug/mcp.
type Snapshot struct {
	Sessions []SessionInfo // by server, then scope
	Events   []Event       // newest first
}

// SessionInfo describes a session.
type SessionInfo struct {
	Server, Scope string
	ID            string    // the MCP session ID, "" while connecting or if the server set none
	InFlight      int       // requests using the session
	IdleSince     time.Time // zero while in use
}

// Snapshot returns the current sessions and recent events.
func (m *Manager) Snapshot() Snapshot {
	var snap Snapshot
	m.mu.Lock()
	for k, s := range m.sessions {
		info := SessionInfo{Server: k.server, Scope: k.scope, InFlight: s.users}
		if s.users == 0 {
			info.IdleSince = s.lastUsed
		}
		select {
		case <-s.ready:
			info.ID = s.id
		default:
		}
		snap.Sessions = append(snap.Sessions, info)
	}
	m.mu.Unlock()
	slices.SortFunc(snap.Sessions, func(a, b SessionInfo) int {
		return cmp.Or(strings.Compare(a.Server, b.Server), strings.Compare(a.Scope, b.Scope))
	})
	m.events.mu.Lock()
	snap.Events = slices.Clone(m.events.ring)
	m.events.mu.Unlock()
	slices.Reverse(snap.Events)
	return snap
}
