//go:build !windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

// A held daemon.lock with no readable pid is a daemon that is starting (the
// lock is taken before the pid file is written) or a damaged pid file. Either
// way a live process holds the lock, so StopDaemon must say so and leave the
// lock file in place: unlinking it lets the next gt up lock a fresh inode and
// start a second daemon beside the first (gt-dicyp).
func TestStopDaemonKeepsHeldLockWhenPIDUnknown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "daemon.lock")
	holder := flock.New(lockPath)
	if locked, err := holder.TryLock(); err != nil || !locked {
		t.Fatalf("test could not take the daemon lock: locked=%v err=%v", locked, err)
	}
	defer holder.Unlock()

	err := StopDaemon(townRoot)
	if err == nil {
		t.Fatal("StopDaemon reported success although a live process holds the lock")
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Errorf("error = %v, want it to tell the caller to retry", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Errorf("daemon.lock was removed while held: %v", statErr)
	}
	// A second lock attempt must still fail: the holder is still the owner.
	probe := flock.New(lockPath)
	if got, _ := probe.TryLock(); got {
		_ = probe.Unlock()
		t.Error("a second process could take the lock after StopDaemon")
	}
}
