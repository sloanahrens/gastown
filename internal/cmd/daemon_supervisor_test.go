package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// supervisorCalls records what a stubbed supervisor path did.
type supervisorCalls struct {
	argv    [][]string // every argv handed to the supervisor, in order
	spawned bool       // the direct spawn ran
	stopped bool       // the direct stop ran
}

// last returns the most recent supervisor argv; nil when none ran.
func (c *supervisorCalls) last() []string {
	if len(c.argv) == 0 {
		return nil
	}
	return c.argv[len(c.argv)-1]
}

// joined returns the most recent supervisor argv as a command line.
func (c *supervisorCalls) joined() string { return strings.Join(c.last(), " ") }

// stubSupervisor replaces the seams the daemon's supervisor paths use —
// supervisor detection (GOOS + file paths), running the supervisor command,
// reading the job's live state, the direct spawn, the direct stop and the
// running check — and returns a recorder for what was called. state is the
// live read of the job; its Kind is filled in from what detection found.
func stubSupervisor(t *testing.T, goos, file string, state templates.SupervisorState, runErr error) *supervisorCalls {
	t.Helper()
	prevPlist, prevUnit, prevRun, prevState := supervisorPlistPath, supervisorUnitPath, supervisorRun, supervisorStateFor
	prevSpawn, prevStop, prevIsRunning, prevGOOS := spawnDaemonDirect, stopDaemonDirect, daemonIsRunning, supervisorGOOS
	prevPollInterval := daemonPollInterval
	t.Cleanup(func() {
		supervisorPlistPath, supervisorUnitPath, supervisorRun, supervisorStateFor = prevPlist, prevUnit, prevRun, prevState
		spawnDaemonDirect, stopDaemonDirect, daemonIsRunning, supervisorGOOS = prevSpawn, prevStop, prevIsRunning, prevGOOS
		daemonPollInterval = prevPollInterval
	})
	// waitForDaemon polls in real time; shrinking the interval keeps the same
	// attempt count at a wall-clock cost of ms instead of seconds.
	daemonPollInterval = time.Millisecond

	calls := &supervisorCalls{}
	supervisorGOOS = goos
	supervisorPlistPath = func() (string, error) { return file, nil }
	supervisorUnitPath = func() (string, error) { return file, nil }
	supervisorRun = func(a []string) error {
		calls.argv = append(calls.argv, append([]string{}, a...))
		return runErr
	}
	supervisorStateFor = func(kind string) templates.SupervisorState {
		s := state
		s.Kind = kind
		return s
	}
	spawnDaemonDirect = func(townRoot string) (int, error) { calls.spawned = true; return 1234, nil }
	stopDaemonDirect = func(townRoot string) error { calls.stopped = true; return nil }
	// Not running before a start, running after it.
	checks := 0
	daemonIsRunning = func(townRoot string) (bool, int, error) {
		checks++
		if checks == 1 {
			return false, 0, nil
		}
		return true, 4242, nil
	}
	return calls
}

// loadedJob is the live read of a supervisor whose job is loaded and whose
// process is not the daemon under test: start goes through it, and a stop
// leaves the job unloaded.
func loadedJob() templates.SupervisorState {
	return templates.SupervisorState{Loaded: true, LastExit: -1}
}

// writeSupervisorFile writes a plist/unit naming town as WorkingDirectory,
// the way the two templates render it.
func writeSupervisorFile(t *testing.T, name, town string) string {
	t.Helper()
	var body string
	if strings.HasSuffix(name, ".plist") {
		body = "<plist><dict><key>WorkingDirectory</key>\n<string>" + town + "</string></dict></plist>"
	} else {
		body = "[Service]\nWorkingDirectory=" + town + "\nExecStart=/usr/local/bin/gt daemon run\n"
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// With a launchd plist provisioned for this town, gt daemon start must start
// the daemon THROUGH launchd (kickstart) and never spawn its own child: a
// manual daemon holds daemon.lock, and the KeepAlive job then respawns every
// ~10 s, loses the lock and exits 1 for as long as the manual one lives
// (gt-3jrm).
func TestStartDaemon_UsesLaunchdWhenProvisioned(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)

	via, pid, err := startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if calls.spawned {
		t.Fatal("spawned a direct daemon child although launchd is provisioned")
	}
	if via != "launchd" || pid != 4242 {
		t.Fatalf("startDaemon = (%q, %d), want (launchd, 4242)", via, pid)
	}
	got := calls.joined()
	if !strings.HasPrefix(got, "launchctl kickstart -k gui/") || !strings.HasSuffix(got, "/com.gastown.daemon") {
		t.Fatalf("supervisor command = %q, want launchctl kickstart -k gui/<uid>/com.gastown.daemon", got)
	}
}

// A job the service manager does not know — the state gt daemon stop leaves
// on macOS — cannot be kickstarted, so start bootstraps it rather than
// falling back to a hand spawn that recreates the crash loop.
func TestStartDaemon_BootstrapsAJobTheManagerDoesNotKnow(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	calls := stubSupervisor(t, "darwin", plist, templates.SupervisorState{Loaded: false, LastExit: -1},
		errors.New("Could not find service"))

	via, pid, err := startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if via != "launchd" || pid != 4242 || calls.spawned {
		t.Fatalf("startDaemon = (%q, %d, spawned=%v), want (launchd, 4242, false)", via, pid, calls.spawned)
	}
	if len(calls.argv) != 2 {
		t.Fatalf("supervisor commands = %v, want a kickstart that failed and a bootstrap", calls.argv)
	}
	if got := calls.joined(); got != "launchctl bootstrap gui/"+strconv.Itoa(os.Getuid())+" "+plist {
		t.Fatalf("second supervisor command = %q, want the bootstrap for %s", got, plist)
	}
}

// No plist → the direct spawn, exactly as before.
func TestStartDaemon_SpawnsDirectlyWithoutSupervisor(t *testing.T) {
	calls := stubSupervisor(t, "darwin", filepath.Join(t.TempDir(), "absent.plist"), loadedJob(), nil)
	via, _, err := startDaemon(t.TempDir())
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if len(calls.argv) != 0 || via != "" {
		t.Fatalf("ran=%v via=%q with no supervisor provisioned", calls.argv, via)
	}
	if !calls.spawned {
		t.Fatal("did not spawn the daemon directly")
	}
}

// The plist is per user and names the town it was provisioned for; from a
// different workspace it must not be kickstarted (that would restart the
// other town's daemon and then wait for this one's lock in vain).
func TestStartDaemon_IgnoresSupervisorOfAnotherTown(t *testing.T) {
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", filepath.Join(t.TempDir(), "other-town"))
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	via, _, err := startDaemon(t.TempDir())
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if len(calls.argv) != 0 || via != "" || !calls.spawned {
		t.Fatalf("ran=%v via=%q spawned=%v; want the direct spawn, no supervisor", calls.argv, via, calls.spawned)
	}
}

// A plist that exists but whose job is not loaded (booted out) makes
// kickstart fail. The daemon must NOT be spawned by hand — a job that is in
// fact still loaded would respawn against it forever — and the error must
// say how to load the job.
func TestStartDaemon_SupervisorFailureIsAnErrorNotAManualSpawn(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), errors.New("launchd refused"))
	// The daemon never comes up: every answer is "not running" for this case.
	daemonIsRunning = func(string) (bool, int, error) { return false, 0, nil }
	_, _, err := startDaemon(town)
	if err == nil {
		t.Fatal("startDaemon succeeded although launchd could not start the daemon")
	}
	if len(calls.argv) == 0 {
		t.Fatal("did not try the supervisor first")
	}
	if calls.spawned {
		t.Fatal("spawned a manual daemon beside a provisioned supervisor")
	}
	if !strings.Contains(err.Error(), "launchctl bootstrap gui/") || !strings.Contains(err.Error(), plist) {
		t.Fatalf("error does not say how to load the job: %v", err)
	}
}

// A supervisor command that reports failure may still have started the
// daemon; that is success, not an error.
func TestStartDaemon_SupervisorErrorButDaemonCameUp(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), errors.New("kickstart: noise on stderr"))
	via, pid, err := startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if via != "launchd" || pid != 4242 || calls.spawned {
		t.Fatalf("got (%q, %d, spawned=%v), want (launchd, 4242, false)", via, pid, calls.spawned)
	}
}

// A supervisor file that exists but cannot be read must not be mistaken
// for "no supervisor" — that is the one answer that recreates the loop.
func TestStartDaemon_UnreadableSupervisorFileIsAnError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read anything")
	}
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	if err := os.Chmod(plist, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(plist, 0o644) })
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	if _, _, err := startDaemon(town); err == nil {
		t.Fatal("startDaemon succeeded with an unreadable supervisor file")
	}
	if len(calls.argv) != 0 || calls.spawned {
		t.Fatalf("ran=%v spawned=%v; want neither with an unreadable supervisor file", calls.argv, calls.spawned)
	}
}

// On Linux the provisioned supervisor is a systemd user unit; the same
// decision routes through systemctl.
func TestStartDaemon_UsesSystemdWhenProvisioned(t *testing.T) {
	town := t.TempDir()
	unit := writeSupervisorFile(t, "gastown-daemon.service", town)
	calls := stubSupervisor(t, "linux", unit, loadedJob(), nil)
	via, _, err := startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if calls.spawned || via != "systemd" {
		t.Fatalf("via=%q spawned=%v, want systemd and no direct spawn", via, calls.spawned)
	}
	if got := calls.joined(); got != "systemctl --user restart gastown-daemon.service" {
		t.Fatalf("supervisor command = %q", got)
	}
}

// An OS with no supervisor support spawns directly.
func TestStartDaemon_UnsupportedOSSpawnsDirectly(t *testing.T) {
	calls := stubSupervisor(t, "windows", filepath.Join(t.TempDir(), "whatever"), loadedJob(), nil)
	if _, _, err := startDaemon(t.TempDir()); err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if len(calls.argv) != 0 || !calls.spawned {
		t.Fatalf("ran=%v spawned=%v, want no supervisor command and a direct spawn", calls.argv, calls.spawned)
	}
}

// The town may be spelled differently from the provisioned file (a symlink
// to it, /tmp vs /private/tmp on macOS); the guard must still see the
// supervisor, or it hand-spawns beside a loaded job.
func TestStartDaemon_MatchesSupervisorThroughSymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "town-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", real)
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	via, _, err := startDaemon(link)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if via != "launchd" || calls.spawned || len(calls.argv) == 0 {
		t.Fatalf("via=%q spawned=%v ran=%v; want launchd through the symlinked town", via, calls.spawned, calls.argv)
	}
}

// A supervisor file with no WorkingDirectory (hand-edited, older format)
// is provisioned as far as launchd is concerned; it must be an error, not
// "no supervisor".
func TestStartDaemon_SupervisorFileWithoutWorkingDirectoryIsAnError(t *testing.T) {
	plist := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(plist, []byte("<plist><dict><key>Label</key><string>com.gastown.daemon</string></dict></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	_, _, err := startDaemon(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "WorkingDirectory") {
		t.Fatalf("err = %v, want an error naming the missing WorkingDirectory", err)
	}
	if len(calls.argv) != 0 || calls.spawned {
		t.Fatalf("ran=%v spawned=%v; want neither", calls.argv, calls.spawned)
	}
}

// A signal alone does not stop a supervised daemon: the loaded job respawns
// what it manages. Stop must go through the supervisor so the job is left
// unloaded, and must not signal the process as well (gt-sq9e).
func TestRunDaemonStop_StopsTheJobThroughTheSupervisor(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	// Holding the lock when stop starts, free once the job has taken the
	// process down.
	checks := 0
	daemonIsRunning = func(string) (bool, int, error) {
		checks++
		return checks == 1, 4242, nil
	}
	chdirTown(t, town)

	if err := runDaemonStop(nil, nil); err != nil {
		t.Fatalf("runDaemonStop: %v", err)
	}
	if got := calls.joined(); got != "launchctl bootout gui/"+strconv.Itoa(os.Getuid())+"/com.gastown.daemon" {
		t.Fatalf("supervisor command = %q, want launchctl bootout gui/<uid>/com.gastown.daemon", got)
	}
	if calls.stopped {
		t.Fatal("signalled the process although the supervisor job took it down")
	}
}

// With the job unloaded, there is no supervisor to stop: the daemon is the
// only thing left to signal, and nothing should be run against launchd.
func TestRunDaemonStop_UnloadedJobSignalsTheDaemon(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	calls := stubSupervisor(t, "darwin", plist, templates.SupervisorState{Loaded: false, LastExit: -1}, nil)
	daemonIsRunning = func(string) (bool, int, error) { return true, 4242, nil }
	chdirTown(t, town)

	if err := runDaemonStop(nil, nil); err != nil {
		t.Fatalf("runDaemonStop: %v", err)
	}
	if len(calls.argv) != 0 {
		t.Fatalf("ran=%v, want no supervisor command for an unloaded job", calls.argv)
	}
	if !calls.stopped {
		t.Fatal("did not signal the daemon holding the lock")
	}
}

// A supervisor that cannot stop the job leaves the daemon's fate unproven:
// the signal still goes out, so a stop is never a no-op, but the command must
// not report the stop it could not confirm (gt-ojbb).
func TestRunDaemonStop_SupervisorFailureIsNotReportedAsSuccess(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), errors.New("Boot-out failed: 3: No such process"))
	daemonIsRunning = func(string) (bool, int, error) { return true, 4242, nil }
	chdirTown(t, town)

	err := runDaemonStop(nil, nil)
	if err == nil {
		t.Fatal("runDaemonStop reported success although the launchd job could not be stopped")
	}
	if !strings.Contains(err.Error(), "com.gastown.daemon") && !strings.Contains(err.Error(), "launchd") {
		t.Errorf("err = %v, want it to name the job that may still be loaded", err)
	}
	if got := calls.joined(); got != "launchctl bootout gui/"+strconv.Itoa(os.Getuid())+"/com.gastown.daemon" {
		t.Fatalf("supervisor command = %q, want the bootout attempt", got)
	}
	if !calls.stopped {
		t.Fatal("did not signal the daemon after the supervisor could not stop the job")
	}
}

// A probe that fails says nothing about whether the job is loaded, so the
// unreadable state is the same uncertainty as a failed bootout — not the
// "not loaded" reading that turns into a signal and a success report (gt-ojbb).
func TestRunDaemonStop_ProbeFailureIsNotReportedAsSuccess(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	state := templates.SupervisorState{Err: errors.New("launchctl print: exit status 1")}
	calls := stubSupervisor(t, "darwin", plist, state, nil)
	daemonIsRunning = func(string) (bool, int, error) { return true, 4242, nil }
	chdirTown(t, town)

	err := runDaemonStop(nil, nil)
	if err == nil {
		t.Fatal("runDaemonStop reported success although the job's state could not be read")
	}
	if !strings.Contains(err.Error(), "launchctl print") {
		t.Errorf("err = %v, want it to carry the probe failure", err)
	}
	if len(calls.argv) != 0 {
		t.Fatalf("ran=%v, want no supervisor command on a failed probe", calls.argv)
	}
	if !calls.stopped {
		t.Fatal("did not signal the daemon when the job's state was unknown")
	}
}

// Detection failing means the town may or may not have a KeepAlive job; the
// daemon is still signaled, and the unknown is reported rather than assumed
// away (gt-ojbb).
func TestRunDaemonStop_DetectionFailureIsNotReportedAsSuccess(t *testing.T) {
	town := t.TempDir()
	// Present but silent about its town: detectDaemonSupervisor errors rather
	// than answering "no supervisor".
	plist := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(plist, []byte("<plist><dict><key>Label</key><string>com.gastown.daemon</string></dict></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	daemonIsRunning = func(string) (bool, int, error) { return true, 4242, nil }
	chdirTown(t, town)

	err := runDaemonStop(nil, nil)
	if err == nil {
		t.Fatal("runDaemonStop reported success although it could not tell whether a supervisor exists")
	}
	if !strings.Contains(err.Error(), "WorkingDirectory") {
		t.Errorf("err = %v, want it to carry the detection failure", err)
	}
	if len(calls.argv) != 0 {
		t.Fatalf("ran=%v, want no supervisor command when detection failed", calls.argv)
	}
	if !calls.stopped {
		t.Fatal("did not signal the daemon when the supervisor was unknown")
	}
}

// A supervisor SIGTERM can land between the re-check and StopDaemon's own: the
// daemon releases the lock, StopDaemon finds nothing to stop and reports "not
// running", and the stop that did happen must not surface as a failure (gt-ojbb).
func TestRunDaemonStop_DaemonReleasingTheLockMidStopIsNotAnError(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	stopDaemonDirect = func(string) error { calls.stopped = true; return errors.New("daemon is not running") }
	checks := 0
	daemonIsRunning = func(string) (bool, int, error) {
		checks++
		return checks < 3, 4242, nil // holding the lock, then gone
	}
	chdirTown(t, town)

	if err := runDaemonStop(nil, nil); err != nil {
		t.Fatalf("runDaemonStop: %v, want no error for a daemon that stopped under the bootout", err)
	}
	if !calls.stopped {
		t.Fatal("did not attempt the direct stop after the bootout")
	}
}

// Still holding the lock after a failed direct stop is a stop that did not
// happen, and it stays an error (gt-ojbb).
func TestRunDaemonStop_StillRunningAfterTheDirectStopIsAnError(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	stopDaemonDirect = func(string) error { return errors.New("sending termination signal: operation not permitted") }
	daemonIsRunning = func(string) (bool, int, error) { return true, 4242, nil }
	chdirTown(t, town)

	err := runDaemonStop(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("err = %v, want the direct stop failure", err)
	}
}

// chdirTown points the command's workspace lookup at a town root of its own.
func chdirTown(t *testing.T, town string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(town)
}

// stubDaemonRunning answers the daemon's running check with pid; 0 means the
// lock is free.
func stubDaemonRunning(t *testing.T, pid int) {
	t.Helper()
	prev := daemonIsRunning
	t.Cleanup(func() { daemonIsRunning = prev })
	daemonIsRunning = func(string) (bool, int, error) { return pid != 0, pid, nil }
}

// stubSupervisorState answers the live read of the supervisor job with st.
func stubSupervisorState(t *testing.T, st templates.SupervisorState) {
	t.Helper()
	prev := supervisorStateFor
	t.Cleanup(func() { supervisorStateFor = prev })
	supervisorStateFor = func(kind string) templates.SupervisorState {
		s := st
		s.Kind = kind
		return s
	}
}

// writeHostSupervisorFile installs the plist / unit this host would use, in an
// isolated HOME, naming town as its WorkingDirectory. The resolved path is a
// real per-user one, so moving HOME first is what keeps a test from writing at
// the operator's own.
func writeHostSupervisorFile(t *testing.T, town string) {
	t.Helper()
	home := t.TempDir()
	dataHome := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", dataHome)
	path, kind := templates.SupervisorFilePath()
	if path == "" {
		t.Skip("no supervisor on this host")
	}
	if !strings.HasPrefix(path, home) && !strings.HasPrefix(path, dataHome) {
		t.Fatalf("supervisor file %s is outside the isolated HOME %s", path, home)
	}
	var body string
	if kind == "launchd" {
		body = "<plist><dict><key>WorkingDirectory</key>\n<string>" + town + "</string></dict></plist>"
	} else {
		body = "[Service]\nWorkingDirectory=" + town + "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gt-sq9e: a daemon holding the lock while the provisioned job crash-loops
// behind it must not read as supervised. The job has no process of its own
// between crashes, which is what the line has to name.
func TestRunDaemonStatus_NamesACrashLoopingSupervisor(t *testing.T) {
	town := t.TempDir()
	writeHostSupervisorFile(t, town)
	chdirTown(t, town)
	stubDaemonRunning(t, 76426)
	stubSupervisorState(t, templates.SupervisorState{Loaded: true, Runs: 58, LastExit: 1})

	kind := templates.SupervisorStatus()
	if kind == "none" {
		t.Fatal("the supervisor file just written is not being seen")
	}
	out := captureStdout(t, func() { _ = runDaemonStatus(nil, nil) })

	want := "Supervised: " + kind + " (DETACHED - " + kind + " is running no daemon while PID 76426 holds the lock, runs=58, last exit code=1)"
	if !strings.Contains(out, want) {
		t.Errorf("status output = %q, want it to contain %q", out, want)
	}
}

// The daemon being the supervisor's own process is the one state that reads
// as plain "Supervised: launchd".
func TestRunDaemonStatus_PlainKindWhenTheJobRunsTheDaemon(t *testing.T) {
	town := t.TempDir()
	writeHostSupervisorFile(t, town)
	chdirTown(t, town)
	stubDaemonRunning(t, 64604)
	stubSupervisorState(t, templates.SupervisorState{Loaded: true, PID: 64604, Runs: 53, LastExit: 0})

	kind := templates.SupervisorStatus()
	if kind == "none" {
		t.Fatal("the supervisor file just written is not being seen")
	}
	out := captureStdout(t, func() { _ = runDaemonStatus(nil, nil) })

	if !strings.Contains(out, "Supervised: "+kind+"\n") {
		t.Errorf("status output = %q, want a bare 'Supervised: %s'", out, kind)
	}
}

// With no daemon running and the job loaded but spawning nothing, the state
// belongs in the not-running branch too: that is where an operator looks
// after a failed restart.
func TestRunDaemonStatus_NamesAFailingSupervisorWithNoDaemon(t *testing.T) {
	town := t.TempDir()
	writeHostSupervisorFile(t, town)
	chdirTown(t, town)
	stubDaemonRunning(t, 0)
	stubSupervisorState(t, templates.SupervisorState{Loaded: true, Runs: 58, LastExit: 1})

	kind := templates.SupervisorStatus()
	if kind == "none" {
		t.Fatal("the supervisor file just written is not being seen")
	}
	out := captureStdout(t, func() { _ = runDaemonStatus(nil, nil) })

	want := "Supervised: " + kind + " (FAILING - loaded, running no daemon, runs=58, last exit code=1)"
	if !strings.Contains(out, want) {
		t.Errorf("status output = %q, want it to contain %q", out, want)
	}
}

// stubRestart replaces the seams restartDaemon uses and returns a recorder.
// pids is the sequence the lock reports, one entry per read with the last
// repeating: a restart reads the outgoing PID first and then polls for a
// different one, so the sequence is how these tests express "the same daemon
// is still on its way out" versus "a new one is up".
func stubRestart(t *testing.T, goos, file string, pids ...int) *supervisorCalls {
	t.Helper()
	prevPlist, prevUnit, prevRun, prevState := supervisorPlistPath, supervisorUnitPath, supervisorRun, supervisorStateFor
	prevSpawn, prevStop, prevIsRunning, prevGOOS := spawnDaemonDirect, stopDaemonDirect, daemonIsRunning, supervisorGOOS
	prevPollInterval := daemonPollInterval
	t.Cleanup(func() {
		supervisorPlistPath, supervisorUnitPath, supervisorRun, supervisorStateFor = prevPlist, prevUnit, prevRun, prevState
		spawnDaemonDirect, stopDaemonDirect, daemonIsRunning, supervisorGOOS = prevSpawn, prevStop, prevIsRunning, prevGOOS
		daemonPollInterval = prevPollInterval
	})
	// waitForDaemon/waitForRestart poll in real time; a test that exhausts
	// the full attempt budget (e.g. the new daemon never takes the lock)
	// would otherwise block for waitForRestart's real 60 s. Shrinking the
	// interval keeps the same number of polls at a wall-clock cost of ms.
	daemonPollInterval = time.Millisecond

	calls := &supervisorCalls{}
	supervisorGOOS = goos
	supervisorPlistPath = func() (string, error) { return file, nil }
	supervisorUnitPath = func() (string, error) { return file, nil }
	supervisorRun = func(a []string) error {
		calls.argv = append(calls.argv, append([]string{}, a...))
		return nil
	}
	supervisorStateFor = func(kind string) templates.SupervisorState {
		return templates.SupervisorState{Kind: kind, Loaded: true, LastExit: -1}
	}
	spawnDaemonDirect = func(string) (int, error) { calls.spawned = true; return 9999, nil }
	stopDaemonDirect = func(string) error { calls.stopped = true; return nil }
	reads := 0
	daemonIsRunning = func(string) (bool, int, error) {
		pid := pids[min(reads, len(pids)-1)]
		reads++
		return pid != 0, pid, nil
	}
	return calls
}

// A plist as this binary renders it for town, but without the ExitTimeOut a
// binary that passes a shutdown budget adds — the shape a file installed
// before that key existed has on disk (gt-x872). Written where the supervisor
// path seams say the plist lives.
func writeStaleSupervisorFile(t *testing.T, town string) string {
	t.Helper()
	body, ok, err := templates.SupervisorFileContent("launchd", town, 0)
	if err != nil || !ok {
		t.Fatalf("SupervisorFileContent(launchd, %q) = (ok=%v, err=%v)", town, ok, err)
	}
	p := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The plist on disk is repaired on the way into a start, and because launchd
// reads a job definition only when it loads one, the job is loaded from the
// rewritten file rather than kickstarted from the definition it cached
// (gt-x872): a kickstart would restart the daemon under the ExitTimeOut the
// file was installed with, which is the value the repair exists to replace.
func TestStartDaemon_RepairsAStaleExitTimeOutBeforeStarting(t *testing.T) {
	town := t.TempDir()
	plist := writeStaleSupervisorFile(t, town)
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)

	via, pid, err := startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if via != "launchd" || pid != 4242 || calls.spawned {
		t.Fatalf("startDaemon = (%q, %d, spawned=%v), want (launchd, 4242, false)", via, pid, calls.spawned)
	}
	if len(calls.argv) != 2 {
		t.Fatalf("supervisor commands = %v, want an unload then a load", calls.argv)
	}
	if got := strings.Join(calls.argv[0], " "); !strings.HasPrefix(got, "launchctl bootout gui/") {
		t.Errorf("first supervisor command = %q, want a bootout first: a loaded job does not re-read its file", got)
	}
	if got := strings.Join(calls.argv[1], " "); got != "launchctl bootstrap gui/"+strconv.Itoa(os.Getuid())+" "+plist {
		t.Errorf("second supervisor command = %q, want the bootstrap for %s", got, plist)
	}

	repaired, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(repaired), "<key>ExitTimeOut</key>") {
		t.Errorf("plist was not repaired before the job was loaded from it:\n%s", repaired)
	}
}

// A restart repairs the same way, so the ExitTimeOut a restart depends on is
// in force for the stop that restart causes and every one after it (gt-x872).
func TestRestartDaemon_RepairsAStaleExitTimeOutBeforeRestarting(t *testing.T) {
	town := t.TempDir()
	plist := writeStaleSupervisorFile(t, town)
	calls := stubRestart(t, "darwin", plist, 1111, 1111, 4242)

	via, pid, err := restartDaemon(town)
	if err != nil {
		t.Fatalf("restartDaemon: %v", err)
	}
	if via != "launchd" || pid != 4242 {
		t.Fatalf("restartDaemon = (%q, %d), want (launchd, 4242)", via, pid)
	}
	if len(calls.argv) != 2 {
		t.Fatalf("supervisor commands = %v, want an unload then a load", calls.argv)
	}
	if got := strings.Join(calls.argv[0], " "); !strings.HasPrefix(got, "launchctl bootout gui/") {
		t.Errorf("first supervisor command = %q, want a bootout: the job has to be loaded from the repaired file", got)
	}
	repaired, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(repaired), "<key>ExitTimeOut</key>") {
		t.Errorf("plist was not repaired before the job was loaded from it:\n%s", repaired)
	}
}

// A file that is not this binary's rendering of this town is left exactly as
// it is, and the start proceeds in place: repairing it would be a
// reconfiguration of somebody else's job, not a repair (gt-x872).
func TestSyncSupervisorFile_LeavesAFileItDidNotRender(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	before, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	stubSupervisor(t, "darwin", plist, loadedJob(), nil)

	if rewritten := syncSupervisorFile(town); rewritten {
		t.Error("syncSupervisorFile rewrote a plist this binary did not render")
	}
	after, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("plist changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A repair that cannot be made is warned about and dropped: the job file
// being behind is a reason to restart the daemon under a worse ExitTimeOut,
// never a reason to leave the town with no daemon at all (gt-x872).
func TestStartDaemon_StartsWithAFileItCouldNotRepair(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory, so the repair would succeed")
	}
	town := t.TempDir()
	body, ok, err := templates.SupervisorFileContent("launchd", town, 0)
	if err != nil || !ok {
		t.Fatalf("SupervisorFileContent(launchd, %q) = (ok=%v, err=%v)", town, ok, err)
	}
	dir := t.TempDir()
	plist := filepath.Join(dir, "com.gastown.daemon.plist")
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Unwritable, so the repair cannot be installed over it.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)

	via, pid, err := startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if via != "launchd" || pid != 4242 {
		t.Fatalf("startDaemon = (%q, %d), want (launchd, 4242)", via, pid)
	}
	if got := calls.joined(); !strings.HasPrefix(got, "launchctl kickstart -k gui/") {
		t.Errorf("supervisor command = %q, want the ordinary kickstart when no repair was made", got)
	}
	after, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != body {
		t.Error("the plist changed although the repair could not be installed")
	}
}

// The town boot is the one place a stale job file is found with the daemon
// already up, so it is the one place the reload happens without a start or a
// restart asking for it (gt-x872). The note it returns is what stops the
// daemon's PID changing from looking unexplained.
func TestReconcileSupervisorJob_ReloadsAStaleJob(t *testing.T) {
	town := t.TempDir()
	plist := writeStaleSupervisorFile(t, town)
	calls := stubRestart(t, "darwin", plist, 1111, 4242)

	note, err := reconcileSupervisorJob(town, 1111)
	if err != nil {
		t.Fatalf("reconcileSupervisorJob: %v", err)
	}
	if note == "" {
		t.Error("reconcileSupervisorJob returned no note for a job it reloaded")
	}
	if len(calls.argv) != 2 {
		t.Fatalf("supervisor commands = %v, want an unload then a load", calls.argv)
	}
	if got := strings.Join(calls.argv[0], " "); !strings.HasPrefix(got, "launchctl bootout gui/") {
		t.Errorf("first supervisor command = %q, want a bootout", got)
	}
}

// A job file that is already current leaves the running daemon alone: the
// reload is an interruption, and one that is not needed is one not paid.
func TestReconcileSupervisorJob_LeavesACurrentJobRunning(t *testing.T) {
	town := t.TempDir()
	body, ok, err := templates.SupervisorFileContent("launchd", town, daemon.ShutdownBudget)
	if err != nil || !ok {
		t.Fatalf("SupervisorFileContent(launchd, %q) = (ok=%v, err=%v)", town, ok, err)
	}
	plist := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := stubRestart(t, "darwin", plist, 1111)

	note, err := reconcileSupervisorJob(town, 1111)
	if err != nil {
		t.Fatalf("reconcileSupervisorJob: %v", err)
	}
	if note != "" {
		t.Errorf("reconcileSupervisorJob note = %q, want \"\"", note)
	}
	if len(calls.argv) != 0 {
		t.Errorf("supervisor commands = %v, want none", calls.argv)
	}
}

// A restart of a supervised daemon goes through the supervisor's kickstart -k
// and never through a hand stop: the stop path is launchctl bootout, which
// unloads the job, so a restart built from it leaves the town unsupervised
// between the two commands (gt-oqbw).
func TestRestartDaemon_SupervisedRestartsInPlace(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	calls := stubRestart(t, "darwin", plist, 1111, 1111, 4242)

	via, pid, err := restartDaemon(town)
	if err != nil {
		t.Fatalf("restartDaemon: %v", err)
	}
	if calls.stopped || calls.spawned {
		t.Errorf("supervised restart used a hand path: stopped=%v spawned=%v", calls.stopped, calls.spawned)
	}
	if got := calls.joined(); !strings.HasPrefix(got, "launchctl kickstart -k gui/") || !strings.HasSuffix(got, "/com.gastown.daemon") {
		t.Errorf("supervisor command = %q, want launchctl kickstart -k gui/<uid>/com.gastown.daemon", got)
	}
	if via != "launchd" || pid != 4242 {
		t.Errorf("restartDaemon = (%q, %d), want (launchd, 4242)", via, pid)
	}
	// Reports the NEW pid, not the outgoing one the lock still names.
	if pid == 1111 {
		t.Error("restart reported the outgoing daemon's pid")
	}
}

// With no supervisor provisioned the daemon is run by hand, so the restart is
// a hand stop followed by a hand start.
func TestRestartDaemon_HandRestartWithoutASupervisor(t *testing.T) {
	town := t.TempDir()
	calls := stubRestart(t, "darwin", "", 1111, 0)

	via, pid, err := restartDaemon(town)
	if err != nil {
		t.Fatalf("restartDaemon: %v", err)
	}
	if !calls.stopped || !calls.spawned {
		t.Errorf("unsupervised restart = stopped:%v spawned:%v, want both", calls.stopped, calls.spawned)
	}
	if len(calls.argv) != 0 {
		t.Errorf("no supervisor is provisioned but %v ran", calls.argv)
	}
	if via != "" || pid != 9999 {
		t.Errorf("restartDaemon = (%q, %d), want (\"\", 9999)", via, pid)
	}
}

// The hand path must not spawn a new daemon into the lock before the old one
// is confirmed gone: stopDaemonDirect sends SIGKILL and returns without
// waiting to confirm it took, so restartDaemon confirms it itself
// (waitForDaemonGone) before calling startDaemon. Here the lock keeps
// reporting the old PID for the whole confirm budget, so the restart must
// fail loudly instead of racing a spawn against a still-live old daemon.
func TestRestartDaemon_HandPathRefusesToStartBeforeTheOldDaemonIsGone(t *testing.T) {
	town := t.TempDir()
	calls := stubRestart(t, "darwin", "", 1111) // never reports gone

	_, _, err := restartDaemon(town)
	if err == nil {
		t.Fatal("restartDaemon reported success although the old daemon's lock was never confirmed released")
	}
	if !calls.stopped {
		t.Error("stopDaemonDirect was never called")
	}
	if calls.spawned {
		t.Error("a new daemon was spawned before the old one was confirmed gone")
	}
}

// A restart with no daemon running is a start: the caller asking for "the
// daemon on the current binary" does not have to check first.
func TestRestartDaemon_StartsAStoppedDaemon(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	// Two stopped reads: the restart's own check, then start's. The third is
	// waitForDaemon seeing the daemon the kickstart brought up.
	calls := stubRestart(t, "darwin", plist, 0, 0, 4242)

	via, pid, err := restartDaemon(town)
	if err != nil {
		t.Fatalf("restartDaemon: %v", err)
	}
	if calls.stopped {
		t.Error("a stopped daemon must not be stopped again")
	}
	if !strings.Contains(calls.joined(), "kickstart") || via != "launchd" || pid != 4242 {
		t.Errorf("restartDaemon = (%q, %d) via %q", via, pid, calls.joined())
	}
}

// A restart whose new daemon never takes the lock is an error, not a success
// that reports the outgoing PID: the whole point is that the process now
// running is the one on the new binary.
func TestRestartDaemon_ErrorsWhenTheNewDaemonNeverTakesTheLock(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	// The lock keeps naming the old PID for the whole wait: the restarted
	// daemon never came up (or came up and died immediately).
	calls := stubRestart(t, "darwin", plist, 1111)

	if _, _, err := restartDaemon(town); err == nil {
		t.Fatal("restartDaemon reported success although no new daemon took the lock")
	}
	if !strings.Contains(calls.joined(), "kickstart") {
		t.Errorf("the restart was never attempted: %v", calls.argv)
	}
}

// A lock reporting running=true with pid 0 is a start still in flight (the
// PID file has not been written yet), not the new daemon a restart is
// waiting for — waitForRestart must keep polling past it rather than
// returning pid 0 as success.
func TestRestartDaemon_WaitForRestartIgnoresPidZero(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	// oldPID, then a stretch of "running but pid unknown yet", then the real
	// new daemon.
	calls := stubRestart(t, "darwin", plist, 1111, 1111, 0, 0, 4242)

	via, pid, err := restartDaemon(town)
	if err != nil {
		t.Fatalf("restartDaemon: %v", err)
	}
	if via != "launchd" || pid != 4242 {
		t.Errorf("restartDaemon = (%q, %d), want (launchd, 4242)", via, pid)
	}
	if !strings.Contains(calls.joined(), "kickstart") {
		t.Errorf("the restart was never attempted: %v", calls.argv)
	}
}

// waitForRestart's budget (restartWaitBudget = daemon.ShutdownBudget +
// daemonStartupMargin) must outlast the outgoing daemon's own bounded
// shutdown stages (pushDoltRemotesBounded's 20s + the Dolt server's own
// graceful-stop wait, 30s + OTel's 5s) plus the incoming daemon's startup
// preflight — the exact gap the original 10s budget (100 attempts) did not
// cover (gt-oqbw). This simulates a restart that is genuinely slow,
// not stuck: the lock keeps naming the outgoing PID for slowAttempts of
// restartWaitAttempts before the new daemon takes it, comfortably inside the
// budget but well past what the original one allowed. Under the old budget
// this would time out and be misreported as a failed restart although it was
// still in progress.
func TestRestartDaemon_ToleratesASlowShutdownAndRestart(t *testing.T) {
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	const oldPID, newPID, slowAttempts = 1111, 4242, 550
	if slowAttempts >= restartWaitAttempts {
		t.Fatalf("slowAttempts (%d) must stay below restartWaitAttempts (%d) or this test proves nothing", slowAttempts, restartWaitAttempts)
	}
	pids := make([]int, 0, slowAttempts+1)
	for range slowAttempts {
		pids = append(pids, oldPID)
	}
	pids = append(pids, newPID)
	calls := stubRestart(t, "darwin", plist, pids...)

	via, pid, err := restartDaemon(town)
	if err != nil {
		t.Fatalf("restartDaemon: %v (budget too short for a slow-but-real restart)", err)
	}
	if via != "launchd" || pid != newPID {
		t.Errorf("restartDaemon = (%q, %d), want (launchd, %d)", via, pid, newPID)
	}
	if !strings.Contains(calls.joined(), "kickstart") {
		t.Errorf("the restart was never attempted: %v", calls.argv)
	}
}

// gt-o848l: `gt daemon status` is the "is it running?" probe for the
// Makefile's install restart and mol-gastown-boot's `status || start`. It
// exited 0 either way, so make install STARTED a deliberately stopped daemon
// and the boot formula never started a missing one. Not running exits 3
// (LSB "program is not running"); running exits 0.
func TestRunDaemonStatus_ExitCodeReportsRunning(t *testing.T) {
	town := t.TempDir()
	chdirTown(t, town)
	stubSupervisorState(t, templates.SupervisorState{})

	stubDaemonRunning(t, 0)
	var err error
	_ = captureStdout(t, func() { err = runDaemonStatus(nil, nil) })
	var silent *SilentExitError
	if !errors.As(err, &silent) || silent.Code != 3 {
		t.Errorf("status with no daemon: err = %v, want SilentExitError code 3", err)
	}

	stubDaemonRunning(t, 4242)
	_ = captureStdout(t, func() { err = runDaemonStatus(nil, nil) })
	if err != nil {
		t.Errorf("status with a running daemon: err = %v, want nil", err)
	}
}
