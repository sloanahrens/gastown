package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// supervisorCalls records what a stubbed supervisor path did.
type supervisorCalls struct {
	argv    [][]string // every argv handed to the supervisor, in order
	envs    [][]string // the environment each argv ran with, parallel to argv; nil for the plain seam
	spawned bool       // the direct spawn ran
	// spawnEnv is the environment the direct spawn was handed.
	spawnEnv []string
	stopped  bool // the direct stop ran
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

// stubSupervisor builds the daemonControl the daemon's supervisor paths run
// on — supervisor detection (GOOS + file paths), running the supervisor
// command, reading the job's live state, the direct spawn, the direct stop
// and the running check — and returns it with a recorder for what was
// called. state is the live read of the job; its Kind is filled in from what
// detection found. The waits between polls return at once.
func stubSupervisor(t *testing.T, goos, file string, state templates.SupervisorState, runErr error) (*daemonControl, *supervisorCalls) {
	t.Helper()
	calls := &supervisorCalls{}
	c := &daemonControl{
		goos:      goos,
		plistPath: func() (string, error) { return file, nil },
		unitPath:  func() (string, error) { return file, nil },
		uid:       os.Getuid(),
		run: func(a []string) error {
			calls.argv = append(calls.argv, append([]string{}, a...))
			calls.envs = append(calls.envs, nil)
			return runErr
		},
		runEnv: func(a, env []string) error {
			calls.argv = append(calls.argv, append([]string{}, a...))
			calls.envs = append(calls.envs, append([]string{}, env...))
			return runErr
		},
		// The host has no docker to read a container with unless a test says
		// otherwise; a fake that fails is never mistaken for an absent
		// container.
		dockerInspect: func(context.Context, []string) ([]byte, error) {
			return nil, errors.New("docker: daemon unreachable")
		},
		stateFor: func(kind string) templates.SupervisorState {
			s := state
			s.Kind = kind
			return s
		},
		spawn: func(townRoot string, env []string) (int, error) {
			calls.spawned = true
			calls.spawnEnv = env
			return 1234, nil
		},
		stopDirect: func(townRoot string) error { calls.stopped = true; return nil },
		install:    installSupervisorFile,
		statusLine: func(string, int) string { return "none" },
		sleep:      func(time.Duration) {},
	}
	// Not running before a start, running after it.
	checks := 0
	c.isRunning = func(townRoot string) (bool, int, error) {
		checks++
		if checks == 1 {
			return false, 0, nil
		}
		return true, 4242, nil
	}
	return c, calls
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)

	via, pid, err := c.startDaemon(town)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, templates.SupervisorState{Loaded: false, LastExit: -1},
		errors.New("Could not find service"))

	via, pid, err := c.startDaemon(town)
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
	t.Parallel()
	c, calls := stubSupervisor(t, "darwin", filepath.Join(t.TempDir(), "absent.plist"), loadedJob(), nil)
	via, _, err := c.startDaemon(t.TempDir())
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
	t.Parallel()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", filepath.Join(t.TempDir(), "other-town"))
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	via, _, err := c.startDaemon(t.TempDir())
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), errors.New("launchd refused"))
	// The daemon never comes up: every answer is "not running" for this case.
	c.isRunning = func(string) (bool, int, error) { return false, 0, nil }
	_, _, err := c.startDaemon(town)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), errors.New("kickstart: noise on stderr"))
	via, pid, err := c.startDaemon(town)
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
	t.Parallel()
	town := t.TempDir()
	// A directory where the file should be: it exists, and reading it fails
	// for every user, root included.
	plist := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.Mkdir(plist, 0o755); err != nil {
		t.Fatal(err)
	}
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	if _, _, err := c.startDaemon(town); err == nil {
		t.Fatal("startDaemon succeeded with an unreadable supervisor file")
	}
	if len(calls.argv) != 0 || calls.spawned {
		t.Fatalf("ran=%v spawned=%v; want neither with an unreadable supervisor file", calls.argv, calls.spawned)
	}
}

// On Linux the provisioned supervisor is a systemd user unit; the same
// decision routes through systemctl.
func TestStartDaemon_UsesSystemdWhenProvisioned(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unit := writeSupervisorFile(t, "gastown-daemon.service", town)
	c, calls := stubSupervisor(t, "linux", unit, loadedJob(), nil)
	via, _, err := c.startDaemon(town)
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

// The owner a systemd unit's start names is the invocation id the unit runs
// now, the same id daemon.DetectSelf reads from the environment there, so the
// guard recognizes a restart of that unit. An id that cannot be read leaves
// the owner's id empty and no error: the state the guard judges on the
// recorded owner's heartbeat and probe (gt-h7iy7).
func TestSystemdSupervisorOwner(t *testing.T) {
	t.Parallel()
	const kind = config.SupervisorSystemd
	rows := []struct {
		name  string
		state templates.SupervisorState
		want  daemon.Owner
	}{
		{
			name:  "the unit's current invocation id",
			state: templates.SupervisorState{Kind: kind, Loaded: true, PID: 4242, LastExit: -1, InvocationID: "inv-2"},
			want:  daemon.Owner{Kind: kind, ID: "inv-2"},
		},
		{
			name:  "a unit reporting no invocation id",
			state: templates.SupervisorState{Kind: kind, Loaded: true, PID: 4242, LastExit: -1},
			want:  daemon.Owner{Kind: kind},
		},
		{
			name:  "a unit whose state cannot be read",
			state: templates.SupervisorState{Kind: kind, LastExit: -1, InvocationID: "inv-2", Err: errors.New("systemctl: failed to connect to bus")},
			want:  daemon.Owner{Kind: kind},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			c := &daemonControl{
				goos:     "linux",
				stateFor: func(string) templates.SupervisorState { return row.state },
			}
			owner, err := c.fileSupervisor("/unit", kind).owner()
			if err != nil {
				t.Fatalf("owner: %v", err)
			}
			if owner != row.want {
				t.Errorf("owner = %+v, want %+v", owner, row.want)
			}
		})
	}
}

// The owner the systemd supervisor reports is the one the real guard judges:
// state.json naming the invocation the unit runs now, under a fresh
// heartbeat, is this unit restarting itself, and any other id is refused
// (gt-h7iy7).
func TestSystemdRestartOwnerPassesTheRealTakeoverGuard(t *testing.T) {
	t.Parallel()
	const running = "inv-2"
	c := &daemonControl{
		goos: "linux",
		stateFor: func(kind string) templates.SupervisorState {
			return templates.SupervisorState{Kind: kind, Loaded: true, PID: 4242, LastExit: -1, InvocationID: running}
		},
	}
	self, err := c.fileSupervisor("/unit", config.SupervisorSystemd).owner()
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	now := time.Now()
	recorded := daemon.Owner{Kind: config.SupervisorSystemd, ID: running}
	in := daemon.TakeoverInput{
		State: &daemon.State{Owner: &recorded, Running: true, LastHeartbeat: now.Add(-time.Minute)},
		Now:   now,
		Self:  self,
	}
	if err := daemon.CheckTakeover(in); err != nil {
		t.Errorf("CheckTakeover for the unit's own invocation = %v, want the restart allowed", err)
	}
	in.Self = daemon.Owner{Kind: config.SupervisorSystemd, ID: "inv-9"}
	if err := daemon.CheckTakeover(in); err == nil {
		t.Error("CheckTakeover allowed a start naming another invocation id")
	}
}

// An OS with no supervisor support spawns directly.
func TestStartDaemon_UnsupportedOSSpawnsDirectly(t *testing.T) {
	t.Parallel()
	c, calls := stubSupervisor(t, "windows", filepath.Join(t.TempDir(), "whatever"), loadedJob(), nil)
	if _, _, err := c.startDaemon(t.TempDir()); err != nil {
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
	t.Parallel()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "town-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", real)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	via, _, err := c.startDaemon(link)
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
	t.Parallel()
	plist := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(plist, []byte("<plist><dict><key>Label</key><string>com.gastown.daemon</string></dict></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	_, _, err := c.startDaemon(t.TempDir())
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	// Holding the lock when stop starts, free once the job has taken the
	// process down.
	checks := 0
	c.isRunning = func(string) (bool, int, error) {
		checks++
		return checks == 1, 4242, nil
	}

	if err := c.stopDaemon(io.Discard, town); err != nil {
		t.Fatalf("runDaemonStop: %v", err)
	}
	if got := calls.joined(); got != "launchctl bootout gui/"+strconv.Itoa(os.Getuid())+"/com.gastown.daemon" {
		t.Fatalf("supervisor command = %q, want launchctl bootout gui/<uid>/com.gastown.daemon", got)
	}
	if calls.stopped {
		t.Fatal("signaled the process although the supervisor job took it down")
	}
}

// With the job unloaded, there is no supervisor to stop: the daemon is the
// only thing left to signal, and nothing should be run against launchd.
func TestRunDaemonStop_UnloadedJobSignalsTheDaemon(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, templates.SupervisorState{Loaded: false, LastExit: -1}, nil)
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	if err := c.stopDaemon(io.Discard, town); err != nil {
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), errors.New("Boot-out failed: 3: No such process"))
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	state := templates.SupervisorState{Err: errors.New("launchctl print: exit status 1")}
	c, calls := stubSupervisor(t, "darwin", plist, state, nil)
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
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
	t.Parallel()
	town := t.TempDir()
	// Present but silent about its town: detectDaemonSupervisor errors rather
	// than answering "no supervisor".
	plist := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(plist, []byte("<plist><dict><key>Label</key><string>com.gastown.daemon</string></dict></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	c.stopDirect = func(string) error { calls.stopped = true; return errors.New("daemon is not running") }
	checks := 0
	c.isRunning = func(string) (bool, int, error) {
		checks++
		return checks < 3, 4242, nil // holding the lock, then gone
	}

	if err := c.stopDaemon(io.Discard, town); err != nil {
		t.Fatalf("runDaemonStop: %v, want no error for a daemon that stopped under the bootout", err)
	}
	if !calls.stopped {
		t.Fatal("did not attempt the direct stop after the bootout")
	}
}

// Still holding the lock after a failed direct stop is a stop that did not
// happen, and it stays an error (gt-ojbb).
func TestRunDaemonStop_StillRunningAfterTheDirectStopIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, _ := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	c.stopDirect = func(string) error { return errors.New("sending termination signal: operation not permitted") }
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("err = %v, want the direct stop failure", err)
	}
}

// gt daemon status names the supervisor through the status line for the PID
// holding the lock, or for none when no daemon runs (the line itself is
// pinned in templates' supervisor_state_test.go).
func TestDaemonStatus_PrintsTheSupervisorLineForTheLockHolder(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{76426, 0} {
		c, _ := stubSupervisor(t, "darwin", "", loadedJob(), nil)
		c.isRunning = func(string) (bool, int, error) { return pid != 0, pid, nil }
		var asked []int
		c.statusLine = func(_ string, lockPID int) string {
			asked = append(asked, lockPID)
			return "launchd (DETACHED - stub)"
		}
		var out strings.Builder
		_ = c.daemonStatus(&out, t.TempDir())
		if len(asked) != 1 || asked[0] != pid {
			t.Errorf("pid %d: status line asked for %v, want [%d]", pid, asked, pid)
		}
		if !strings.Contains(out.String(), "Supervised: launchd (DETACHED - stub)\n") {
			t.Errorf("pid %d: status output = %q, want the supervisor line", pid, out.String())
		}
	}
}

// stubRestart builds the daemonControl restartDaemon runs on and returns a
// recorder. pids is the sequence the lock reports, one entry per read with
// the last repeating: a restart reads the outgoing PID first and then polls
// for a different one, so the sequence is how these tests express "the same
// daemon is still on its way out" versus "a new one is up".
func stubRestart(t *testing.T, goos, file string, pids ...int) (*daemonControl, *supervisorCalls) {
	t.Helper()
	c, calls := stubSupervisor(t, goos, file, loadedJob(), nil)
	c.spawn = func(string, []string) (int, error) { calls.spawned = true; return 9999, nil }
	reads := 0
	c.isRunning = func(string) (bool, int, error) {
		pid := pids[min(reads, len(pids)-1)]
		reads++
		return pid != 0, pid, nil
	}
	return c, calls
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
	t.Parallel()
	town := t.TempDir()
	plist := writeStaleSupervisorFile(t, town)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)

	via, pid, err := c.startDaemon(town)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeStaleSupervisorFile(t, town)
	c, calls := stubRestart(t, "darwin", plist, 1111, 1111, 4242)

	via, pid, err := c.restartDaemon(town)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	before, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := stubSupervisor(t, "darwin", plist, loadedJob(), nil)

	if rewritten := c.syncSupervisorFile(town); rewritten {
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
	t.Parallel()
	town := t.TempDir()
	body, ok, err := templates.SupervisorFileContent("launchd", town, 0)
	if err != nil || !ok {
		t.Fatalf("SupervisorFileContent(launchd, %q) = (ok=%v, err=%v)", town, ok, err)
	}
	plist := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	c.install = func(string, string) error { return errors.New("read-only file system") }

	via, pid, err := c.startDaemon(town)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeStaleSupervisorFile(t, town)
	c, calls := stubRestart(t, "darwin", plist, 1111, 4242)

	note, err := c.reconcileSupervisorJob(town, 1111)
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
	t.Parallel()
	town := t.TempDir()
	body, ok, err := templates.SupervisorFileContent("launchd", town, daemon.ShutdownBudget)
	if err != nil || !ok {
		t.Fatalf("SupervisorFileContent(launchd, %q) = (ok=%v, err=%v)", town, ok, err)
	}
	plist := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, calls := stubRestart(t, "darwin", plist, 1111)

	note, err := c.reconcileSupervisorJob(town, 1111)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubRestart(t, "darwin", plist, 1111, 1111, 4242)

	via, pid, err := c.restartDaemon(town)
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

// A restart of a systemd daemon asks the guard about the invocation the unit
// runs now, read before anything is stopped, so the daemon coming back is this
// unit's own and not a second daemon's start; a guard that refuses that owner
// refuses the restart before any command runs (gt-h7iy7).
func TestRestartDaemon_SystemdNamesTheRunningInvocation(t *testing.T) {
	t.Parallel()
	running := daemon.Owner{Kind: config.SupervisorSystemd, ID: "inv-2"}
	rows := []struct {
		name string
		// allowed is the owner the guard accepts, standing in for the one
		// state.json records when CheckTakeover reads it.
		allowed daemon.Owner
		wantOK  bool
	}{
		{name: "the guard sees the unit's running invocation", allowed: running, wantOK: true},
		{name: "a guard refusing a different owner refuses the restart", allowed: daemon.Owner{Kind: config.SupervisorSystemd, ID: "inv-9"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			unit := writeSupervisorFile(t, "gastown-daemon.service", town)
			c, calls := stubRestart(t, "linux", unit, 1111, 1111, 4242)
			c.stateFor = func(kind string) templates.SupervisorState {
				return templates.SupervisorState{Kind: kind, Loaded: true, PID: 4242, LastExit: -1, InvocationID: "inv-2"}
			}
			var guarded daemon.Owner
			c.guard = func(townRoot string, self daemon.Owner, takeover bool) error {
				guarded = self
				if self != row.allowed {
					return errors.New("state.json records a different owner; refusing to start beside it")
				}
				return nil
			}

			via, _, err := c.restartDaemon(town)
			if guarded != running {
				t.Errorf("guard was asked about %+v, want %+v", guarded, running)
			}
			if !row.wantOK {
				if err == nil {
					t.Fatal("restartDaemon succeeded, want the guard's refusal")
				}
				if len(calls.argv) != 0 || calls.spawned {
					t.Errorf("ran=%v spawned=%v after the refusal, want neither", calls.argv, calls.spawned)
				}
				return
			}
			if err != nil {
				t.Fatalf("restartDaemon: %v", err)
			}
			if got := calls.joined(); got != "systemctl --user restart gastown-daemon.service" {
				t.Errorf("supervisor command = %q, want the systemctl restart", got)
			}
			if via != config.SupervisorSystemd {
				t.Errorf("via = %q, want %q", via, config.SupervisorSystemd)
			}
		})
	}
}

// With no supervisor provisioned the daemon is run by hand, so the restart is
// a hand stop followed by a hand start.
func TestRestartDaemon_HandRestartWithoutASupervisor(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c, calls := stubRestart(t, "darwin", "", 1111, 0)

	via, pid, err := c.restartDaemon(town)
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
	t.Parallel()
	town := t.TempDir()
	c, calls := stubRestart(t, "darwin", "", 1111) // never reports gone

	_, _, err := c.restartDaemon(town)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	// Two stopped reads: the restart's own check, then start's. The third is
	// waitForDaemon seeing the daemon the kickstart brought up.
	c, calls := stubRestart(t, "darwin", plist, 0, 0, 4242)

	via, pid, err := c.restartDaemon(town)
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	// The lock keeps naming the old PID for the whole wait: the restarted
	// daemon never came up (or came up and died immediately).
	c, calls := stubRestart(t, "darwin", plist, 1111)

	if _, _, err := c.restartDaemon(town); err == nil {
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
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	// oldPID, then a stretch of "running but pid unknown yet", then the real
	// new daemon.
	c, calls := stubRestart(t, "darwin", plist, 1111, 1111, 0, 0, 4242)

	via, pid, err := c.restartDaemon(town)
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
// shutdown stage (the Dolt server's own graceful-stop wait, 30s) plus the
// incoming daemon's startup
// preflight — the exact gap the original 10s budget (100 attempts) did not
// cover (gt-oqbw). This simulates a restart that is genuinely slow,
// not stuck: the lock keeps naming the outgoing PID for slowAttempts of
// restartWaitAttempts before the new daemon takes it, comfortably inside the
// budget but well past what the original one allowed. Under the old budget
// this would time out and be misreported as a failed restart although it was
// still in progress.
func TestRestartDaemon_ToleratesASlowShutdownAndRestart(t *testing.T) {
	t.Parallel()
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
	c, calls := stubRestart(t, "darwin", plist, pids...)

	via, pid, err := c.restartDaemon(town)
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
	t.Parallel()
	c, _ := stubSupervisor(t, "darwin", "", templates.SupervisorState{}, nil)

	c.isRunning = func(string) (bool, int, error) { return false, 0, nil }
	err := c.daemonStatus(io.Discard, t.TempDir())
	var silent *SilentExitError
	if !errors.As(err, &silent) || silent.Code != 3 {
		t.Errorf("status with no daemon: err = %v, want SilentExitError code 3", err)
	}

	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }
	if err := c.daemonStatus(io.Discard, t.TempDir()); err != nil {
		t.Errorf("status with a running daemon: err = %v, want nil", err)
	}
}

// scriptedJob stubs the supervisor commands against a job that is loaded until
// its stop runs and loaded again by a bootstrap that succeeds. bootoutErr is
// returned by the stop (which still unloads the job, as launchd does when it
// reports failure for a job it removed); bootstrapFails is how many
// bootstraps fail before one takes.
type scriptedJob struct {
	loaded         bool
	bootoutErr     error
	bootstrapFails int
	bootstraps     int
}

func stubScriptedJob(c *daemonControl, calls *supervisorCalls, job *scriptedJob) {
	c.run = func(a []string) error {
		calls.argv = append(calls.argv, append([]string{}, a...))
		switch a[1] {
		case "bootout":
			job.loaded = false
			return job.bootoutErr
		case "bootstrap":
			job.bootstraps++
			if job.bootstraps <= job.bootstrapFails {
				return errors.New("Bootstrap failed: 5: Input/output error")
			}
			job.loaded = true
		case "kickstart":
			if !job.loaded {
				return errors.New("Could not find service")
			}
		}
		return nil
	}
	c.stateFor = func(kind string) templates.SupervisorState {
		return templates.SupervisorState{Kind: kind, Loaded: job.loaded, LastExit: -1}
	}
}

func provisionedLaunchd(t *testing.T) *daemonSupervisor {
	t.Helper()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, _ := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	sup, err := c.detectDaemonSupervisor(town)
	if err != nil || sup == nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want a launchd supervisor", sup, err)
	}
	return sup
}

// launchd finishes removing a job after bootout returns, and a bootstrap that
// lands in that window fails. The job has to end up loaded anyway: a reload
// that gives up after one try leaves the town with no job and no daemon
// (gt-4k3fj.11).
func TestStartFromFile_RetriesABootstrapThatLosesTheRaceWithTheUnload(t *testing.T) {
	t.Parallel()
	sup := provisionedLaunchd(t)
	calls, job := &supervisorCalls{}, &scriptedJob{loaded: true, bootstrapFails: 3}
	stubScriptedJob(sup.ctl, calls, job)

	if err := sup.startFromFile(); err != nil {
		t.Fatalf("startFromFile: %v", err)
	}
	if !job.loaded {
		t.Error("the job was left unloaded")
	}
	if len(calls.argv) != 5 { // bootout, then three failed bootstraps and the one that took
		t.Errorf("supervisor commands = %v, want a bootout then 4 bootstraps", calls.argv)
	}
}

// A bootout that reports failure for a job launchd did remove must not end
// the reload: the job is gone, so the load still has to happen.
func TestStartFromFile_LoadsAfterABootoutThatFailedButUnloaded(t *testing.T) {
	t.Parallel()
	sup := provisionedLaunchd(t)
	calls, job := &supervisorCalls{}, &scriptedJob{loaded: true, bootoutErr: errors.New("Boot-out failed: 5: Input/output error")}
	stubScriptedJob(sup.ctl, calls, job)

	if err := sup.startFromFile(); err != nil {
		t.Fatalf("startFromFile: %v", err)
	}
	if !job.loaded || job.bootstraps != 1 {
		t.Errorf("loaded=%v bootstraps=%d, want the job loaded by one bootstrap", job.loaded, job.bootstraps)
	}
}

// A bootout that failed and left the job loaded changed nothing: the job is
// still supervising, so this is an error and not a bootstrap beside it.
func TestStartFromFile_BootoutThatLeavesTheJobLoadedIsAnErrorWithoutABootstrap(t *testing.T) {
	t.Parallel()
	sup := provisionedLaunchd(t)
	calls := &supervisorCalls{}
	stubScriptedJob(sup.ctl, calls, &scriptedJob{})
	sup.ctl.run = func(a []string) error {
		calls.argv = append(calls.argv, a)
		return errors.New("boom")
	}
	sup.ctl.stateFor = func(kind string) templates.SupervisorState {
		return templates.SupervisorState{Kind: kind, Loaded: true, LastExit: -1}
	}

	if err := sup.startFromFile(); err == nil {
		t.Fatal("startFromFile succeeded although the unload failed and the job is still loaded")
	}
	if len(calls.argv) != 1 {
		t.Errorf("supervisor commands = %v, want only the failed bootout", calls.argv)
	}
}

// When no bootstrap takes, the error says the daemon has no supervisor rather
// than reporting a start that failed.
func TestStartFromFile_GivesUpLoudlyWhenTheJobWillNotLoad(t *testing.T) {
	t.Parallel()
	sup := provisionedLaunchd(t)
	calls, job := &supervisorCalls{}, &scriptedJob{loaded: true, bootstrapFails: 1 << 30}
	stubScriptedJob(sup.ctl, calls, job)

	err := sup.startFromFile()
	if err == nil || !strings.Contains(err.Error(), "no supervisor") {
		t.Fatalf("startFromFile = %v, want an error naming the missing supervisor", err)
	}
	if job.bootstraps != supervisorLoadAttempts {
		t.Errorf("bootstraps = %d, want %d", job.bootstraps, supervisorLoadAttempts)
	}
}

// A daemon running outside a job the manager does not know is replaced by the
// job, not kickstarted (which fails) and not left beside a job loaded next to
// it (gt-4k3fj.11, gt-3jrm).
func TestRestartDaemon_LoadsAJobTheManagerLostAndReplacesTheDaemonBesideIt(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	// The lock: the hand-run daemon, then free, then the job's daemon.
	c, calls := stubRestart(t, "darwin", plist, 1111, 0, 4242)
	job := &scriptedJob{}
	stubScriptedJob(c, calls, job)

	via, pid, err := c.restartDaemon(town)
	if err != nil {
		t.Fatalf("restartDaemon: %v", err)
	}
	if via != "launchd" || pid != 4242 {
		t.Errorf("restartDaemon = (%q, %d), want (launchd, 4242)", via, pid)
	}
	if !calls.stopped || !job.loaded {
		t.Errorf("stopped=%v loaded=%v, want the old daemon stopped and the job loaded", calls.stopped, job.loaded)
	}
}

// IsRunning reports running with pid 0 while the daemon holds the lock but
// has not written its PID file yet; waitForDaemon must poll past that window
// rather than hand callers a PID of 0 to print (gt-2flp3).
func TestWaitForDaemon_PollsPastPidZero(t *testing.T) {
	t.Parallel()
	type read struct {
		running bool
		pid     int
	}
	tests := []struct {
		name    string
		reads   []read
		wantPID int
		wantErr bool
	}{
		{"pid file already written", []read{{true, 4242}}, 4242, false},
		{"not yet running, then running", []read{{false, 0}, {true, 4242}}, 4242, false},
		{"lock held before pid file", []read{{true, 0}, {true, 4242}}, 4242, false},
		{"lock held for many polls before pid file", []read{{true, 0}, {true, 0}, {true, 0}, {true, 0}, {true, 4242}}, 4242, false},
		{"pid file never written", []read{{true, 0}}, 0, true},
		{"never running", []read{{false, 0}}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			c := &daemonControl{
				sleep: func(time.Duration) {},
				isRunning: func(string) (bool, int, error) {
					r := tt.reads[min(reads, len(tt.reads)-1)]
					reads++
					return r.running, r.pid, nil
				},
			}
			pid, err := c.waitForDaemon(t.TempDir())
			if (err != nil) != tt.wantErr {
				t.Fatalf("waitForDaemon err = %v, wantErr %v", err, tt.wantErr)
			}
			if pid != tt.wantPID {
				t.Errorf("waitForDaemon pid = %d, want %d", pid, tt.wantPID)
			}
		})
	}
}

// writeComposeFile writes the compose file a container town's supervisor
// drives, where the town root says it lives, and returns its path.
func writeComposeFile(t *testing.T, town string) string {
	t.Helper()
	p := composeFilePath(town)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// containerTownConfig is a town's daemon settings for the container kind,
// naming its own container and compose project.
func containerTownConfig(name, project string) *config.DaemonPatrolConfig {
	return &config.DaemonPatrolConfig{
		Supervisor:            config.SupervisorContainer,
		ContainerNameSetting:  name,
		ComposeProjectSetting: project,
	}
}

// stubContainerTown is stubSupervisor whose town sets cfg: Linux with a unit
// path that names no file, so a supervisor could only come from the setting.
func stubContainerTown(t *testing.T, cfg *config.DaemonPatrolConfig) (*daemonControl, *supervisorCalls) {
	t.Helper()
	c, calls := stubSupervisor(t, "linux", filepath.Join(t.TempDir(), "gt.service"), templates.SupervisorState{}, nil)
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) { return cfg, nil }
	return c, calls
}

// inspectArgv is the one docker command the container state read runs.
func inspectArgv(container string) []string {
	return []string{"docker", "--context", defaultDockerContext, "inspect",
		"-f", "{{.State.Status}} {{.Config.Image}}", container}
}

// absentContainer is the run of an inspect docker answered "No such object"
// for: the marker on stderr with the exit, nothing on stdout.
func absentContainer(_ context.Context, argv []string) ([]byte, error) {
	return nil, &templates.DockerInspectError{
		StderrText: "Error: No such object: " + argv[len(argv)-1] + "\n",
		Err:        errors.New("exit status 1"),
	}
}

// The container state read maps what docker inspects into the supervisor
// state, under the town's own container name and the explicit context. An
// absent container is known and not running; a read that failed in any other
// way is unknown, which is never the same as not running, and only stderr can
// say the container is absent (gt-qpeei.18).
func TestContainerJobState(t *testing.T) {
	t.Parallel()
	const name = "gt-town-daemon-1"
	tests := []struct {
		name    string
		stdout  string // the inspect format's output
		stderr  string // docker's diagnostics on a failed run
		err     error  // a runner failure with no docker command behind it
		want    templates.SupervisorState
		wantErr string // a substring of the error, "" when the read has none
	}{
		{"running", "running gt-daemon:abc123\n", "", nil,
			templates.SupervisorState{Loaded: true, Status: "running", Image: "gt-daemon:abc123"}, ""},
		{"exited", "exited gt-daemon:abc123\n", "", nil,
			templates.SupervisorState{Loaded: true, Status: "exited", Image: "gt-daemon:abc123"}, ""},
		{"restarting", "restarting gt-daemon:abc123\n", "", nil,
			templates.SupervisorState{Loaded: true, Status: "restarting", Image: "gt-daemon:abc123"}, ""},
		{"no_such_object", "", "Error: No such object: gt-town-daemon-1\n", nil,
			templates.SupervisorState{}, ""},
		// The marker in stdout is not docker's verdict: a status the format
		// printed can read "No such object" while the read itself failed.
		{"marker_on_stdout_is_not_absent", "Error: No such object: gt-town-daemon-1\n",
			"Cannot connect to the Docker daemon at unix:///var/run/docker.sock\n", nil,
			templates.SupervisorState{}, "exit status 1"},
		{"docker_unreachable", "", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock\n", nil,
			templates.SupervisorState{}, "exit status 1"},
		{"runner_cannot_exec_docker", "", "", &exec.Error{Name: "docker", Err: exec.ErrNotFound},
			templates.SupervisorState{}, "executable file not found"},
		{"unreadable_status", "running\n", "", nil,
			templates.SupervisorState{}, "no status and image"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			read := templates.ContainerJobState(func(_ context.Context, argv []string) ([]byte, error) {
				asked = argv
				switch {
				case tt.stderr != "":
					return []byte(tt.stdout), &templates.DockerInspectError{StderrText: tt.stderr, Err: errors.New("exit status 1")}
				default:
					return []byte(tt.stdout), tt.err
				}
			}, defaultDockerContext, name)

			st := read(config.SupervisorContainer)
			if want := inspectArgv(name); strings.Join(asked, " ") != strings.Join(want, " ") {
				t.Errorf("ran %q, want %q", strings.Join(asked, " "), strings.Join(want, " "))
			}
			if st.Kind != config.SupervisorContainer || st.Container != name || st.LastExit != -1 {
				t.Errorf("state = %+v, want kind container, container %s and LastExit -1", st, name)
			}
			if tt.wantErr == "" {
				if st.Err != nil {
					t.Fatalf("Err = %v, want none", st.Err)
				}
			} else if st.Err == nil || !strings.Contains(st.Err.Error(), tt.wantErr) {
				t.Fatalf("Err = %v, want a failure naming %q", st.Err, tt.wantErr)
			}
			if st.Loaded != tt.want.Loaded || st.Status != tt.want.Status || st.Image != tt.want.Image {
				t.Errorf("state = %+v, want Loaded=%v status %q image %q",
					st, tt.want.Loaded, tt.want.Status, tt.want.Image)
			}
		})
	}
}

// A container town's status reads its own container: the name from the town's
// settings, the same one the compose file and the owner id use, under the
// explicit context — a scratch town never inspects the live container.
func TestDaemonStatus_ContainerTownReadsItsOwnContainer(t *testing.T) {
	t.Parallel()
	const image = "gt-daemon:abc123"
	tests := []struct {
		name  string
		cfg   *config.DaemonPatrolConfig
		finds string // the container the inspect must name
	}{
		{"town naming its own container", containerTownConfig("scratch-daemon-1", "scratch"), "scratch-daemon-1"},
		{"town naming the default", containerTownConfig(config.DefaultContainerName, config.DefaultComposeProject), config.DefaultContainerName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, _ := stubContainerTown(t, tt.cfg)
			c.statusLine = c.supervisedLine
			var asked []string
			c.dockerInspect = func(_ context.Context, argv []string) ([]byte, error) {
				asked = argv
				return []byte("running " + image + "\n"), nil
			}
			c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

			var out strings.Builder
			if err := c.daemonStatus(&out, t.TempDir()); err != nil {
				t.Fatalf("daemonStatus: %v", err)
			}
			if want := inspectArgv(tt.finds); strings.Join(asked, " ") != strings.Join(want, " ") {
				t.Errorf("ran %q, want %q", strings.Join(asked, " "), strings.Join(want, " "))
			}
			want := "Supervised: container " + tt.finds + " running " + image + "\n"
			if !strings.Contains(out.String(), want) {
				t.Errorf("status output = %q, want it to carry %q", out.String(), want)
			}
		})
	}
}

// A container town whose settings do not name a container this binary may act
// on has an unknown state and runs no docker command: the live default is
// never the fallback for a name the town left out (gt-ix1kz).
func TestDaemonStatus_ContainerTownWithoutNamesIsUnknown(t *testing.T) {
	t.Parallel()
	settings := []struct {
		name string
		cfg  *config.DaemonPatrolConfig
	}{
		{"container_name only", &config.DaemonPatrolConfig{
			Supervisor: config.SupervisorContainer, ContainerNameSetting: "scratch-daemon-1"}},
		{"compose_project only", &config.DaemonPatrolConfig{
			Supervisor: config.SupervisorContainer, ComposeProjectSetting: "scratch"}},
		{"a name docker cannot take", &config.DaemonPatrolConfig{
			Supervisor: config.SupervisorContainer, ContainerNameSetting: "-nope", ComposeProjectSetting: "scratch"}},
	}
	for _, tt := range settings {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, _ := stubContainerTown(t, tt.cfg)
			c.statusLine = c.supervisedLine
			ran := false
			c.dockerInspect = func(context.Context, []string) ([]byte, error) {
				ran = true
				return []byte("running gt-daemon:abc123\n"), nil
			}
			c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

			var out strings.Builder
			_ = c.daemonStatus(&out, t.TempDir())
			if ran {
				t.Error("inspected a container the town's settings do not name")
			}
			if !strings.Contains(out.String(), "Supervised: container (unverified - ") {
				t.Errorf("status output = %q, want the container line unverified", out.String())
			}
			if strings.Contains(out.String(), config.DefaultContainerName) {
				t.Errorf("status output = %q, names the live default %s", out.String(), config.DefaultContainerName)
			}
		})
	}
}

// Settings that cannot be read leave the town's supervisor unknown: the file
// reading describes the host's own manager, which a container town does not
// run under, and "none" would claim the town has no supervisor (gt-qpeei.18).
func TestDaemonStatus_UnreadableSettingsIsUnknown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, _ := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) {
		return nil, errors.New("daemon.json: unexpected end of JSON input")
	}
	c.statusLine = c.supervisedLine
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	var out strings.Builder
	if err := c.daemonStatus(&out, town); err != nil {
		t.Fatalf("daemonStatus: %v", err)
	}
	if !strings.Contains(out.String(), "Supervised: unknown (unverified - ") {
		t.Errorf("status output = %q, want the supervisor line unverified", out.String())
	}
	if strings.Contains(out.String(), config.SupervisorLaunchd) {
		t.Errorf("status output = %q, reads the host's launchd for a town whose settings could not be read", out.String())
	}
}

// A container town's restart never falls back to the hand stop: an absent
// container runs no daemon to replace, and the lock PID of a container town is
// not a process this host may signal (gt-qpeei.10).
func TestRestartDaemon_ContainerAbsentContainerIsNotStoppedByHand(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeComposeFile(t, town)
	c, calls := productionContainerControl(containerTownConfig("scratch-daemon-1", "scratch"))
	c.dockerInspect = absentContainer
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	if _, _, err := c.restartDaemon(town); err == nil {
		t.Fatal("restarted although docker could not start the daemon")
	}
	if calls.stopped {
		t.Error("stopped the daemon by hand: a container town's lock PID names no host process")
	}
}

// The same for a start: an absent container is not a job to bootstrap beside a
// failed command, which would bring up the whole compose project.
func TestStartDaemon_ContainerAbsentContainerIsNotBootstrapped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeComposeFile(t, town)
	c, calls := productionContainerControl(containerTownConfig("scratch-daemon-1", "scratch"))
	c.dockerInspect = absentContainer
	c.isRunning = func(string) (bool, int, error) { return false, 0, nil }

	if _, _, err := c.startDaemon(town); err == nil {
		t.Fatal("started although docker could not start the daemon")
	}
	if len(calls.argv) != 1 || !strings.HasSuffix(calls.joined(), "up -d daemon") {
		t.Errorf("ran %v, want the start alone", calls.argv)
	}
	if calls.spawned {
		t.Error("spawned a daemon by hand")
	}
}

// A town that sets daemon.supervisor to container starts its daemon through
// docker compose: the explicit context (never the global one), the town's
// compose project, the compose file, and the daemon service.
func TestStartDaemon_ContainerRunsDockerCompose(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	compose := writeComposeFile(t, town)
	c, calls := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))

	via, pid, err := c.startDaemon(town)
	if err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if via != "container" || pid != 4242 || calls.spawned {
		t.Fatalf("startDaemon = (%q, %d, spawned=%v), want (container, 4242, false)", via, pid, calls.spawned)
	}
	want := "docker --context desktop-linux compose -p scratch -f " + compose + " up -d daemon"
	if got := calls.joined(); got != want {
		t.Errorf("supervisor command = %q, want %q", got, want)
	}
}

// The --context is a field with Docker Desktop's Linux engine as its default,
// so a control that names another context drives that one instead.
func TestStartDaemon_ContainerContextIsAField(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, field, want string }{
		{"default", "", "desktop-linux"},
		{"configured", "scratch-ctx", "scratch-ctx"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			compose := writeComposeFile(t, town)
			c, calls := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
			c.dockerContext = tt.field

			if _, _, err := c.startDaemon(town); err != nil {
				t.Fatalf("startDaemon: %v", err)
			}
			want := "docker --context " + tt.want + " compose -p scratch -f " + compose + " up -d daemon"
			if got := calls.joined(); got != want {
				t.Errorf("supervisor command = %q, want %q", got, want)
			}
		})
	}
}

// A container town with a launchd plist provisioned for it still runs under
// the container: the setting is what the town asked for, and the job launchd
// keeps is the rollback, not the supervisor.
func TestDetectDaemonSupervisor_ContainerNeverReturnsLaunchd(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	writeComposeFile(t, town)
	c, _ := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) {
		return containerTownConfig("scratch-daemon-1", "scratch"), nil
	}

	sup, err := c.detectDaemonSupervisor(town)
	if err != nil || sup == nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want the container supervisor", sup, err)
	}
	if sup.name != "container" {
		t.Errorf("supervisor = %q, want container although launchd is provisioned for this town", sup.name)
	}
}

// The compose file is what the container supervisor drives, so a town that
// names the container kind without one is an error — never "no supervisor",
// which would let a daemon be spawned by hand beside it.
func TestDetectDaemonSupervisor_ContainerWithoutAComposeFileIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))

	sup, err := c.detectDaemonSupervisor(town)
	if err == nil || sup != nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want an error and no supervisor", sup, err)
	}
	if !strings.Contains(err.Error(), composeFilePath(town)) {
		t.Errorf("error = %v, want it to name the missing file %s", err, composeFilePath(town))
	}
}

// A town that does not set both container_name and compose_project reads the
// live daemon's default for whichever it left out, and a scratch town that
// acted on that default would stop or replace the live container. Every
// command that would act on a container refuses instead, before any docker
// argv is built, and the error names both fields and never the live project
// (gt-ix1kz).
func TestContainerSupervisor_RefusesATownWithoutBothContainerSettings(t *testing.T) {
	t.Parallel()
	settings := []struct {
		name string
		cfg  *config.DaemonPatrolConfig
	}{
		{"neither", &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer}},
		{"container_name only", &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer, ContainerNameSetting: "scratch-daemon-1"}},
		{"compose_project only", &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer, ComposeProjectSetting: "scratch"}},
	}
	commands := []struct {
		name  string
		drive func(t *testing.T, c *daemonControl, town string) error
	}{
		{"start", func(t *testing.T, c *daemonControl, town string) error {
			_, _, err := c.startDaemon(town)
			return err
		}},
		{"stop", func(t *testing.T, c *daemonControl, town string) error {
			// Holding the lock and supervised, or the stop returns before it
			// asks the supervisor for anything.
			checks := 0
			c.isRunning = func(string) (bool, int, error) {
				checks++
				return checks == 1, 4242, nil
			}
			c.stateFor = func(kind string) templates.SupervisorState {
				return templates.SupervisorState{Kind: kind, Loaded: true, LastExit: -1}
			}
			return c.stopDaemon(io.Discard, town)
		}},
		{"bootstrap", func(t *testing.T, c *daemonControl, town string) error {
			// The job the manager does not know: the load that follows the
			// unload is a bootstrap, and a bootstrap that refuses is not
			// retried — every attempt would refuse the same way, so waiting
			// between them only delays the error the operator needs.
			c.stateFor = func(kind string) templates.SupervisorState {
				return templates.SupervisorState{Kind: kind, Loaded: false, LastExit: -1}
			}
			waits := 0
			c.sleep = func(time.Duration) { waits++ }
			sup, err := c.detectDaemonSupervisor(town)
			if err != nil {
				t.Fatalf("detectDaemonSupervisor: %v", err)
			}
			err = sup.startFromFile()
			if waits != 0 {
				t.Errorf("waited between %d retries of a refusal, want none", waits)
			}
			return err
		}},
	}
	for _, setting := range settings {
		for _, command := range commands {
			t.Run(setting.name+"/"+command.name, func(t *testing.T) {
				t.Parallel()
				town := t.TempDir()
				writeComposeFile(t, town)
				c, calls := stubContainerTown(t, setting.cfg)

				err := command.drive(t, c, town)
				if err == nil {
					t.Fatal("succeeded although the town does not set both container_name and compose_project")
				}
				for _, field := range []string{"daemon.container_name", "daemon.compose_project"} {
					if !strings.Contains(err.Error(), field) {
						t.Errorf("error = %v, want it to name %s", err, field)
					}
				}
				if strings.Contains(err.Error(), "-p "+config.DefaultComposeProject) {
					t.Errorf("error = %v, points at the live project %s", err, config.DefaultComposeProject)
				}
				if len(calls.argv) != 0 {
					t.Errorf("ran %v, want no docker command at all", calls.argv)
				}
				if calls.spawned {
					t.Error("spawned a daemon by hand")
				}
			})
		}
	}
}

// A container town's supervisor file is its compose file, which this binary
// never rewrites, and the launchd plist the host may still carry is not the
// town's to touch either. Reconciling must write neither.
func TestSyncSupervisorFile_LeavesTheFilesOfAContainerTownAlone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	plist := writeStaleSupervisorFile(t, town)
	compose := writeComposeFile(t, town)
	c, _ := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) {
		return containerTownConfig("scratch-daemon-1", "scratch"), nil
	}
	installs := 0
	c.install = func(path, content string) error {
		installs++
		return installSupervisorFile(path, content)
	}
	before := map[string]string{}
	for _, path := range []string{compose, plist} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = string(body)
	}
	// The control: the same stale plist on a host town is rewritten, so the
	// container town's untouched plist is the branch under test.
	host := t.TempDir()
	hostPlist := writeStaleSupervisorFile(t, host)
	hostControl, _ := stubSupervisor(t, "darwin", hostPlist, loadedJob(), nil)
	if !hostControl.syncSupervisorFile(host) {
		t.Fatal("control: a host town's stale plist was not repaired, so the fixture proves nothing")
	}

	if c.syncSupervisorFile(town) {
		t.Error("rewrote a supervisor file for a container town")
	}
	if installs != 0 {
		t.Errorf("wrote %d supervisor file(s) for a container town, want none", installs)
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s changed although this town runs in a container", path)
		}
	}
}

// syncSupervisorFile reports false, and leaves the file it would have repaired
// as it was, when it cannot make the repair: settings it cannot read, a kind it
// cannot tell, and an install that fails. Each is a warning, never a reason to
// refuse the start that follows.
func TestSyncSupervisorFile_CannotRepairReportsFalseAndWritesNothing(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		patrolConfig func(string) (*config.DaemonPatrolConfig, error)
		installErr   error
		wantInstalls int
	}{
		{
			name: "unreadable settings",
			patrolConfig: func(string) (*config.DaemonPatrolConfig, error) {
				return nil, errors.New("daemon.json: unexpected end of JSON input")
			},
			wantInstalls: 0,
		},
		{
			name: "unknown supervisor kind",
			patrolConfig: func(string) (*config.DaemonPatrolConfig, error) {
				return &config.DaemonPatrolConfig{Supervisor: "kube"}, nil
			},
			wantInstalls: 0,
		},
		{
			name:         "install fails",
			patrolConfig: func(string) (*config.DaemonPatrolConfig, error) { return nil, nil },
			installErr:   errors.New("read-only file system"),
			wantInstalls: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			plist := writeStaleSupervisorFile(t, town)
			before, err := os.ReadFile(plist)
			if err != nil {
				t.Fatal(err)
			}
			c, _ := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
			c.patrolConfig = tt.patrolConfig
			installs := 0
			c.install = func(string, string) error {
				installs++
				return tt.installErr
			}

			if c.syncSupervisorFile(town) {
				t.Error("reported a rewrite it could not make")
			}
			if installs != tt.wantInstalls {
				t.Errorf("installs = %d, want %d", installs, tt.wantInstalls)
			}
			after, err := os.ReadFile(plist)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Error("the supervisor file changed although the repair failed")
			}
		})
	}
}

// productionContainerControl is the host's own daemonControl, so the state
// reader is the production one, with the seams a container town needs faked:
// its settings, the commands it runs, the direct stop and spawn, and the sleep.
func productionContainerControl(cfg *config.DaemonPatrolConfig) (*daemonControl, *supervisorCalls) {
	calls := &supervisorCalls{}
	c := realDaemonControl()
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) { return cfg, nil }
	c.run = func(argv []string) error {
		calls.argv = append(calls.argv, append([]string{}, argv...))
		return errors.New("docker: daemon unreachable")
	}
	c.dockerInspect = func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("docker: daemon unreachable")
	}
	c.stopDirect = func(string) error { calls.stopped = true; return nil }
	c.spawn = func(string, []string) (int, error) { calls.spawned = true; return 1234, nil }
	c.sleep = func(time.Duration) {}
	return c, calls
}

// A container whose state cannot be read is unknown, never "not loaded": a
// failed start that read "not loaded" would be retried as a bootstrap, which
// brings up the whole compose project.
func TestStartDaemon_ContainerFailedStartIsNotBootstrapped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeComposeFile(t, town)
	c, calls := productionContainerControl(containerTownConfig("scratch-daemon-1", "scratch"))
	c.isRunning = func(string) (bool, int, error) { return false, 0, nil }

	if _, _, err := c.startDaemon(town); err == nil {
		t.Fatal("started although docker could not start the daemon")
	}
	if len(calls.argv) != 1 || !strings.HasSuffix(calls.joined(), "up -d daemon") {
		t.Errorf("ran %v, want the start alone: a bootstrap would bring up the whole project", calls.argv)
	}
	if calls.spawned || calls.stopped {
		t.Error("touched the daemon by hand after the container start failed")
	}
}

// A failed container restart must not read "not loaded" either: that reading
// sends the restart to a hand stop of the daemon that is still running.
func TestRestartDaemon_ContainerFailedRestartLeavesTheDaemonAlone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeComposeFile(t, town)
	c, calls := productionContainerControl(containerTownConfig("scratch-daemon-1", "scratch"))
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	if _, _, err := c.restartDaemon(town); err == nil {
		t.Fatal("restarted although docker could not start the daemon")
	}
	if calls.stopped {
		t.Error("stopped the running daemon by hand after the container restart failed")
	}
	// The restart verb (gt-qpeei.39) names the daemon service alone, so a failed
	// restart is still never a bootstrap of the whole project.
	if len(calls.argv) != 1 || !strings.HasSuffix(calls.joined(), "up -d --force-recreate daemon") {
		t.Errorf("ran %v, want the restart alone", calls.argv)
	}
}

// A container town's daemon runs in another namespace, so daemon.lock's PID
// is not a process this host may signal. With the container's state unreadable,
// the stop must refuse instead of falling through to that signal, the mop-up
// the launchd and systemd kinds may use (gt-qpeei.10).
func TestRunDaemonStop_ContainerUnknownStateNeverSignalsTheLockPID(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	compose := writeComposeFile(t, town)
	c, calls := productionContainerControl(containerTownConfig("scratch-daemon-1", "scratch"))
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
	if err == nil {
		t.Fatal("reported a stop although the container job's state is unknown and the daemon still holds the lock")
	}
	if calls.stopped {
		t.Fatal("signaled the lock-file PID of a container town")
	}
	if len(calls.argv) != 0 {
		t.Errorf("ran %v, want no docker command when the job's state is unknown", calls.argv)
	}
	want := "docker --context desktop-linux compose -p scratch -f " + compose + " stop daemon"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to name the stop command %q", err, want)
	}
}

// An absent container needs no supervisor command at all: docker proved the
// daemon runs nowhere of this town's, so the lock is stale — and its PID is
// still not a process this host may signal (gt-qpeei.18).
func TestRunDaemonStop_ContainerAbsentContainerNeverSignalsTheLockPID(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeComposeFile(t, town)
	c, calls := productionContainerControl(containerTownConfig("scratch-daemon-1", "scratch"))
	c.dockerInspect = absentContainer
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
	if err == nil {
		t.Fatal("reported a stop although the lock of a container that does not exist was all that was found")
	}
	if calls.stopped {
		t.Fatal("signaled the stale lock PID of a container town")
	}
	if len(calls.argv) != 0 {
		t.Errorf("ran %v, want no docker command for a container docker reports as absent", calls.argv)
	}
	if !strings.Contains(err.Error(), "scratch-daemon-1") {
		t.Errorf("error = %v, want it to name the absent container", err)
	}
}

// A container job that is loaded but whose stop command fails leaves the
// daemon's fate unproven, and the lock is still not a license to signal: the
// stop reports the failed command and the one that does stop the container.
func TestRunDaemonStop_ContainerFailedStopNeverSignalsTheLockPID(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	compose := writeComposeFile(t, town)
	c, calls := productionContainerControl(containerTownConfig("scratch-daemon-1", "scratch"))
	c.dockerInspect = func(context.Context, []string) ([]byte, error) {
		return []byte("running gt-daemon:abc\n"), nil
	}
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
	if err == nil {
		t.Fatal("reported a stop although the container stop command failed and the daemon still holds the lock")
	}
	if calls.stopped {
		t.Fatal("signaled the lock-file PID of a container town")
	}
	if !strings.Contains(err.Error(), "daemon unreachable") {
		t.Errorf("error = %v, want it to carry the failed stop command", err)
	}
	want := "docker --context desktop-linux compose -p scratch -f " + compose + " stop daemon"
	if got := calls.joined(); got != want {
		t.Errorf("supervisor command = %q, want %q", got, want)
	}
}

// Detection can fail where the kind still decides the stop: a container town
// whose compose file went missing has no supervisor to build, and that error
// is not permission to signal a host PID.
func TestRunDaemonStop_ContainerWithoutAComposeFileNeverSignalsTheLockPID(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c, calls := productionContainerControl(containerTownConfig("scratch-daemon-1", "scratch"))
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
	if err == nil {
		t.Fatal("reported a stop although detection failed and the daemon still holds the lock")
	}
	if calls.stopped {
		t.Fatal("signaled the lock-file PID although the town runs in a container")
	}
	if !strings.Contains(err.Error(), composeFilePath(town)) {
		t.Errorf("error = %v, want it to carry the missing compose file %s", err, composeFilePath(town))
	}
	want := "docker --context desktop-linux compose -p scratch -f " + composeFilePath(town) + " stop daemon"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to name the stop command %q", err, want)
	}
}

// Unreadable settings leave it unknown whether the town runs in a container,
// and an unknown is not the launchd kind: the stop refuses the signal rather
// than guess what the lock's PID belongs to.
func TestRunDaemonStop_UnreadableSettingsNeverSignalTheLockPID(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) {
		return nil, errors.New("daemon.json: unexpected end of JSON input")
	}
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
	if err == nil {
		t.Fatal("reported a stop although the town's daemon settings could not be read")
	}
	if calls.stopped {
		t.Fatal("signaled the lock-file PID without knowing whether the town runs in a container")
	}
	if !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Errorf("error = %v, want it to carry the settings failure", err)
	}
}

// A daemon.supervisor value that is not a kind this binary knows leaves the
// stop unable to say whether the town runs in a container, and that unknown
// refuses the signal the same way an unreadable file does.
func TestRunDaemonStop_UnknownSupervisorKindNeverSignalsTheLockPID(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c, calls := stubSupervisor(t, "darwin", plist, loadedJob(), nil)
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) {
		return &config.DaemonPatrolConfig{Supervisor: "kube"}, nil
	}
	c.isRunning = func(string) (bool, int, error) { return true, 4242, nil }

	err := c.stopDaemon(io.Discard, town)
	if err == nil {
		t.Fatal("reported a stop although daemon.supervisor names a kind this binary does not know")
	}
	if calls.stopped {
		t.Fatal("signaled the lock-file PID without knowing the town's supervisor kind")
	}
	if !strings.Contains(err.Error(), "daemon.supervisor") {
		t.Errorf("error = %v, want it to name daemon.supervisor", err)
	}
}

// A setting this host has no manager for is refused rather than answered with
// the host's own: driving systemd for a town that asked for launchd is not
// what the setting says.
func TestDetectDaemonSupervisor_RefusesAKindThisHostCannotRun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c, _ := stubSupervisor(t, "darwin", filepath.Join(t.TempDir(), "com.gastown.daemon.plist"), loadedJob(), nil)
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) {
		return &config.DaemonPatrolConfig{Supervisor: config.SupervisorSystemd}, nil
	}

	sup, err := c.detectDaemonSupervisor(town)
	if err == nil || sup != nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want an error and no supervisor", sup, err)
	}
}

// Unreadable settings are unknown, not "sets none": detection stops instead of
// falling back to the host's own supervisor, which could hand-spawn a daemon
// beside one the town asked for.
func TestDetectDaemonSupervisor_UnreadableSettingsAreAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c, _ := stubSupervisor(t, "darwin", filepath.Join(t.TempDir(), "com.gastown.daemon.plist"), loadedJob(), nil)
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) {
		return nil, errors.New("daemon.json: unexpected end of JSON input")
	}

	sup, err := c.detectDaemonSupervisor(town)
	if err == nil || sup != nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want an error and no supervisor", sup, err)
	}
}

// A daemon.supervisor value that is not one of the accepted kinds is refused
// by detection rather than read as unset: an operator who typed a kind must
// not be handed the host's own manager without being told.
func TestDetectDaemonSupervisor_RefusesAnUnknownKind(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c, _ := stubSupervisor(t, "darwin", filepath.Join(t.TempDir(), "com.gastown.daemon.plist"), loadedJob(), nil)
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) {
		return &config.DaemonPatrolConfig{Supervisor: "kube"}, nil
	}

	sup, err := c.detectDaemonSupervisor(town)
	if err == nil || sup != nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want an error and no supervisor", sup, err)
	}
	if !strings.Contains(err.Error(), "daemon.supervisor") {
		t.Errorf("error = %v, want it to name daemon.supervisor", err)
	}
}

// A container setting docker cannot take is a refusal like the missing one:
// the town set one field and inherits the live value for the other, so the
// command stops before it runs.
func TestStartDaemon_ContainerRefusesSettingsDockerCannotTake(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeComposeFile(t, town)
	c, calls := stubContainerTown(t, &config.DaemonPatrolConfig{
		Supervisor:           config.SupervisorContainer,
		ContainerNameSetting: "-not a container name",
	})

	_, _, err := c.startDaemon(town)
	if err == nil {
		t.Fatal("started although container_name is set alone and outside the shape docker takes")
	}
	for _, field := range []string{"daemon.container_name", "daemon.compose_project"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error = %v, want it to name %s", err, field)
		}
	}
	if len(calls.argv) != 0 {
		t.Errorf("ran %v, want no docker command at all", calls.argv)
	}
}

// A restart whose supervisor command fails and whose daemon then does not come
// back reports the failure and how to load the job: a daemon that never
// returned is not a restart that worked.
func TestRestartDaemon_SupervisorFailureWithoutANewDaemonIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	// The lock keeps naming the outgoing PID for the whole wait.
	c, calls := stubRestart(t, "darwin", plist, 1111)
	c.run = func(a []string) error {
		calls.argv = append(calls.argv, append([]string{}, a...))
		return errors.New("Could not find service")
	}

	if _, _, err := c.restartDaemon(town); err == nil {
		t.Fatal("restartDaemon reported success although launchd failed and no new daemon took the lock")
	} else if !strings.Contains(err.Error(), "could not restart the daemon") {
		t.Errorf("error = %v, want it to report the failed restart", err)
	}
	if !strings.Contains(calls.joined(), "kickstart") {
		t.Errorf("the restart was never attempted: %v", calls.argv)
	}
}

// The container supervisor's restart is `up -d --force-recreate daemon` on the
// same context, project and file as its start: `up -d` alone does not recreate
// an unchanged service, so a restart on the same image would do nothing
// (gt-qpeei.39). The file kinds have no restart verb of their own.
func TestContainerSupervisorRestartRecreatesTheService(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	compose := writeComposeFile(t, town)
	c, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	sup, err := c.detectDaemonSupervisor(town)
	if err != nil || sup == nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want the container supervisor", sup, err)
	}

	got, err := sup.restart()
	if err != nil {
		t.Fatalf("restart builder: %v", err)
	}
	want := []string{"docker", "--context", defaultDockerContext, "compose", "-p", "scratch", "-f", compose, "up", "-d", "--force-recreate", "daemon"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("restart argv = %v, want %v", got, want)
	}
	start, err := sup.start()
	if err != nil {
		t.Fatalf("start builder: %v", err)
	}
	// Everything but the verb is the start's: the same context, project and file.
	withoutFlag := make([]string, 0, len(got))
	for _, a := range got {
		if a != "--force-recreate" {
			withoutFlag = append(withoutFlag, a)
		}
	}
	if strings.Join(withoutFlag, " ") != strings.Join(start, " ") {
		t.Errorf("restart argv %v differs from the start argv %v by more than --force-recreate", got, start)
	}

	launchd := c.fileSupervisor("/plist", config.SupervisorLaunchd)
	systemd := c.fileSupervisor("/unit", config.SupervisorSystemd)
	if launchd.restart != nil || systemd.restart != nil {
		t.Error("a file kind has a restart builder: launchd and systemd restart with their start")
	}
}

// containerRestartRig is a running container town whose restart is proven from
// state.json: the readings state.json gives in order (the last repeats; the
// first is the baseline read before the restart), what docker says about the
// container, and what the restart command records.
type containerRestartRig struct {
	c         *daemonControl
	calls     *supervisorCalls
	town      string
	readings  []stateReading
	reads     int
	sleeps    int
	dockerOut string
	dockerErr error
}

type stateReading struct {
	state *daemon.State
	err   error
}

var restartBaselineStart = time.Date(2026, 10, 30, 6, 0, 0, 0, time.UTC)

func containerOwnerState(start time.Time, running bool) *daemon.State {
	owner := daemon.Owner{Kind: config.SupervisorContainer, ID: "scratch-daemon-1"}
	return &daemon.State{Running: running, Owner: &owner, StartedAt: start}
}

func newContainerRestartRig(t *testing.T, readings ...stateReading) *containerRestartRig {
	t.Helper()
	r := &containerRestartRig{readings: readings, dockerOut: "running scratch-image\n"}
	r.town = startGuardTown(t)
	writeComposeFile(t, r.town)
	r.c, r.calls = stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	r.c.guard = guardProbe(neverProbed(t))
	r.c.isRunning = func(string) (bool, int, error) { return true, 1111, nil }
	r.c.sleep = func(time.Duration) { r.sleeps++ }
	r.c.readState = func(string) (*daemon.State, error) {
		i := min(r.reads, len(r.readings)-1)
		r.reads++
		return r.readings[i].state, r.readings[i].err
	}
	r.c.dockerInspect = func(context.Context, []string) ([]byte, error) {
		if r.dockerErr != nil {
			return nil, r.dockerErr
		}
		return []byte(r.dockerOut), nil
	}
	return r
}

// restartDaemon over a running container runs the restart builder and never the
// start builder, with nothing in the environment (a restart is not a takeover),
// and judges the result by state.json, so there is no pid to return
// (gt-qpeei.40): the owner is the town's container, running, with a start time
// later than the one recorded before the restart.
func TestRestartDaemon_ContainerRunsTheRestartBuilderNotTheStart(t *testing.T) {
	t.Parallel()
	later := restartBaselineStart.Add(time.Minute)
	r := newContainerRestartRig(t,
		stateReading{state: containerOwnerState(restartBaselineStart, true)}, // the baseline
		stateReading{state: containerOwnerState(later, true)})

	via, pid, err := r.c.restartDaemon(r.town)
	if err != nil {
		t.Fatalf("restartDaemon: %v", err)
	}
	if via != config.SupervisorContainer || pid != 0 {
		t.Errorf("restartDaemon = (%q, %d), want (container, 0): a container restart has no pid", via, pid)
	}
	if len(r.calls.argv) != 1 {
		t.Fatalf("commands = %v, want exactly the restart", r.calls.argv)
	}
	if got := r.calls.joined(); !strings.HasSuffix(got, " up -d --force-recreate daemon") {
		t.Errorf("command = %q, want the --force-recreate restart", got)
	}
	if r.calls.envs[0] != nil {
		t.Errorf("the restart carried environment %v, want none", r.calls.envs[0])
	}
}

// The restart is proven only by state.json and docker together. Each of these
// readings keeps the wait going to the end of the budget on the fake sleeps and
// ends in the timeout that names the reading and prints the logs command; none
// is ever success. A test that treats an unknown state as success fails here.
func TestRestartDaemon_ContainerRestartIsNotProvenByAnythingLess(t *testing.T) {
	t.Parallel()
	later := restartBaselineStart.Add(time.Minute)
	good := stateReading{state: containerOwnerState(later, true)}
	baseline := stateReading{state: containerOwnerState(restartBaselineStart, true)}
	launchdOwner := containerOwnerState(later, true)
	other := daemon.Owner{Kind: config.SupervisorLaunchd, ID: templates.LaunchdLabel}
	launchdOwner.Owner = &other
	cases := []struct {
		name      string
		readings  []stateReading
		dockerOut string
		dockerErr error
		want      string
	}{
		{"the same start time", []stateReading{baseline, baseline}, "running img\n", nil, "still records the start"},
		{"an earlier start time", []stateReading{baseline, {state: containerOwnerState(restartBaselineStart.Add(-time.Hour), true)}}, "running img\n", nil, "still records the start"},
		{"another owner with a later start", []stateReading{baseline, {state: launchdOwner}}, "running img\n", nil, "names owner"},
		{"the container's daemon recorded not running", []stateReading{baseline, {state: containerOwnerState(later, false)}}, "running img\n", nil, "not running"},
		{"state.json unreadable", []stateReading{baseline, {err: errors.New("read failed")}}, "running img\n", nil, "cannot be read"},
		{"state.json missing", []stateReading{baseline, {}}, "running img\n", nil, "is missing"},
		{"docker state unknown", []stateReading{baseline, good}, "", errors.New("docker: unreachable"), "container's state cannot be read"},
		{"docker reports it exited", []stateReading{baseline, good}, "exited img\n", nil, "does not report the container running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newContainerRestartRig(t, tc.readings...)
			r.dockerOut, r.dockerErr = tc.dockerOut, tc.dockerErr

			_, _, err := r.c.restartDaemon(r.town)

			if err == nil || !strings.Contains(err.Error(), "did not take the town") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want the timeout naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "docker --context "+defaultDockerContext+" logs scratch-daemon-1") {
				t.Errorf("error lacks the docker logs command: %v", err)
			}
			if r.sleeps != restartWaitAttempts {
				t.Errorf("waited %d polls, want the whole budget of %d", r.sleeps, restartWaitAttempts)
			}
		})
	}
}

// The wait goes on past stale readings and passes when the new start shows up,
// and a restart command that reports an error after the container came up is
// still success, as it is for the other kinds.
func TestRestartDaemon_ContainerWaitPassesWhenTheNewStartAppears(t *testing.T) {
	t.Parallel()
	stale := stateReading{state: containerOwnerState(restartBaselineStart, true)}
	fresh := stateReading{state: containerOwnerState(restartBaselineStart.Add(time.Minute), true)}
	for _, runErr := range []error{nil, errors.New("docker: exit 1")} {
		r := newContainerRestartRig(t, stale, stale, stale, stale, fresh)
		if runErr != nil {
			recorded := r.c.run
			r.c.run = func(a []string) error { recorded(a); return runErr }
		}
		via, pid, err := r.c.restartDaemon(r.town)
		if err != nil || via != config.SupervisorContainer || pid != 0 {
			t.Fatalf("runErr=%v: restartDaemon = (%q, %d, %v), want success without a pid", runErr, via, pid, err)
		}
		if r.sleeps != 4 {
			t.Errorf("runErr=%v: waited %d polls, want 3 stale polls and then the one that sees the new start", runErr, r.sleeps)
		}
	}
}

// A restart whose command failed and whose container never took the town is the
// "could not restart" error with the load hint, not a bare timeout.
func TestRestartDaemon_ContainerFailedCommandAndNoNewStartIsAnError(t *testing.T) {
	t.Parallel()
	stale := stateReading{state: containerOwnerState(restartBaselineStart, true)}
	r := newContainerRestartRig(t, stale)
	recorded := r.c.run
	r.c.run = func(a []string) error { recorded(a); return errors.New("docker: exit 1") }

	_, _, err := r.c.restartDaemon(r.town)

	if err == nil || !strings.Contains(err.Error(), "could not restart the daemon") {
		t.Fatalf("error = %v, want the could-not-restart error", err)
	}
}

// A state.json that cannot be read before the restart refuses it: with no
// recorded start the new daemon could not be told from the old one. A missing
// file records no start, so any start passes.
func TestRestartDaemon_ContainerBaselineReadFailureRefusesBeforeRestarting(t *testing.T) {
	t.Parallel()
	r := newContainerRestartRig(t, stateReading{err: errors.New("read failed")})

	_, _, err := r.c.restartDaemon(r.town)

	if err == nil || !strings.Contains(err.Error(), "nothing was restarted") {
		t.Fatalf("error = %v, want a refusal that nothing was restarted", err)
	}
	if len(r.calls.argv) != 0 {
		t.Errorf("a refused restart ran %v", r.calls.argv)
	}

	missing := newContainerRestartRig(t, stateReading{}, stateReading{state: containerOwnerState(restartBaselineStart, true)})
	if _, _, err := missing.c.restartDaemon(missing.town); err != nil {
		t.Errorf("a restart with no state.json before it = %v, want success on the first start recorded", err)
	}
}

// The restart's output names a pid only when there is one.
func TestRestartedLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		via  string
		pid  int
		want string
	}{
		{"container", 0, "Daemon restarted under container"},
		{"launchd", 4242, "Daemon restarted under launchd (PID 4242)"},
		{"", 7, "Daemon restarted (PID 7)"},
	} {
		if got := restartedLine(tc.via, tc.pid); got != tc.want {
			t.Errorf("restartedLine(%q, %d) = %q, want %q", tc.via, tc.pid, got, tc.want)
		}
	}
}

// With container_name and compose_project unset the restart builder refuses with
// errSupervisorRefused and nothing runs, so a scratch town can never recreate
// the live container (gt-ix1kz).
func TestContainerSupervisorRestartRefusesAnUnsetTown(t *testing.T) {
	t.Parallel()
	town := startGuardTown(t)
	writeComposeFile(t, town)
	c, calls := stubContainerTown(t, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer})
	sup, err := c.detectDaemonSupervisor(town)
	if err != nil || sup == nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want the container supervisor", sup, err)
	}

	err = sup.runArgv(sup.restart)

	if !errors.Is(err, errSupervisorRefused) || !errors.Is(err, errContainerSettingsUnset) {
		t.Fatalf("restart error = %v, want errSupervisorRefused wrapping the unset-settings refusal", err)
	}
	if len(calls.argv) != 0 {
		t.Errorf("a refused restart ran %v", calls.argv)
	}

	c.isRunning = func(string) (bool, int, error) { return true, 1111, nil }
	if _, _, err := c.restartDaemon(town); !errors.Is(err, errContainerSettingsUnset) {
		t.Errorf("restartDaemon error = %v, want the unset-settings refusal; a town that sets neither container field must not be recreated", err)
	}
	if len(calls.argv) != 0 {
		t.Errorf("restartDaemon on an unset town ran %v", calls.argv)
	}
}

// The restart proof needs the owner the restarted daemon will name, and a town
// that sets neither container field has none: the proof is refused rather than
// built on the live container's default name. (restartDaemon asks the guard
// first, which refuses the same town, so this reaches the function directly.)
func TestContainerRestartProofRefusesAnUnsetTown(t *testing.T) {
	t.Parallel()
	town := startGuardTown(t)
	writeComposeFile(t, town)
	c, _ := stubContainerTown(t, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer})
	sup, err := c.detectDaemonSupervisor(town)
	if err != nil || sup == nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want the container supervisor", sup, err)
	}
	c.readState = func(string) (*daemon.State, error) {
		t.Error("state.json was read for a town whose container the settings do not name")
		return nil, nil
	}

	if _, err := c.containerRestartProofFor(town, sup); !errors.Is(err, errContainerSettingsUnset) {
		t.Fatalf("containerRestartProofFor error = %v, want the unset-settings refusal", err)
	}
}
