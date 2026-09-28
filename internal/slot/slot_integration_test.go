//go:build integration

package slot

import (
	"os/exec"
	"runtime"
	"testing"
)

// TestIntegrationProcessGone_ReapedChild pins processGone's one "true" — the
// answer that licenses deleting a pid's containers — against a real child that
// has exited and been reaped.
func TestIntegrationProcessGone_ReapedChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("processGone has no certain probe on windows and always answers false by design (owner_process_windows.go)")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("cannot run true: %v", err)
	}
	if !processGone(cmd.Process.Pid) {
		t.Errorf("processGone(%d) = false for a child that exited and was reaped", cmd.Process.Pid)
	}
}
