package daemon

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// A PID read from a pid file, or found listening on a port, names whatever
// process holds that number now — not necessarily the process that wrote it.
// Pid files outlive their processes and macOS reuses PIDs; ports are held by
// forwarders (Docker Desktop's com.docker.backend holds every published
// container port). Before signaling such a PID we check its command line is
// the process we mean to stop, and refuse otherwise (gt-p7zy0).

// processArgsFn reads a live process's argv (nil if unreadable). A var so
// tests can present any command line; production uses ps(1) via
// doltserver.ProcessArgs, the one ps reader for identity checks. Command-line
// matching here is a safety gate before a signal, not state inference; see
// doltserver.ProcessArgs for why it survives the gt-utuk ZFC cleanup.
var processArgsFn = doltserver.ProcessArgs

// verifyDoltSQLServerFn proves a PID is a dolt sql-server before the Dolt
// manager signals it. A var for the same reason as processArgsFn.
var verifyDoltSQLServerFn = doltserver.VerifyDoltSQLServerPID

// isGTDaemonArgs reports whether argv is `gt daemon run`, the only way the
// daemon is launched (spawnDaemonProcess, the launchd plist, doctor's fix).
// The binary name is not checked: installs and tests name it differently.
func isGTDaemonArgs(args []string) bool {
	return len(args) >= 3 && args[1] == "daemon" && args[2] == "run"
}

// verifyGTDaemonPID returns nil only when pid is a running `gt daemon run`.
// Like the dolt matcher it fails closed: an unreadable argv is refused.
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
	args := processArgsFn(pid)
	if len(args) == 0 {
		return fmt.Errorf("cannot verify PID %d is the gt daemon: command line unreadable", pid)
	}
	if !isGTDaemonArgs(args) {
		return fmt.Errorf("PID %d is not the gt daemon (command: %q)", pid, strings.Join(args, " "))
	}
	return nil
}
