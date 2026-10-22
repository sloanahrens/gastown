package doctor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// failingIdentityLister is a tmux session listing that cannot be read.
type failingIdentityLister struct {
	err error
}

func (f failingIdentityLister) ListSessions() ([]string, error) {
	return nil, f.err
}

func (f failingIdentityLister) ListSessionIDs() (map[string]string, error) {
	return nil, f.err
}

// TestIdentityCollisionRunReportsUnknownOnListingError: when tmux cannot list
// its sessions, a lock whose writer is gone is not reported as a stale lock.
// Its state is unknown, so the check is skipped rather than passed.
func TestIdentityCollisionRunReportsUnknownOnListingError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	runtimeDir := filepath.Join(townRoot, "rig", "worker", ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(lock.LockInfo{PID: 999999999, AcquiredAt: time.Now(), SessionID: "$7"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	check := NewIdentityCollisionCheck()
	check.lister = failingIdentityLister{err: errors.New("tmux list-sessions: permission denied")}
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusSkipped {
		t.Errorf("Status = %v, want %v", result.Status, StatusSkipped)
	}
	if !strings.Contains(result.Message, "lock state unknown") {
		t.Errorf("Message = %q, want it to say the lock state is unknown", result.Message)
	}
	if len(result.Details) != 0 {
		t.Errorf("Details = %v, want none: a failed listing reports no lock as stale or orphaned", result.Details)
	}
}
