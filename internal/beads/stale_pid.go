package beads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// CleanStaleDoltServerPID removes the dolt-server.pid file inside a beads
// directory if the referenced process is no longer alive. A stale PID file
// causes bd to connect to port 3307 (configured in the co-located config.yaml),
// which may be occupied by a different Dolt server serving different databases.
// The resulting connection hangs until the bd read timeout (30s) kills it.
//
// This is a defensive measure — the Dolt server writes dolt-server.pid on
// startup but does not always clean it up on crash or unclean shutdown.
func CleanStaleDoltServerPID(beadsDir string) {
	pidPath := filepath.Join(beadsDir, "dolt", "dolt-server.pid")
	data, err := os.ReadFile(pidPath) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		return // No PID file, nothing to clean
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		// Corrupt PID file — remove it
		_ = os.Remove(pidPath)
		return
	}

	// A Dolt server running under another uid answers EPERM to signal 0. It is
	// alive, and its pid file must stay, so only a process that is gone counts.
	if pidAlive(pid) {
		return
	}

	_ = os.Remove(pidPath)
	fmt.Fprintf(os.Stderr, "Cleaned stale dolt-server.pid (PID %d) from %s\n", pid, beadsDir)
}

// pidAlive reports whether pid names a live process. A pid this process may not
// signal still exists, so EPERM counts as alive. A non-positive pid is never a
// process: signal 0 to pid 0 or -1 addresses a process group or every process,
// so it is refused before any probe.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	sigErr := p.Signal(syscall.Signal(0))
	return sigErr == nil || errors.Is(sigErr, syscall.EPERM)
}
