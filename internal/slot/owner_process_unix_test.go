//go:build !windows

package slot

import (
	"os"
	"testing"
)

// processGone is the one probe whose "true" deletes containers, so it is
// pinned against real processes: this one (alive), pid 1 (alive, and owned by
// root, so EPERM for an ordinary user), and non-positive pids. The "exited and
// reaped" case needs a child process: see
// TestIntegrationProcessGone_ReapedChild.
func TestProcessGone(t *testing.T) {
	t.Parallel()
	if processGone(os.Getpid()) {
		t.Error("processGone(self) = true")
	}
	if processGone(1) {
		t.Error("processGone(1) = true: a process this user cannot signal is not gone")
	}
	if processGone(0) || processGone(-1) {
		t.Error("processGone of a non-positive pid = true")
	}
}
