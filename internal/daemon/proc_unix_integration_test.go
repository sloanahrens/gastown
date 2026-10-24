//go:build integration && unix

package daemon

import (
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
