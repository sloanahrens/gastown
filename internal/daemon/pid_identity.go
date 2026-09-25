package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// A PID read from a pid file, or found listening on a port, names whatever
// process holds that number now — not necessarily the process that wrote it.
// Pid files outlive their processes and macOS reuses PIDs; ports are held by
// forwarders (Docker Desktop's com.docker.backend holds every published
// container port). Before signaling such a PID we check its command line is
// the process we mean to stop, and refuse otherwise (gt-p7zy0).

// processArgsFn reads a live process's argv. A var so tests can present any
// command line; production uses ps(1).
var processArgsFn = processArgs

// verifyDoltSQLServerFn proves a PID is a dolt sql-server before the Dolt
// manager signals it. A var for the same reason as processArgsFn.
var verifyDoltSQLServerFn = doltserver.VerifyDoltSQLServerPID

func processArgs(pid int) ([]string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output() //nolint:gosec // G204: numeric pid, fixed args
	if err != nil {
		return nil, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return nil, fmt.Errorf("no process %d", pid)
	}
	return fields, nil
}

// isGTDaemonArgs reports whether argv is `gt daemon run`, the only way the
// daemon is launched (spawnDaemonProcess, the launchd plist, doctor's fix).
// The binary name is not checked: installs and tests name it differently.
func isGTDaemonArgs(args []string) bool {
	return len(args) >= 3 && args[1] == "daemon" && args[2] == "run"
}

// verifyGTDaemonPID returns nil only when pid is a running `gt daemon run`.
// On Windows there is no ps(1); the daemon.lock flock is the guard there, as
// it was before this check existed.
func verifyGTDaemonPID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid PID %d", pid)
	}
	if pid == os.Getpid() {
		return fmt.Errorf("PID %d is this process", pid)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	args, err := processArgsFn(pid)
	if err != nil {
		return fmt.Errorf("cannot verify PID %d is the gt daemon: %w", pid, err)
	}
	if !isGTDaemonArgs(args) {
		return fmt.Errorf("PID %d is not the gt daemon (command: %q)", pid, strings.Join(args, " "))
	}
	return nil
}
