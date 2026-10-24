//go:build !windows

package beads

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// writeDoltPidFile writes pid to the dolt-server.pid path under beadsDir, the
// path CleanStaleDoltServerPID reads, and returns that path.
func writeDoltPidFile(t *testing.T, beadsDir string, pid int) string {
	t.Helper()
	dir := filepath.Join(beadsDir, "dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "dolt-server.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	return path
}

// TestCleanStaleDoltServerPID_KeepsPidFileOnEPERM: pid 1 belongs to root, so a
// signal 0 probe from any other user returns EPERM. That process is alive, and
// a Dolt server under another uid must keep its pid file.
func TestCleanStaleDoltServerPID_KeepsPidFileOnEPERM(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root can signal pid 1, so the probe never returns EPERM")
	}
	beadsDir := t.TempDir()
	path := writeDoltPidFile(t, beadsDir, 1)

	CleanStaleDoltServerPID(beadsDir)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("pid file of a live process was removed: %v", err)
	}
}

// TestCleanStaleDoltServerPID_RemovesPidFileOfExitedProcess covers the other
// side: a child that has exited and been reaped answers ESRCH, so its pid file
// is stale and must go.
func TestCleanStaleDoltServerPID_RemovesPidFileOfExitedProcess(t *testing.T) {
	t.Parallel()
	child := exec.Command("sh", "-c", "exit 0")
	if err := child.Run(); err != nil {
		t.Fatalf("run child: %v", err)
	}
	beadsDir := t.TempDir()
	path := writeDoltPidFile(t, beadsDir, child.Process.Pid)

	CleanStaleDoltServerPID(beadsDir)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pid file of an exited process still exists (stat error: %v)", err)
	}
}
