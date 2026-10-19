package mcp

import "testing"

func TestSameLoginScope(t *testing.T) {
	cases := []struct {
		old, new string
		same     bool
	}{
		{"https://example.com/mcp", "https://example.com/mcp", true},
		{"https://example.com/mcp", "https://example.com/mcp?tools=all", true},
		{"https://example.com/mcp", "https://example.com/other", true},
		{"https://example.com/mcp", "https://EXAMPLE.com/mcp", true},     // host compared case-insensitively
		{"https://example.com/mcp", "https://example.com:443/mcp", true}, // default port filled in
		{"http://example.com/mcp", "http://example.com:80/mcp", true},
		{"https://example.com/mcp", "https://other.com/mcp", false},
		{"https://example.com/mcp", "http://example.com/mcp", false}, // scheme change
		{"https://example.com/mcp", "https://example.com:8443/mcp", false},
		{"://bad", "://bad", true}, // unparsable, equal strings
		{"://bad", "://worse", false},
	}
	for _, c := range cases {
		if got := SameLoginScope(c.old, c.new); got != c.same {
			t.Errorf("SameLoginScope(%q, %q) = %v, want %v", c.old, c.new, got, c.same)
		}
	}
}
