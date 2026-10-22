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

// idListingFailingLister lists session names but cannot list their IDs.
type idListingFailingLister struct {
	err error
}

func (f idListingFailingLister) ListSessions() ([]string, error) {
	return nil, nil
}

func (f idListingFailingLister) ListSessionIDs() (map[string]string, error) {
	return nil, f.err
}

// writeIdentityLocks writes an agent.lock for each worker under a new town
// root, keyed by worker name, and returns the town root.
func writeIdentityLocks(t *testing.T, workers map[string]lock.LockInfo) string {
	t.Helper()
	townRoot := t.TempDir()
	for worker, info := range workers {
		runtimeDir := filepath.Join(townRoot, "rig", worker, ".runtime")
		if err := os.MkdirAll(runtimeDir, 0755); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtimeDir, "agent.lock"), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return townRoot
}

// TestIdentityCollisionRunReportsUnknownOnListingError: when tmux cannot list
// its sessions, a lock whose writer is gone is not reported as a stale lock.
// Its state is unknown, so the check is skipped rather than passed.
func TestIdentityCollisionRunReportsUnknownOnListingError(t *testing.T) {
	t.Parallel()
	townRoot := writeIdentityLocks(t, map[string]lock.LockInfo{
		"worker": {PID: 999999999, AcquiredAt: time.Now(), SessionID: "$7"},
	})

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

// TestIdentityCollisionRunReportsUnknownOnIDListingError: a failure listing
// session IDs, after the names listed fine, leaves the lock unknown too.
func TestIdentityCollisionRunReportsUnknownOnIDListingError(t *testing.T) {
	t.Parallel()
	townRoot := writeIdentityLocks(t, map[string]lock.LockInfo{
		"worker": {PID: 999999999, AcquiredAt: time.Now(), SessionID: "$7"},
	})

	check := NewIdentityCollisionCheck()
	check.lister = idListingFailingLister{err: errors.New("tmux list-panes: permission denied")}
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

// TestIdentityCollisionRunJudgesSessionlessLocksOnListingError: a lock that
// names no session needs no listing, so a failed listing still reports it stale,
// while the lock that names a session stays unknown rather than stale.
func TestIdentityCollisionRunJudgesSessionlessLocksOnListingError(t *testing.T) {
	t.Parallel()
	townRoot := writeIdentityLocks(t, map[string]lock.LockInfo{
		"stale-worker": {PID: 999999999, AcquiredAt: time.Now()},
		"held-worker":  {PID: 999999999, AcquiredAt: time.Now(), SessionID: "$7"},
	})

	check := NewIdentityCollisionCheck()
	check.lister = failingIdentityLister{err: errors.New("tmux list-sessions: permission denied")}
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Errorf("Status = %v, want %v", result.Status, StatusWarning)
	}
	if !strings.Contains(result.Message, "1 stale lock(s) found") {
		t.Errorf("Message = %q, want it to report the sessionless lock as stale", result.Message)
	}
	if !strings.Contains(result.Message, "1 lock(s) state unknown") {
		t.Errorf("Message = %q, want it to report the session lock as state unknown", result.Message)
	}
	var staleDetails []string
	for _, d := range result.Details {
		if strings.Contains(d, "stale-worker") {
			staleDetails = append(staleDetails, d)
		}
		if strings.Contains(d, "held-worker") {
			t.Errorf("Details = %v, want held-worker absent: its state is unknown, not stale", result.Details)
		}
	}
	if len(staleDetails) != 1 {
		t.Errorf("Details = %v, want exactly one entry for stale-worker", result.Details)
	}
}
