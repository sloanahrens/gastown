package daemon

import (
	"fmt"
	"os"
	"path/filepath"
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

// processCWDFn reads a live process's working directory ("" if unreadable).
// A var for the same reason as processArgsFn.
var processCWDFn = doltserver.ProcessCWD

// verifyDoltSQLServerFn proves a PID is a dolt sql-server serving townRoot
// before the Dolt manager signals it. A var for the same reason as
// processArgsFn.
var verifyDoltSQLServerFn = doltserver.VerifyTownDoltSQLServerPID

// isGTDaemonArgs reports whether argv is `gt daemon run`, the only way the
// daemon is launched (spawnDaemonProcess, the launchd plist, doctor's fix).
// The binary name is not checked: installs and tests name it differently.
func isGTDaemonArgs(args []string) bool {
	return len(args) >= 3 && args[1] == "daemon" && args[2] == "run"
}

// verifyGTDaemonPID returns nil only when pid is a running `gt daemon run`
// for townRoot. Like the dolt matcher it fails closed: an unreadable argv or
// working directory is refused.
//
// argv names no town, so the town is the process's working directory: every
// launch path (spawnDaemonProcess, the launchd plist and systemd unit's
// WorkingDirectory, doctor's fix) starts the daemon in townRoot, and
// `gt daemon run` finds its workspace from there. Without this a reused PID
// that is another town's daemon would be signaled (gt-l9s6f).
//
// On Windows there is no ps(1); the daemon.lock flock is the guard there, as
// it was before this check existed.
func verifyGTDaemonPID(townRoot string, pid int) error {
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
	cwd := processCWDFn(pid)
	if cwd == "" {
		return fmt.Errorf("cannot verify PID %d is this town's gt daemon: working directory unreadable", pid)
	}
	if !pathWithin(cwd, townRoot) {
		return fmt.Errorf("PID %d is a gt daemon for another town (cwd %s, not under %s)", pid, cwd, townRoot)
	}
	return nil
}

// pathWithin reports whether path is dir or below it, comparing real paths:
// lsof and /proc report the resolved cwd, while townRoot may spell a symlink
// (macOS /tmp and /var are links into /private).
func pathWithin(path, dir string) bool {
	realDir := realPath(dir)
	rel, err := filepath.Rel(realDir, realPath(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func realPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}
