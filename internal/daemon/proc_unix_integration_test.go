//go:build integration && unix

package daemon

import (
	"os"
	"os/exec"
	"testing"
)

// A child this process has reaped is gone, even though its pid may now belong
// to another process. Run waits for the child, so the *os.Process reports done
// and isProcessAlive must not probe the number again. Integration tier: it
// launches one short-lived child.
func TestIntegrationIsProcessAlive_ReapedChildIsGone(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running child: %v", err)
	}
	if isProcessAlive(cmd.Process) {
		t.Error("isProcessAlive(reaped child) = true, want false")
	}
}

// Pid 1 belongs to root. An unprivileged signal 0 to it fails with EPERM, which
// still means the process exists, so isProcessAlive must report it alive. Root
// may signal any pid, so the probe succeeds there and proves nothing; skip it.
func TestIntegrationIsProcessAlive_OtherUIDIsAlive(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root signals pid 1 without EPERM; the case needs an unprivileged caller")
	}
	p, err := os.FindProcess(1)
	if err != nil {
		t.Fatalf("finding pid 1: %v", err)
	}
	if !isProcessAlive(p) {
		t.Error("isProcessAlive(pid 1 under another uid) = false, want true")
	}
}
