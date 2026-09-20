package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// daemonSupervisor is the service manager a provisioned daemon runs under
// (gt daemon enable-supervisor). Starting the daemon by hand while one of
// these is loaded is the trap gt-3jrm closes: the hand-started process
// holds daemon.lock, and the KeepAlive job then respawns every ~10 s, loses
// the flock and exits 1 for as long as the manual daemon lives —
// runDaemonEnableSupervisor refuses to provision in that state for the same
// reason, and start has to honor the same rule from the other side.
type daemonSupervisor struct {
	name  string   // "launchd" / "systemd", for messages
	start []string // argv that (re)starts the daemon through it
	load  string   // how to load the job when start says it is not loaded
}

// Seams for tests: supervisor detection, running its command, the direct
// spawn, and the running check. Production values are the real ones.
var (
	supervisorPlistPath = templates.LaunchdPlistPath
	supervisorUnitPath  = templates.SystemdUnitPath
	supervisorRun       = func(argv []string) error {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed argv from detectDaemonSupervisor
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	spawnDaemonDirect = spawnDaemonProcess
	daemonIsRunning   = daemon.IsRunning
	supervisorGOOS    = runtime.GOOS
)

// detectDaemonSupervisor reports the supervisor provisioned FOR townRoot, or
// nil when the daemon is meant to be run by hand (no plist / unit file
// installed) or the installed one belongs to another workspace: the file is
// per user, and its WorkingDirectory is the town it was provisioned for —
// kickstarting it from a different town would restart that town's daemon
// and then wait for this town's lock in vain. A file that exists but cannot
// be read is an error, never "no supervisor": the whole point of the check
// is to never hand-spawn beside a provisioned one.
func detectDaemonSupervisor(townRoot string) (*daemonSupervisor, error) {
	switch supervisorGOOS {
	case "darwin":
		p, err := supervisorPlistPath()
		if err != nil {
			// No resolvable home means nowhere a plist could have been
			// provisioned; same reading as templates.SupervisorStatus.
			return nil, nil
		}
		isFor, err := supervisorFileIsFor(p, townRoot)
		if err != nil || !isFor {
			return nil, err
		}
		return &daemonSupervisor{
			name:  "launchd",
			start: []string{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/com.gastown.daemon", os.Getuid())},
			load:  fmt.Sprintf("launchctl bootstrap gui/%d %s", os.Getuid(), p),
		}, nil
	case "linux":
		p, err := supervisorUnitPath()
		if err != nil {
			return nil, nil
		}
		isFor, err := supervisorFileIsFor(p, townRoot)
		if err != nil || !isFor {
			return nil, err
		}
		return &daemonSupervisor{
			name:  "systemd",
			start: []string{"systemctl", "--user", "restart", "gastown-daemon.service"},
			load:  "systemctl --user daemon-reload && systemctl --user enable --now gastown-daemon.service",
		}, nil
	}
	return nil, nil
}

// supervisorFileIsFor reports whether the plist / unit at path names
// townRoot as its WorkingDirectory (both templates render it as
// "<string>{{.TownRoot}}</string>" / "WorkingDirectory={{.TownRoot}}"). An
// absent file is (false, nil); a present but unreadable one is an error.
func supervisorFileIsFor(path, townRoot string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking supervisor file %s: %w", path, err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // fixed per-user path from templates
	if err != nil {
		return false, fmt.Errorf("reading supervisor file %s: %w", path, err)
	}
	// The town may be reached by a different spelling than the one the
	// file was rendered with (a symlinked path, GT_TOWN_ROOT vs a cwd walk;
	// /tmp vs /private/tmp on macOS), and a miss here means a hand spawn
	// beside a loaded job. Compare the recorded WorkingDirectory as a path,
	// symlinks resolved, not as a string.
	recorded := supervisorWorkingDirectory(string(data))
	if recorded == "" {
		// Present but not in the shape the templates render (hand-edited,
		// older format): still a provisioned supervisor as far as launchd
		// or systemd is concerned, so refusing beats a hand spawn beside it.
		return false, fmt.Errorf("supervisor file %s has no WorkingDirectory; cannot tell which town it serves — fix or remove it", path)
	}
	return samePath(recorded, townRoot), nil
}

// supervisorWorkingDirectory pulls the WorkingDirectory out of a rendered
// plist ("<key>WorkingDirectory</key>\n<string>X</string>") or unit
// ("WorkingDirectory=X"); "" when neither form is present.
func supervisorWorkingDirectory(body string) string {
	if i := strings.Index(body, "<key>WorkingDirectory</key>"); i >= 0 {
		rest := body[i:]
		if a := strings.Index(rest, "<string>"); a >= 0 {
			if b := strings.Index(rest[a:], "</string>"); b >= 0 {
				return strings.TrimSpace(rest[a+len("<string>") : a+b])
			}
		}
		return ""
	}
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "WorkingDirectory="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// samePath reports whether two paths name the same directory once cleaned
// and with symlinks resolved; a path that cannot be resolved compares by
// its cleaned spelling.
func samePath(a, b string) bool {
	canon := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Clean(r)
		}
		return filepath.Clean(p)
	}
	return canon(a) == canon(b)
}

// startDaemon starts the daemon for townRoot: through the provisioned
// supervisor when there is one, directly otherwise. It reports how the
// daemon was started ("launchd", "systemd" or "" for a direct spawn) and its
// PID; callers print. When the supervisor command fails the daemon is NOT
// spawned by hand — a job that is still loaded would respawn against the
// manual daemon forever, the very loop this path exists to prevent — the
// error says how to load the job, or that removing the file returns the
// daemon to manual operation.
func startDaemon(townRoot string) (via string, pid int, err error) {
	running, pid, err := daemonIsRunning(townRoot)
	if err != nil {
		return "", 0, fmt.Errorf("checking daemon status: %w", err)
	}
	if running {
		return "", pid, fmt.Errorf("daemon already running (PID %d)", pid)
	}
	sup, err := detectDaemonSupervisor(townRoot)
	if err != nil {
		return "", 0, err
	}
	if sup != nil {
		if runErr := supervisorRun(sup.start); runErr != nil {
			// The command may have failed after starting the daemon.
			if pid, err := waitForDaemon(townRoot); err == nil {
				return sup.name, pid, nil
			}
			return sup.name, 0, fmt.Errorf("%s is provisioned for this town but could not start the daemon: %v\n  load it with: %s\n  or remove its file to run the daemon by hand", sup.name, runErr, sup.load)
		}
		pid, err = waitForDaemon(townRoot)
		if err != nil {
			return sup.name, 0, fmt.Errorf("daemon did not come up under %s: %w", sup.name, err)
		}
		return sup.name, pid, nil
	}
	pid, err = spawnDaemonDirect(townRoot)
	return "", pid, err
}

// waitForDaemon polls the lock for up to 3 s and returns the daemon's PID.
func waitForDaemon(townRoot string) (int, error) {
	for range 30 {
		time.Sleep(100 * time.Millisecond)
		running, pid, err := daemonIsRunning(townRoot)
		if err != nil {
			return 0, fmt.Errorf("checking daemon status: %w", err)
		}
		if running {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("daemon failed to start (check logs with 'gt daemon logs')")
}
