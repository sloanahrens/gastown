//go:build linux

package polecat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// LockSeatPick refuses when the lock path is a directory (gt-xqynlp). This holds only on
// Linux: gofrs/flock opens the path with O_CREATE, and Linux fails O_CREAT on an existing
// directory with EISDIR. macOS opens the directory and flocks it, so the lock is returned there.
func TestLockSeatPickRefusesWhenTheLockPathIsADirectory(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, ".runtime", "locks", "seat-pick.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := newTestManager(&rig.Rig{Name: "myrig", Path: rigPath}, newWorld(), nil, newNoDatabaseDB()).LockSeatPick()
	if err == nil {
		unlock()
		t.Fatal("LockSeatPick returned no error for a directory at the lock path")
	}
}
