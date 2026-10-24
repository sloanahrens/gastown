//go:build !windows

package cmd

import (
	"testing"
)

// pid 1 belongs to root, so an ordinary user's probe of it fails with EPERM.
// The process exists and must read as alive. Root's probe succeeds instead,
// which is alive too, so the assertion holds under either uid.
func TestIsProcessRunning_TreatsEPERMAsAlive(t *testing.T) {
	t.Parallel()
	if !isProcessRunning(1) {
		t.Error("isProcessRunning(1) = false; pid 1 exists under another uid, want true")
	}
}
