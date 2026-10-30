package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// startGuardNow is the clock the guard reads in these tests.
var startGuardNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// startGuardOther is the owner a hand-run daemon recorded in state.json: it
// is none of the supervisors the start paths below start under, so a fresh
// heartbeat of it is another owner's.
var startGuardOther = daemon.Owner{Kind: daemon.OwnerKindManual, ID: "other-host:7"}

// startGuardLaunchd is the recorded owner of a launchd daemon.
var startGuardLaunchd = daemon.Owner{Kind: config.SupervisorLaunchd, ID: templates.LaunchdLabel}

// guardProbe wraps daemon.GuardStart with the fixed clock and the given probe,
// the shape of the control's guard seam. Everything else is the real guard.
func guardProbe(probe daemon.OwnerProbe) func(string, daemon.Owner, bool) error {
	return func(townRoot string, self daemon.Owner, takeover bool) error {
		return daemon.GuardStart(townRoot, self, takeover, probe, startGuardNow)
	}
}

// neverProbed fails the test if the guard reads an owner, for the starts the
// guard decides on the files alone.
func neverProbed(t *testing.T) daemon.OwnerProbe {
	t.Helper()
	return func(owner daemon.Owner) (bool, bool) {
		t.Errorf("guard probed %+v, but the verdict needs no read of an owner", owner)
		return false, false
	}
}

// probeAnswers reports every owner as running and known as given.
func probeAnswers(running, known bool) daemon.OwnerProbe {
	return func(daemon.Owner) (bool, bool) { return running, known }
}

// startGuardTown is a town root with the daemon directory a start expects.
func startGuardTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	return town
}

func writeTownDaemonFile(t *testing.T, town, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(town, "daemon", name), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeTownState records state as the town's state.json.
func writeTownState(t *testing.T, town string, state daemon.State) {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	writeTownDaemonFile(t, town, "state.json", data)
}

// freshHeartbeatOf is a running state whose owner beat a minute ago.
func freshHeartbeatOf(owner daemon.Owner) daemon.State {
	return daemon.State{Running: true, Owner: &owner, LastHeartbeat: startGuardNow.Add(-time.Minute)}
}

// writeCurrentSupervisorFile writes the plist this binary would render for
// town, so a start finds the file current and rewrites nothing.
func writeCurrentSupervisorFile(t *testing.T, town string) string {
	t.Helper()
	body, ok, err := templates.SupervisorFileContent("launchd", town, daemon.ShutdownBudget)
	if err != nil || !ok {
		t.Fatalf("SupervisorFileContent(launchd, %q) = (ok=%v, err=%v)", town, ok, err)
	}
	p := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// startPath is one way the CLI starts a daemon, built on a fresh town. run
// drives it with the real startup guard (the control's probe seam given by the
// test) and launched reports whether anything was started: a supervisor argv
// or a direct spawn ran, or the supervisor file was provisioned.
type startPath struct {
	name  string
	build func(t *testing.T, town string, probe daemon.OwnerProbe) (run func() error, launched func() bool)
}

// startPaths are the CLI start paths R1 lists (gt-qpeei.28): gt daemon start
// under each kind of supervisor and by hand, gt daemon restart over a running
// daemon, the job reload gt up makes (gt up's ensureDaemon calls startDaemon
// for a stopped daemon, which is the first three rows, and
// reconcileSupervisorJob for a running one), and enable-supervisor.
func startPaths() []startPath {
	supervised := func(c *daemonControl, calls *supervisorCalls) func() bool {
		return func() bool { return len(calls.argv) > 0 || calls.spawned }
	}
	return []startPath{
		{
			name: "gt daemon start under launchd",
			build: func(t *testing.T, town string, probe daemon.OwnerProbe) (func() error, func() bool) {
				c, calls := stubSupervisor(t, "darwin", writeSupervisorFile(t, "com.gastown.daemon.plist", town), loadedJob(), nil)
				c.guard = guardProbe(probe)
				return func() error { _, _, err := c.startDaemon(town); return err }, supervised(c, calls)
			},
		},
		{
			name: "gt daemon start by hand",
			build: func(t *testing.T, town string, probe daemon.OwnerProbe) (func() error, func() bool) {
				c, calls := stubSupervisor(t, "plan9", "", templates.SupervisorState{}, nil)
				c.guard = guardProbe(probe)
				return func() error { _, _, err := c.startDaemon(town); return err }, supervised(c, calls)
			},
		},
		{
			name: "gt daemon start in a container",
			build: func(t *testing.T, town string, probe daemon.OwnerProbe) (func() error, func() bool) {
				writeComposeFile(t, town)
				c, calls := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
				c.guard = guardProbe(probe)
				return func() error { _, _, err := c.startDaemon(town); return err }, supervised(c, calls)
			},
		},
		{
			name: "gt daemon restart over a running daemon",
			build: func(t *testing.T, town string, probe daemon.OwnerProbe) (func() error, func() bool) {
				c, calls := stubRestart(t, "darwin", writeCurrentSupervisorFile(t, town), 1111, 4242)
				c.guard = guardProbe(probe)
				return func() error { _, _, err := c.restartDaemon(town); return err }, supervised(c, calls)
			},
		},
		{
			name: "gt up reloading a stale job",
			build: func(t *testing.T, town string, probe daemon.OwnerProbe) (func() error, func() bool) {
				c, calls := stubRestart(t, "darwin", writeStaleSupervisorFile(t, town), 1111, 4242)
				c.guard = guardProbe(probe)
				return func() error { _, err := c.reconcileSupervisorJob(town, 1111); return err }, supervised(c, calls)
			},
		},
		{
			name: "gt daemon enable-supervisor",
			build: func(t *testing.T, town string, probe daemon.OwnerProbe) (func() error, func() bool) {
				c, _ := stubSupervisor(t, "darwin", filepath.Join(t.TempDir(), "com.gastown.daemon.plist"), templates.SupervisorState{}, nil)
				c.guard = guardProbe(probe)
				provisioned := false
				provision := func(string, time.Duration) (string, error) { provisioned = true; return "", nil }
				run := func() error {
					return enableSupervisor(town, provision, func(townRoot string) error {
						return c.guardStart(townRoot, c.hostSupervisorJob(), false)
					})
				}
				return run, func() bool { return provisioned }
			},
		},
	}
}

// TestStartPathsAskTheStartupGuard pins, for every start path, the verdict of
// the town files the guard reads: another owner's fresh heartbeat, a leftover
// lock with no state, and an unparsable state all refuse with nothing launched,
// and a town with neither file goes on. A path that stops asking the guard
// launches in the refusing rows and turns its row red.
func TestStartPathsAskTheStartupGuard(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name    string
		setup   func(t *testing.T, town string)
		wantErr string // "" means the start goes on
	}{
		{
			name: "a fresh heartbeat from another owner refuses",
			setup: func(t *testing.T, town string) {
				writeTownState(t, town, freshHeartbeatOf(startGuardOther))
			},
			wantErr: "takeover window",
		},
		{
			name: "a leftover daemon.lock with no state.json refuses",
			setup: func(t *testing.T, town string) {
				writeTownDaemonFile(t, town, "daemon.lock", nil)
			},
			wantErr: "state.json is missing",
		},
		{
			name:    "neither file goes on",
			setup:   func(*testing.T, string) {},
			wantErr: "",
		},
		{
			name: "an unparsable state.json refuses",
			setup: func(t *testing.T, town string) {
				writeTownDaemonFile(t, town, "state.json", []byte("{not json"))
			},
			wantErr: "does not parse",
		},
	}
	for _, path := range startPaths() {
		for _, sc := range scenarios {
			t.Run(path.name+"/"+sc.name, func(t *testing.T) {
				t.Parallel()
				town := startGuardTown(t)
				sc.setup(t, town)
				run, launched := path.build(t, town, neverProbed(t))

				err := run()

				if sc.wantErr == "" {
					if err != nil {
						t.Fatalf("start refused a town with no daemon file: %v", err)
					}
					if !launched() {
						t.Fatal("a start the guard allows launched nothing")
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), sc.wantErr) {
					t.Fatalf("start error = %v, want it to contain %q", err, sc.wantErr)
				}
				if launched() {
					t.Fatal("the guard refused, but the path launched a daemon (supervisor argv, spawn or provision ran)")
				}
			})
		}
	}
}

// A supervisor error refuses the start. The job state cannot be read, the
// compose file is gone, the settings name no container, or the supervisor
// command fails: none of them falls back to a hand spawn of gt daemon run
// beside the supervisor (gt-3jrm). A fallback added to any row turns it red.
func TestStartDaemonSupervisorErrorNeverFallsBackToASpawn(t *testing.T) {
	t.Parallel()
	notRunning := func(string) (bool, int, error) { return false, 0, nil }
	cases := []struct {
		name  string
		build func(t *testing.T, town string) (*daemonControl, *supervisorCalls)
	}{
		{
			name: "the supervisor command fails",
			build: func(t *testing.T, town string) (*daemonControl, *supervisorCalls) {
				return stubSupervisor(t, "darwin", writeSupervisorFile(t, "com.gastown.daemon.plist", town), loadedJob(), errors.New("launchctl: boom"))
			},
		},
		{
			name: "the container's compose file is missing",
			build: func(t *testing.T, town string) (*daemonControl, *supervisorCalls) {
				return stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
			},
		},
		{
			name: "the container town sets neither container field",
			build: func(t *testing.T, town string) (*daemonControl, *supervisorCalls) {
				writeComposeFile(t, town)
				return stubContainerTown(t, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer})
			},
		},
		{
			name: "the container supervisor command fails",
			build: func(t *testing.T, town string) (*daemonControl, *supervisorCalls) {
				writeComposeFile(t, town)
				c, calls := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
				c.run = func(a []string) error { calls.argv = append(calls.argv, a); return errors.New("docker: boom") }
				return c, calls
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := startGuardTown(t)
			c, calls := tc.build(t, town)
			c.isRunning = notRunning
			c.guard = guardProbe(neverProbed(t))

			_, _, err := c.startDaemon(town)

			if err == nil {
				t.Fatal("startDaemon succeeded although the supervisor failed")
			}
			if calls.spawned {
				t.Fatal("startDaemon spawned gt daemon run by hand after a supervisor error")
			}
		})
	}
}

// guardSelfFor records the owner each supervisor kind hands the guard: the one
// the daemon it starts will name for itself, so the guard judges the start the
// way Run will.
func TestStartGuardNamesTheOwnerTheStartedDaemonWillBe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		build   func(t *testing.T, town string) *daemonControl
		want    daemon.Owner
		wantErr string
	}{
		{
			name: "launchd",
			build: func(t *testing.T, town string) *daemonControl {
				c, _ := stubSupervisor(t, "darwin", writeSupervisorFile(t, "com.gastown.daemon.plist", town), loadedJob(), nil)
				return c
			},
			want: startGuardLaunchd,
		},
		{
			name: "a container naming its own container",
			build: func(t *testing.T, town string) *daemonControl {
				writeComposeFile(t, town)
				c, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
				return c
			},
			want: daemon.Owner{Kind: config.SupervisorContainer, ID: "scratch-daemon-1"},
		},
		{
			name: "no supervisor",
			build: func(t *testing.T, town string) *daemonControl {
				c, _ := stubSupervisor(t, "plan9", "", templates.SupervisorState{}, nil)
				return c
			},
			want: daemon.Owner{Kind: daemon.OwnerKindManual},
		},
		{
			// gt-ix1kz: the live container's name must never be read off a
			// default, so a container town without both fields refuses before
			// the guard is asked about anyone.
			name: "a container town without its fields is refused before the guard",
			build: func(t *testing.T, town string) *daemonControl {
				writeComposeFile(t, town)
				c, _ := stubContainerTown(t, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer})
				return c
			},
			wantErr: "daemon.container_name and daemon.compose_project",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := startGuardTown(t)
			c := tc.build(t, town)
			var got *daemon.Owner
			c.guard = func(_ string, self daemon.Owner, _ bool) error { got = &self; return nil }
			sup, err := c.detectDaemonSupervisor(town)
			if err != nil {
				t.Fatalf("detectDaemonSupervisor: %v", err)
			}

			err = c.guardStart(town, sup, false)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("guardStart error = %v, want it to contain %q", err, tc.wantErr)
				}
				if got != nil {
					t.Fatalf("the guard was asked about %+v for a town the supervisor refused", *got)
				}
				return
			}
			if err != nil {
				t.Fatalf("guardStart: %v", err)
			}
			if got == nil || *got != tc.want {
				t.Fatalf("guard asked about %v, want %+v", got, tc.want)
			}
		})
	}
}

// gt daemon start --takeover over another owner's fresh heartbeat. In a
// container the variable rides in the environment of the docker compose
// process, through the env-carrying seam, and only when the owner is shown not
// running; a running or unknown owner refuses with nothing run. The plain argv
// seam carries no environment, and a start without --takeover passes none.
func TestStartDaemonTakeoverInAContainer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		probe    daemon.OwnerProbe
		takeover bool
		wantErr  string
	}{
		{name: "owner not running goes on", probe: probeAnswers(false, true), takeover: true},
		{name: "owner running refuses", probe: probeAnswers(true, true), takeover: true, wantErr: "still running"},
		{name: "owner unknown refuses", probe: probeAnswers(false, false), takeover: true, wantErr: "is unknown"},
		{name: "no takeover refuses before any probe", probe: nil, takeover: false, wantErr: "takeover window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := startGuardTown(t)
			writeTownState(t, town, freshHeartbeatOf(startGuardLaunchd))
			writeComposeFile(t, town)
			c, calls := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
			probe := tc.probe
			if probe == nil {
				probe = neverProbed(t)
			}
			c.guard = guardProbe(probe)

			_, _, err := c.startDaemonWith(town, tc.takeover)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("startDaemonWith error = %v, want it to contain %q", err, tc.wantErr)
				}
				if len(calls.argv) != 0 {
					t.Fatalf("a refused takeover ran %v", calls.argv)
				}
				return
			}
			if err != nil {
				t.Fatalf("startDaemonWith: %v", err)
			}
			if len(calls.argv) != 1 {
				t.Fatalf("compose argv = %v, want exactly one (up -d daemon)", calls.argv)
			}
			if got := strings.Join(calls.argv[0][len(calls.argv[0])-3:], " "); got != "up -d daemon" {
				t.Errorf("compose subcommand = %q, want up -d daemon", got)
			}
			if got := calls.envs[0]; len(got) != 1 || got[0] != "GT_DAEMON_TAKEOVER=1" {
				t.Errorf("compose environment = %v, want [GT_DAEMON_TAKEOVER=1]", got)
			}
		})
	}
}

// A start without --takeover goes through the plain argv seam, which carries no
// environment: the variable is never ambient.
func TestStartDaemonWithoutTakeoverCarriesNoEnvironment(t *testing.T) {
	t.Parallel()
	town := startGuardTown(t)
	writeComposeFile(t, town)
	c, calls := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	c.guard = guardProbe(neverProbed(t))

	if _, _, err := c.startDaemon(town); err != nil {
		t.Fatalf("startDaemon: %v", err)
	}
	if len(calls.argv) != 1 || calls.envs[0] != nil {
		t.Fatalf("argv %v with environments %v, want one argv through the plain seam", calls.argv, calls.envs)
	}
}

// --takeover is refused under launchd and systemd, whatever the owner: the job
// cannot carry the variable into the daemon and loading it beside a recorded
// owner is what a takeover must not do. The message says how to boot the job
// out, and nothing runs.
func TestStartDaemonTakeoverRefusedUnderAFileSupervisor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		goos string
		file string
		want string
	}{
		{"launchd", "darwin", "com.gastown.daemon.plist", "launchctl bootout gui/"},
		{"systemd", "linux", "gastown-daemon.service", "systemctl --user stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := startGuardTown(t)
			writeTownState(t, town, freshHeartbeatOf(startGuardOther))
			c, calls := stubSupervisor(t, tc.goos, writeSupervisorFile(t, tc.file, town), loadedJob(), nil)
			c.guard = func(string, daemon.Owner, bool) error {
				t.Error("the guard was asked about a takeover the file kind refuses outright")
				return nil
			}

			_, _, err := c.startDaemonWith(town, true)

			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "without --takeover") {
				t.Fatalf("startDaemonWith error = %v, want a refusal that names %q and says to start without --takeover", err, tc.want)
			}
			if len(calls.argv) != 0 || calls.spawned {
				t.Fatalf("a refused takeover ran %v (spawned=%v)", calls.argv, calls.spawned)
			}
		})
	}
}

// A hand-run start with --takeover hands the child the variable its own guard
// reads; without it the child gets none.
func TestStartDaemonTakeoverByHandCarriesTheVariableToTheChild(t *testing.T) {
	t.Parallel()
	for _, takeover := range []bool{true, false} {
		town := startGuardTown(t)
		writeTownState(t, town, daemon.State{Running: false, Owner: &startGuardOther, LastHeartbeat: startGuardNow.Add(-time.Hour)})
		c, calls := stubSupervisor(t, "plan9", "", templates.SupervisorState{}, nil)
		c.guard = guardProbe(probeAnswers(false, true))

		if _, _, err := c.startDaemonWith(town, takeover); err != nil {
			t.Fatalf("takeover=%v: startDaemonWith: %v", takeover, err)
		}
		var want []string
		if takeover {
			want = []string{"GT_DAEMON_TAKEOVER=1"}
		}
		if strings.Join(calls.spawnEnv, " ") != strings.Join(want, " ") {
			t.Errorf("takeover=%v: spawn environment = %v, want %v", takeover, calls.spawnEnv, want)
		}
	}
}

// GuardStart's verdict for a daemon.log the CLI did not write is Run's: the
// log is evidence of a prior run. This is the one difference between the two
// callers of the guard, and it is read from the file here.
func TestRealStartGuardReadsTheTownsFiles(t *testing.T) {
	t.Parallel()
	town := startGuardTown(t)
	self := daemon.Owner{Kind: config.SupervisorLaunchd, ID: templates.LaunchdLabel}
	if err := realStartGuard(town, self, false); err != nil {
		t.Fatalf("realStartGuard on a town with no daemon file: %v", err)
	}
	writeTownDaemonFile(t, town, "daemon.log", []byte("an earlier run\n"))
	if err := realStartGuard(town, self, false); err == nil || !strings.Contains(err.Error(), "state.json is missing") {
		t.Fatalf("realStartGuard with a daemon.log and no state.json = %v, want the missing-state refusal", err)
	}
}
