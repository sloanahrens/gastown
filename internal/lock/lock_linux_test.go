//go:build linux

package lock

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/procid"
)

// TestLockInfoIsStaleReadsTheRealProcessTable pins the table IsStale reads in
// production: procid.StartToken's answer for a live pid, from /proc. Only Linux
// provides a start token, so this file builds only there. A host that cannot
// read this process's own token fails the test; there is no skip, because a
// skipped case would pass without proving the wiring (gt-dj2lv4).
func TestLockInfoIsStaleReadsTheRealProcessTable(t *testing.T) {
	t.Parallel()
	pid := os.Getpid()
	token, ok := procid.StartToken(pid)
	if !ok {
		t.Fatalf("procid.StartToken(%d) could not read this process's start token", pid)
	}

	own := LockInfo{PID: pid, Start: token}
	if own.IsStale() {
		t.Error("IsStale() = true for a lock carrying this process's own real start token")
	}
	stranger := LockInfo{PID: pid, Start: "1700000000.12"}
	if !stranger.IsStale() {
		t.Error("IsStale() = false for this live pid carrying a stranger's start token")
	}
	dead := LockInfo{PID: 999999999}
	if !dead.IsStale() {
		t.Error("IsStale() = false for a pid with no process")
	}
}
