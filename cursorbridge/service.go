package cursorbridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shelley.exe.dev/llm"
)

var requestCounter atomic.Uint64

func newRequestID() string {
	return fmt.Sprintf("req-%d-%d", time.Now().UnixMilli(), requestCounter.Add(1))
}

// NodeVersionRequired is the minimum Node version the SDK supports.
const nodeVersionRequired = "22.13"

// Service implements llm.Service backed by the Cursor TypeScript SDK.
// A Service value is shared by all conversations using the same Shelley model
// ID; it multiplexes concurrent requests over one long-lived daemon process.
type Service struct {
	// APIKey is the Cursor API key (user or service account). Required.
	APIKey string
	// ModelID is the Cursor model id (e.g. "composer-2.5"). Required.
	ModelID string
	// ModelParams are Cursor model parameters (e.g. effort=high, fast=true)
	// passed as ModelSelection.params to the SDK. Optional.
	ModelParams []ModelParam
	// DisplayName is used in Provider() strings; defaults to ModelID.
	DisplayName string
	// NodeBin is the node executable; "" means "node" from PATH.
	NodeBin string
	// DaemonScript is daemon.mjs; "" uses the vendored checkout copy.
	DaemonScript string
	// Logger receives bridge diagnostics; defaults to slog.Default().
	Logger *slog.Logger

	mu       sync.Mutex // guards the fields below
	proc     *daemonProcess
	startErr error
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Service) Provider() string { return "cursor" }

// SupportsImages reports whether the service accepts image inputs.
func (s *Service) SupportsImages() bool { return true }

// MaxImageDimension returns the maximum allowed image dimension.
func (s *Service) MaxImageDimension() int { return 8000 }

// MaxImageBytes returns the maximum allowed image size in bytes.
func (s *Service) MaxImageBytes() int { return 8 * 1024 * 1024 }

// SupportsReasoning reports that reasoning controls are not applicable:
// the Cursor agent picks its own reasoning behavior.
func (s *Service) SupportsReasoning() bool { return false }

// SupportedReasoningLevels returns nil (not applicable).
func (s *Service) SupportedReasoningLevels() []llm.ThinkingLevel { return nil }

// DefaultReasoningLevel reports "" — the provider picks its own default.
func (s *Service) DefaultReasoningLevel() string { return "" }

// PatchProfile returns the flat profile: Cursor composer models behave like
// Claude for patch tooling purposes.
func (s *Service) PatchProfile() string { return "flat" }

// Do runs one Shelley LLM round against a durable Cursor agent.
// It returns at the next loop boundary: StopReasonToolUse (Shelley runs the
// tool) or StopReasonEndTurn. Conversation id comes from ctx.
func (s *Service) Do(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if strings.TrimSpace(s.APIKey) == "" {
		return nil, errors.New("cursor bridge: CURSOR_API_KEY is not set")
	}
	if strings.TrimSpace(s.ModelID) == "" {
		return nil, errors.New("cursor bridge: no model configured")
	}
	proc, err := s.daemon(ctx)
	if err != nil {
		return nil, err
	}
	return proc.do(ctx, s, req)
}

// daemon returns the shared daemon process, starting it (and Node) lazily.
func (s *Service) daemon(ctx context.Context) (*daemonProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != nil && s.proc.alive() {
		return s.proc, nil
	}
	p, err := s.startDaemonLocked(ctx)
	if err != nil {
		s.startErr = err
		// Daemon start failures are persistent configuration problems (node
		// missing, SDK not installed) except transient spawn errors; retry on
		// the next request either way by clearing proc.
		s.proc = nil
		return nil, err
	}
	s.proc = p
	return p, nil
}

func (s *Service) startDaemonLocked(ctx context.Context) (*daemonProcess, error) {
	node := s.NodeBin
	if node == "" {
		node = "node"
	}
	if err := checkNodeVersion(ctx, node); err != nil {
		return nil, fmt.Errorf("cursor bridge: %w (the Cursor SDK requires Node >= %s; install with \"uvx nodeenv -n lts ~/node\")", err, nodeVersionRequired)
	}
	script := s.DaemonScript
	var dir string
	if script == "" {
		// Use the vendored daemon + node_modules from the checkout at
		// ~/.config/shelley/shelley-customization when present; otherwise the
		// embedded single-file script (requires @cursor/sdk resolvable from CWD).
		if d := daemonVendoredDir(); d != "" {
			script = filepath.Join(d, "daemon.mjs")
			dir = d
		}
	} else {
		dir = filepath.Dir(script)
	}
	if script == "" {
		return nil, errors.New("cursor bridge: no daemon script available")
	}
	s.logger().Info("Starting Cursor SDK bridge daemon", "node", node, "script", script)
	cmd := exec.Command(node, script)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "CURSOR_API_KEY="+s.APIKey)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cursor bridge: start daemon: %w", err)
	}
	p := &daemonProcess{
		svc:      s,
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		reqs:     make(map[string]chan *daemonLine),
		sessions: make(map[string]*liveSession),
	}
	go p.readLoop()
	go p.stderrLoop(stderr)
	// Handshake is independent of the request ctx so a cancelled first Do
	// does not kill the shared daemon.
	pingCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.ping(pingCtx); err != nil {
		p.kill()
		return nil, fmt.Errorf("cursor bridge: daemon handshake failed: %w", err)
	}
	return p, nil
}

func checkNodeVersion(ctx context.Context, node string) error {
	out, err := exec.CommandContext(ctx, node, "--version").Output()
	if err != nil {
		return fmt.Errorf("node unavailable: %w", err)
	}
	v := strings.TrimSpace(string(out))
	if !strings.HasPrefix(v, "v") {
		return fmt.Errorf("unexpected node version output %q", v)
	}
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("unexpected node version output %q", v)
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	if major < 22 || (major == 22 && minor < 13) {
		return fmt.Errorf("node %s is too old", v)
	}
	return nil
}

func daemonVendoredDir() string {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	d := filepath.Join(home, ".config", "shelley", "shelley-customization", "cursorbridge", "daemon")
	if st, err := os.Stat(filepath.Join(d, "daemon.mjs")); err == nil && !st.IsDir() {
		return d
	}
	return ""
}
