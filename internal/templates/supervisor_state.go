package templates

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// LaunchdLabel is the launchd job label the rendered plist carries.
const LaunchdLabel = "com.gastown.daemon"

// SystemdUnit is the unit name the rendered systemd unit file carries.
const SystemdUnit = "gastown-daemon.service"

// SupervisorReader reads the live state of the supervisor job of the given
// kind ("launchd" / "systemd"); SupervisorJobState in production.
type SupervisorReader func(kind string) SupervisorState

// SupervisorState is what the service manager is doing with the supervisor
// job, the live counterpart of SupervisorStatus, which reads only whether the
// plist / unit file is installed: a provisioned job that is not the process
// holding daemon.lock reads as supervised while it crash-loops behind that
// daemon, losing the flock and exiting non-zero every ThrottleInterval
// (gt-sq9e).
type SupervisorState struct {
	Kind     string // "launchd", "systemd", or "" where no supervisor exists
	Loaded   bool   // the service manager knows the job
	PID      int    // the process the job runs; 0 when it has none
	Runs     int    // how many times the service manager has spawned it
	LastExit int    // exit status of its last run; -1 when unreported
	Err      error  // the job's state could not be read; not the same as not loaded
}

// SupervisorFilePath returns the path of the supervisor file this host would
// use and the kind it belongs to ("launchd" / "systemd"), or ("", "") on a
// host with no supported supervisor. The file need not exist.
func SupervisorFilePath() (path, kind string) {
	switch runtime.GOOS {
	case "darwin":
		if p, err := LaunchdPlistPath(); err == nil {
			return p, "launchd"
		}
	case "linux":
		if p, err := SystemdUnitPath(); err == nil {
			return p, "systemd"
		}
	}
	return "", ""
}

// SupervisorJobState asks the service manager what it is doing with the
// supervisor job of the given kind. A job the manager does not know comes
// back Loaded=false with no error — an ordinary state, since the file can be
// installed without being bootstrapped — while a probe that failed for any
// other reason comes back as Err, because status renders the two differently.
func SupervisorJobState(kind string) SupervisorState {
	switch kind {
	case "launchd":
		argv := []string{"launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), LaunchdLabel)}
		return probeSupervisorJob(kind, argv, parseLaunchdPrint, launchdJobIsMissing)
	case "systemd":
		argv := []string{"systemctl", "--user", "show",
			"-p", "LoadState", "-p", "MainPID", "-p", "NRestarts", "-p", "ExecMainStatus", SystemdUnit}
		return probeSupervisorJob(kind, argv, parseSystemctlShow, systemdJobIsMissing)
	}
	return SupervisorState{Kind: kind, LastExit: -1}
}

// SupervisorStatusLine renders the "Supervised:" value for the daemon holding
// lockPID (0 when none does) in the town at townRoot: "none" where no
// supervisor serves this town, otherwise the kind, with a parenthetical
// whenever the supervisor is not the process holding that lock. read may be
// nil.
func SupervisorStatusLine(townRoot string, lockPID int, read SupervisorReader) string {
	kind := SupervisorStatus()
	if kind == "none" {
		return "none"
	}
	if read == nil {
		read = SupervisorJobState
	}
	path, _ := SupervisorFilePath()
	isFor, err := SupervisorFileIsFor(path, townRoot)
	switch {
	case err != nil:
		// Installed, but it does not say which town it serves. "supervised"
		// is the one reading that must not be guessed here.
		return fmt.Sprintf("%s (unverified - %v)", kind, err)
	case !isFor:
		return "none" // installed for another town; this one has no supervisor
	}
	return read(kind).StatusLine(lockPID)
}

// StatusLine renders the "Supervised:" value for a daemon holding lockPID;
// the bare kind means the supervisor's own process is that daemon, and every
// other reading names what the supervisor is doing instead.
func (s SupervisorState) StatusLine(lockPID int) string {
	switch {
	case s.Kind == "" || s.Kind == "none":
		return "none"
	case s.Err != nil:
		return fmt.Sprintf("%s (unverified - %v)", s.Kind, s.Err)
	case !s.Loaded:
		if lockPID == 0 {
			return fmt.Sprintf("%s (the job is not loaded)", s.Kind)
		}
		return fmt.Sprintf("%s (DETACHED - the job is not loaded; PID %d was started by hand)", s.Kind, lockPID)
	case lockPID != 0 && s.PID == lockPID:
		return s.Kind
	case lockPID != 0 && s.PID == 0:
		return fmt.Sprintf("%s (DETACHED - %s is running no daemon while PID %d holds the lock%s)",
			s.Kind, s.Kind, lockPID, s.evidence())
	case lockPID != 0:
		return fmt.Sprintf("%s (DETACHED - %s runs PID %d, not the daemon holding the lock (PID %d)%s)",
			s.Kind, s.Kind, s.PID, lockPID, s.evidence())
	case s.PID != 0:
		return fmt.Sprintf("%s (pid %d%s)", s.Kind, s.PID, s.evidence())
	case s.Runs == 0:
		return fmt.Sprintf("%s (loaded, not started yet)", s.Kind)
	default:
		return fmt.Sprintf("%s (FAILING - loaded, running no daemon%s)", s.Kind, s.evidence())
	}
}

// evidence renders the spawn count and last exit status the service manager
// reported, as ", runs=N, last exit code=X".
func (s SupervisorState) evidence() string {
	var b strings.Builder
	if s.Runs > 0 {
		fmt.Fprintf(&b, ", runs=%d", s.Runs)
	}
	if s.LastExit >= 0 {
		fmt.Fprintf(&b, ", last exit code=%d", s.LastExit)
	}
	return b.String()
}

// probeSupervisorJob runs argv and parses its output. missing decides whether
// a failed run means the manager does not know the job, which is not an error.
func probeSupervisorJob(kind string, argv []string, parse func(string) (bool, int, int, int), missing func(string, error) bool) SupervisorState {
	out, err := supervisorProbeRun(argv)
	if err != nil {
		if missing(out, err) {
			return SupervisorState{Kind: kind, LastExit: -1}
		}
		return SupervisorState{Kind: kind, LastExit: -1,
			Err: fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(out))}
	}
	loaded, pid, runs, lastExit := parse(out)
	return SupervisorState{Kind: kind, Loaded: loaded, PID: pid, Runs: runs, LastExit: lastExit}
}

// supervisorProbeRun is the seam tests replace to answer a probe without
// running the host's service manager.
var supervisorProbeRun = func(argv []string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed argv from SupervisorJobState
	return string(out), err
}

// parseLaunchdPrint reads 'launchctl print' output, which reports the live
// fields as "pid = N", "runs = N" and "last exit code = N" among many others.
// A job that is loaded but not running prints no pid: pid 0 with Loaded is
// that state, not a failure to read it.
func parseLaunchdPrint(out string) (loaded bool, pid, runs, lastExit int) {
	loaded = true // launchctl only prints a job it knows
	lastExit = -1
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "pid":
			pid = atoiOr(value, 0)
		case "runs":
			runs = atoiOr(value, 0)
		case "last exit code":
			lastExit = atoiOr(value, -1)
		}
	}
	return loaded, pid, runs, lastExit
}

// launchdJobIsMissing reports whether launchctl failed because the job is not
// loaded in the user's gui domain ("Could not find service ... in domain for
// user gui: 501", exit 113) rather than from a fault of its own.
func launchdJobIsMissing(out string, err error) bool {
	return strings.Contains(out, "Could not find service") || strings.Contains(err.Error(), "Could not find service")
}

// parseSystemctlShow reads 'systemctl show' output ("Key=Value" lines): a unit
// the manager does not know reports LoadState=not-found, and a loaded one that
// is not running reports MainPID=0.
func parseSystemctlShow(out string) (loaded bool, pid, runs, lastExit int) {
	lastExit = -1
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			loaded = value == "loaded"
		case "MainPID":
			pid = atoiOr(value, 0)
		case "NRestarts":
			runs = atoiOr(value, 0)
		case "ExecMainStatus":
			lastExit = atoiOr(value, -1)
		}
	}
	return loaded, pid, runs, lastExit
}

// systemdJobIsMissing reports whether systemctl failed because it has no such
// unit rather than from a fault of its own.
func systemdJobIsMissing(out string, err error) bool {
	return strings.Contains(out, "not-found") || strings.Contains(out, "could not be found") ||
		strings.Contains(err.Error(), "not-found")
}

func atoiOr(s string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return fallback
}

// SupervisorFileIsFor reports whether the plist / unit at path names townRoot
// as its WorkingDirectory (both templates render it as
// "<string>{{.TownRoot}}</string>" / "WorkingDirectory={{.TownRoot}}"). An
// absent file is (false, nil); a present but unreadable one is an error.
func SupervisorFileIsFor(path, townRoot string) (bool, error) {
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
	// The town may be reached by a different spelling than the one the file
	// was rendered with (a symlinked path, GT_TOWN_ROOT vs a cwd walk; /tmp vs
	// /private/tmp on macOS), and a miss here means a hand spawn beside a
	// loaded job. Compare the recorded WorkingDirectory as a path, symlinks
	// resolved, not as a string.
	recorded := supervisorWorkingDirectory(string(data))
	if recorded == "" {
		// Present but not in the shape the templates render (hand-edited,
		// older format): still a provisioned supervisor as far as launchd or
		// systemd is concerned, so refusing beats treating it as absent.
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

// samePath reports whether two paths name the same directory once cleaned and
// with symlinks resolved; a path that cannot be resolved compares by its
// cleaned spelling.
func samePath(a, b string) bool {
	canon := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Clean(r)
		}
		return filepath.Clean(p)
	}
	return canon(a) == canon(b)
}
