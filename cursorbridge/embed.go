package cursorbridge

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

//go:embed daemon/daemon.mjs daemon/package.json daemon/package-lock.json
var daemonFS embed.FS

const (
	daemonScriptName = "daemon.mjs"
	daemonPkgName    = "package.json"
	daemonLockName   = "package-lock.json"
)

func extractEmbeddedDaemon() (string, error) {
	script, err := daemonFS.ReadFile("daemon/" + daemonScriptName)
	if err != nil {
		return "", fmt.Errorf("cursor bridge: embed daemon.mjs: %w", err)
	}
	pkg, err := daemonFS.ReadFile("daemon/" + daemonPkgName)
	if err != nil {
		return "", fmt.Errorf("cursor bridge: embed package.json: %w", err)
	}
	lock, err := daemonFS.ReadFile("daemon/" + daemonLockName)
	if err != nil {
		return "", fmt.Errorf("cursor bridge: embed package-lock.json: %w", err)
	}
	sum := sha256.New()
	sum.Write(script)
	sum.Write(lock)
	dir := filepath.Join(os.TempDir(), "shelley-cursor-bridge", hex.EncodeToString(sum.Sum(nil)[:16]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cursor bridge: daemon cache dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, daemonScriptName), script, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, daemonPkgName), pkg, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, daemonLockName), lock, 0o644); err != nil {
		return "", err
	}
	modDir := filepath.Join(dir, "node_modules")
	if st, err := os.Stat(modDir); err != nil || !st.IsDir() {
		cmd := exec.Command("npm", "ci", "--omit=dev")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("cursor bridge: npm ci: %w\n%s", err, out)
		}
	}
	return dir, nil
}
