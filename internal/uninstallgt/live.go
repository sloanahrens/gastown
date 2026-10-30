package uninstallgt

import (
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

// DetectTown returns the town a run belongs to: $GT_TOWN_ROOT when it names a
// town, else the outermost town above selfDir, else the conventional $HOME/gt
// (gt-bqzoq).
//
// selfDir is the running binary's own directory, the only tie an install
// outside the town has to one, and a $GT_TOWN_ROOT without the town marker is
// no town rather than an error, so a caller can point a run at a scratch tree.
func DetectTown(getenv func(string) string, selfDir string, signal0 func(pid int) bool) Town {
	return detectTown(getenv, selfDir, installer.IsTownRoot, signal0)
}

// detectTown is DetectTown with the town marker check injected, so a test reads
// a tree it built rather than whatever sits above its temporary directory.
func detectTown(getenv func(string) string, selfDir string, isTown func(string) bool, signal0 func(pid int) bool) Town {
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
func DaemonRunning(town string, signal0 func(pid int) bool) bool {
	if town == "" || signal0 == nil {
		return false
	}
	pid := daemonPID(town)
	return pid > 0 && signal0(pid)
}

// daemonPID reads the pid an install's daemon writes as the first line of its
// pid file ("PID\nNONCE", internal/daemon/pidfile.go). A pid is never 0 or
// negative, and a signal-0 probe of one of those names the caller's own
// process group, so anything that is not a positive pid reads as no daemon.
func daemonPID(town string) int {
	data, err := os.ReadFile(filepath.Join(town, "daemon", "daemon.pid"))
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(string(data), "\n")
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, line)
	pid, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return pid
}

// Signal0 reports whether a process named by pid exists: the read-only probe
// the daemon check makes, which cannot disturb the process.
func Signal0(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
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
