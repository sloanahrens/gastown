//go:build !windows

package beads

import (
	"os"
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

// noSuchPID is above the largest pid any kernel hands out (pid_max is at most
// 4194304 on Linux and 99999 on macOS), so no process can hold it and the probe
// answers ESRCH. A reaped child's pid is not used, because the kernel may reuse
// it before the probe runs.
const noSuchPID = 1<<31 - 1

// TestCleanStaleDoltServerPID_RemovesPidFileOfAbsentProcess covers the other
// side: a pid with no process behind it answers ESRCH, so its pid file is stale
// and must go.
func TestCleanStaleDoltServerPID_RemovesPidFileOfAbsentProcess(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	path := writeDoltPidFile(t, beadsDir, noSuchPID)

	CleanStaleDoltServerPID(beadsDir)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pid file of an absent process still exists (stat error: %v)", err)
	}
}

// TestCleanStaleDoltServerPID_RemovesPidFileOfNonPositivePID: pid 0 and -1 are
// not processes. Signal 0 to either addresses a group or every process, so the
// file must go without a probe ever reporting it alive.
func TestCleanStaleDoltServerPID_RemovesPidFileOfNonPositivePID(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1} {
		beadsDir := t.TempDir()
		path := writeDoltPidFile(t, beadsDir, pid)

		CleanStaleDoltServerPID(beadsDir)

		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("pid file holding %d still exists (stat error: %v)", pid, err)
		}
	}
}
