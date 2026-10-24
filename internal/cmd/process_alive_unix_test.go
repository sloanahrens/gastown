//go:build !windows

package cmd

import (
	"testing"
)

// pid 1 belongs to root, so an ordinary user's probe of it fails with EPERM.
// The process exists and must read as alive. Root's probe succeeds instead,
// which is alive too, so the assertions hold under either uid.
func TestProcessAliveTreatsEPERMAsAlive(t *testing.T) {
	t.Parallel()
	if !processAlive(1) {
		t.Error("processAlive(1) = false; pid 1 exists under another uid, want true")
	}
	if got, want := processAlive(1), isProcessRunning(1); got != want {
		t.Errorf("processAlive(1) = %v, isProcessRunning(1) = %v; the two must agree", got, want)
	}
}
