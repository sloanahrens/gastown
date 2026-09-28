//go:build integration

package lock

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/tmux"
)

// TestIntegrationCleanStaleLocksUsesTownSocket pins that CleanStaleLocks asks
// the town's tmux server (tmux.GetDefaultSocket), not tmux's default one. A
// lock whose PID is dead but whose session lives on the town socket belongs
// to a live agent; listing the wrong server made stale-lock cleanup delete
// it. The lock names the session by its $N id, as agent locks do.
//
// Not parallel: it sets the tmux package's process-wide default socket.
func TestIntegrationCleanStaleLocksUsesTownSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux is required for this integration test: %v", err)
	}
	socket := constants.TestSocketName("gt-test-lock")
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	out, err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "lockprobe", "-P", "-F", "#{session_id}").Output()
	if err != nil {
		t.Fatalf("starting tmux session on %s: %v", socket, err)
	}
	sessionID := strings.TrimSpace(string(out))
	if !strings.HasPrefix(sessionID, "$") {
		t.Fatalf("session id = %q, want $N", sessionID)
	}

	prev := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket(socket)
	t.Cleanup(func() { tmux.SetDefaultSocket(prev) })

	root := t.TempDir()
	runtimeDir := filepath.Join(root, "worker", ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(LockInfo{PID: 999999999, AcquiredAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(runtimeDir, "agent.lock")
	if err := os.WriteFile(lockPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cleaned, err := CleanStaleLocks(root)
	if err != nil {
		t.Fatalf("CleanStaleLocks: %v", err)
	}
	if cleaned != 0 {
		t.Errorf("CleanStaleLocks cleaned %d locks, want 0: session %s is live on the town socket", cleaned, sessionID)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lock of a live town-socket session was removed: %v", err)
	}
}
