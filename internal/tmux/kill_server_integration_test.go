//go:build integration && !windows

package tmux

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// TestIntegrationKillServerUnlinksSocketFile is the gt-20di fix at its source:
// tmux leaves the socket file behind when a server exits, so a test package
// that kills its server at the end of every run added one file forever.
// KillServer now clears the file too.
func TestIntegrationKillServerUnlinksSocketFile(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	socket := constants.TestSocketName("gt-test-killsrv")
	socketPath := socketPathForTest(t, socket)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("pre-clean %s: %v", socketPath, err)
	}

	tm := NewTmuxWithSocket(socket)
	if err := tm.NewSessionWithCommand("gt-test-killsrv-1", ".", "sleep 300"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("socket file missing while the server is up: %v", err)
	}

	if err := tm.KillServer(); err != nil {
		t.Fatalf("KillServer: %v", err)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file survived KillServer: %v", err)
	}
}
