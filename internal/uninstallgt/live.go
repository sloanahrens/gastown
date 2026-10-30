package uninstallgt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/steveyegge/gastown/internal/installer"
)

// Town is the town a run belongs to, and its daemon's liveness.
type Town struct {
	// Root is the town root; empty means no town was found.
	Root string
	// DaemonRunning reports whether Root's daemon answered its pid-file probe.
	DaemonRunning bool
}

// DetectTown returns the town a run belongs to: $GT_TOWN_ROOT when it carries
// the town marker, else the outermost town above selfDir, else the
// conventional $HOME/gt (gt-bqzoq).
//
// A set $GT_TOWN_ROOT stands in for the search above selfDir entirely, so a
// value without the marker — absent, a file, an ordinary directory — is no
// error but a run pointed at a scratch tree; it then falls through to the same
// $HOME/gt fallback as a host with no value at all, and that fallback counts
// only when its own marker is there. selfDir is the running binary's own
// directory, the only tie an install outside the town has to one.
func DetectTown(getenv func(string) string, selfDir string, signal0 func(pid int) error) Town {
	return detectTown(getenv, selfDir, installer.IsTownRoot, signal0)
}

// detectTown is DetectTown with the town marker check injected, so a test reads
// a tree it built rather than whatever sits above its temporary directory.
func detectTown(getenv func(string) string, selfDir string, isTown func(string) bool, signal0 func(pid int) error) Town {
	town := ""
	if named := getenv("GT_TOWN_ROOT"); named != "" {
		if named = filepath.Clean(named); isTown(named) {
			town = named
		}
	} else if selfDir != "" {
		town = installer.TownRootAbove(selfDir, isTown)
	}
	if town == "" {
		if home := getenv("HOME"); home != "" {
			if conventional := filepath.Join(home, "gt"); isTown(conventional) {
				town = conventional
			}
		}
	}
	return Town{Root: town, DaemonRunning: DaemonRunning(town, signal0)}
}

// DaemonRunning reports whether town's daemon is alive, asking signal0 about
// the pid its pid file names; no town, no pid file or no pid is no daemon.
func DaemonRunning(town string, signal0 func(pid int) error) bool {
	if town == "" || signal0 == nil {
		return false
	}
	// daemonPID already answers 0 for anything unusable, so a probe is made
	// only for a pid the check may signal.
	pid := daemonPID(town)
	if pid == 0 {
		return false
	}
	return probeAlive(signal0(pid))
}

// daemonPID reads the pid an install's daemon writes as the first line of its
// pid file ("PID\nNONCE", internal/daemon/pidfile.go). The line is trimmed and
// parsed whole: a pid with a sign, any other character, or a value of 0 or
// less is not a pid this check may probe, so it reads as no daemon. Probing 0
// or a negative would name the caller's own process group rather than a
// daemon.
func daemonPID(town string) int {
	data, err := os.ReadFile(filepath.Join(town, "daemon", "daemon.pid"))
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(string(data), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// Signal0 returns the error a signal-0 probe of pid reports: nil when the
// process exists, so the caller tells "exists" from "gone" itself. The probe
// is read-only and cannot disturb the process it names.
func Signal0(pid int) error {
	return signal0(os.FindProcess, pid)
}

// signal0 is Signal0 with the process lookup injected. os.FindProcess answers
// without an error on every host this town runs on, so the lookup failure is
// reachable only through the seam, and its error must still reach the caller:
// probeAlive then counts the daemon as running.
func signal0(find func(int) (*os.Process, error), pid int) error {
	proc, err := find(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.Signal(0))
}

// probeAlive maps a signal-0 probe's error to liveness, and only "no such
// process" reads as gone: EPERM says the process exists under another user,
// and any other error leaves liveness unreadable, so both count as running. A
// daemon the check cannot rule out must not make the live install look safe.
func probeAlive(err error) bool {
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone)
}

// CheckLive returns the notice a forced run prints, or the refusal it returns
// without --force, when dir is the live install of t (gt-acdfp).
func (t Town) CheckLive(dir, home string, force bool) ([]string, error) {
	if !t.live(dir, home) {
		return nil, nil
	}
	if !force {
		return nil, &Refusal{Reason: t.refusal(dir)}
	}
	notice := []string{fmt.Sprintf("%s: FORCE=1 — removing the live install of the town at %s", Program, t.Root)}
	if t.DaemonRunning {
		notice = append(notice, Program+": the daemon is still running; it will fail the moment it next uses bd")
	}
	return notice, nil
}

// live reports whether dir is t's live install: the canonical directory, so a
// symlinked alias of $HOME/.local/bin is refused the same as the directory
// itself. A host with no town has no live install, and a directory beside the
// live one is a scratch install a run may touch.
func (t Town) live(dir, home string) bool {
	if t.Root == "" || home == "" {
		return false
	}
	return canonicalPath(dir) == canonicalPath(filepath.Join(home, ".local", "bin"))
}

// refusal is the reason the run returns in place of removing the live install:
// what the removal would take down, and the two ways to go on.
func (t Town) refusal(dir string) string {
	lines := []string{
		fmt.Sprintf("%s is the live install of the town at %s", dir, t.Root),
		fmt.Sprintf("  it would remove %s and %s, which every agent on this host",
			filepath.Join(dir, "gt"), filepath.Join(dir, "bd")),
		"  resolves off PATH — gt mail, gt escalate and the daemon's own escalation",
		"  all shell out to bd, so the town loses its escalation path with them (gt-acdfp).",
	}
	if t.DaemonRunning {
		lines = append(lines, "  the daemon is running now: stop it first with 'gt daemon stop'.")
	}
	lines = append(lines,
		"  to remove them anyway, say so: make uninstall FORCE=1",
		"  to exercise "+Program+" safely, point it at a scratch directory:",
		`    `+Program+` --install-dir "$(mktemp -d)"`,
	)
	return strings.Join(lines, "\n")
}
