// Package mcp connects Shelley to Streamable HTTP MCP servers. The Shelley
// server owns the sessions; `shelley mcp` and the UI reach them through its
// HTTP API.
package mcp

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// Server is a registered MCP server.
type Server struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Description string            `json:"description"`
	Headers     map[string]string `json:"headers"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Names can't contain dots, so that SERVER.TOOL splits at the first one.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Validate reports whether s is a usable configuration.
func (s Server) Validate() error {
	if !nameRE.MatchString(s.Name) {
		return fmt.Errorf("invalid name %q: use letters, digits, _ and -, starting with a letter or digit", s.Name)
	}
	if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid URL %q: must be http or https", s.URL)
	}
	seen := map[string]bool{}
	for k, v := range s.Headers {
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) {
			return fmt.Errorf("invalid header %q", k)
		}
		if seen[http.CanonicalHeaderKey(k)] {
			return fmt.Errorf("duplicate header %q", k)
		}
		seen[http.CanonicalHeaderKey(k)] = true
	}
	return nil
}

// OAuthCapable reports whether s can log in with OAuth: it has no static
// Authorization header.
func (s Server) OAuthCapable() bool {
	for k := range s.Headers {
		if strings.EqualFold(k, "Authorization") {
			return false
		}
	}
	return true
}

// sameConnection reports whether sessions with s and o are interchangeable.
func (s Server) sameConnection(o Server) bool {
	return s.URL == o.URL && maps.Equal(s.Headers, o.Headers)
}

// sameLoginScope reports whether an OAuth login for o still applies to s. A
// login belongs to the authorization server that protects an origin, so
// editing only the path or query of a URL (a common way to pick which tools a
// server exposes) keeps the login; changing scheme or host drops it.
func (s Server) sameLoginScope(o Server) bool {
	so, ok1 := origin(s.URL)
	oo, ok2 := origin(o.URL)
	if !ok1 || !ok2 {
		return s.URL == o.URL
	}
	return so == oo
}

// origin returns rawURL's scheme://host:port with the scheme and host
// lowercased and the default port for the scheme filled in, so that
// "https://h" and "https://H:443" compare equal. ok is false if rawURL can't
// be parsed as an absolute http(s) URL.
func origin(rawURL string) (o string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port, true
}

// SameLoginScope reports whether an OAuth login made for the URL oldURL still
// applies after the URL changes to newURL: it does when only the path or query
// changed, not the scheme or host.
func SameLoginScope(oldURL, newURL string) bool {
	return Server{URL: newURL}.sameLoginScope(Server{URL: oldURL})
}
