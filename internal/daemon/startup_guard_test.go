package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/templates"
)

// guardNow is the clock every startup-guard test reads.
var guardNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// guardSelf is the owner a hand-run start names in these tests.
var guardSelf = Owner{Kind: OwnerKindManual, ID: "fixture-host:4242"}

// guardLaunchd is the owner the town's launchd job records.
var guardLaunchd = Owner{Kind: agentconfig.SupervisorLaunchd, ID: templates.LaunchdLabel}

// guardTown makes a town root with the daemon directory a start expects.
func guardTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	return town
}

// writeGuardFile writes body to name under the town's daemon directory.
func writeGuardFile(t *testing.T, town, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(town, "daemon", name), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedGuardState records state as the town's state.json and returns the bytes
// it wrote, so a test can show the file was left alone.
func seedGuardState(t *testing.T, town string, state State) []byte {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	writeGuardFile(t, town, "state.json", data)
	return data
}

// ownedBy is the state.json owner field for owner.
func ownedBy(owner Owner) *Owner { return &owner }

// answers is a probe that reports every owner as running and known as given.
func answers(running, known bool) func(*testing.T) OwnerProbe {
	return func(*testing.T) OwnerProbe {
		return func(Owner) (bool, bool) { return running, known }
	}
}

// unreachableProbe fails the test if the guard reads any owner, for a start
// the guard must decide without asking.
func unreachableProbe(t *testing.T) OwnerProbe {
	t.Helper()
	return func(owner Owner) (bool, bool) {
		t.Errorf("guard probed %+v, but this start is decided without reading an owner", owner)
		return false, false
	}
}

// TestGuardStartup pins the startup guard's verdict on each town state a start
// can meet (gt-qpeei.27): a nil error lets the start go on.
func TestGuardStartup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		setup         func(t *testing.T, town string)
		takeover      bool
		logPreexisted bool
		probe         func(t *testing.T) OwnerProbe
		wantErr       string // "" means the start may go on
	}{
		{
			name:  "no state and no other daemon file starts",
			probe: unreachableProbe,
		},
		{
			name: "a leftover daemon.lock with no state refuses",
			setup: func(t *testing.T, town string) {
				writeGuardFile(t, town, "daemon.lock", nil)
			},
			probe:   unreachableProbe,
			wantErr: "state.json is missing",
		},
		{
			name:          "a daemon.log from an earlier run with no state refuses",
			logPreexisted: true,
			probe:         unreachableProbe,
			wantErr:       "state.json is missing",
		},
		{
			// New writes the patrol config line to daemon.log before Run, so a
			// log this start made is not evidence of a prior run.
			name: "a daemon.log this start wrote does not count as a prior run",
			setup: func(t *testing.T, town string) {
				writeGuardFile(t, town, "daemon.log", []byte("Loaded patrol config\n"))
			},
			probe: unreachableProbe,
		},
		{
			name: "a fresh heartbeat from another owner refuses without reading it",
			setup: func(t *testing.T, town string) {
				seedGuardState(t, town, State{Running: true, Owner: ownedBy(guardLaunchd), LastHeartbeat: guardNow.Add(-time.Minute)})
			},
			probe:   unreachableProbe,
			wantErr: "takeover window",
		},
		{
			name: "a takeover passes a fresh heartbeat whose owner the probe shows stopped",
			setup: func(t *testing.T, town string) {
				seedGuardState(t, town, State{Running: true, Owner: ownedBy(guardLaunchd), LastHeartbeat: guardNow.Add(-time.Minute)})
			},
			takeover: true,
			probe:    answers(false, true),
		},
		{
			name: "a takeover refuses a fresh heartbeat whose owner is still running",
			setup: func(t *testing.T, town string) {
				seedGuardState(t, town, State{Running: true, Owner: ownedBy(guardLaunchd), LastHeartbeat: guardNow.Add(-time.Minute)})
			},
			takeover: true,
			probe:    answers(true, true),
			wantErr:  "still running",
		},
		{
			name: "a clean stop by an owner the probe shows stopped starts",
			setup: func(t *testing.T, town string) {
				seedGuardState(t, town, State{Running: false, Owner: ownedBy(guardLaunchd), LastHeartbeat: guardNow.Add(-time.Hour)})
			},
			probe: answers(false, true),
		},
		{
			name: "a clean stop by an owner the probe cannot read refuses",
			setup: func(t *testing.T, town string) {
				seedGuardState(t, town, State{Running: false, Owner: ownedBy(guardLaunchd), LastHeartbeat: guardNow.Add(-time.Hour)})
			},
			probe:   answers(false, false),
			wantErr: "is unknown",
		},
		{
			name: "the start that recorded this owner restarts itself",
			setup: func(t *testing.T, town string) {
				seedGuardState(t, town, State{Running: true, Owner: ownedBy(guardSelf), LastHeartbeat: guardNow.Add(-time.Minute)})
			},
			probe: unreachableProbe,
		},
		{
			// A state.json that cannot be read at all (here a directory) refuses
			// before any owner is judged.
			name: "a state.json that cannot be read refuses without reading an owner",
			setup: func(t *testing.T, town string) {
				if err := os.MkdirAll(StateFile(town), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			takeover: true,
			probe:    unreachableProbe,
			wantErr:  "state.json",
		},
		{
			// The candidates are named from the town's daemon settings, so a
			// settings file that does not parse refuses rather than guessing them.
			name: "a daemon settings file that does not parse refuses",
			setup: func(t *testing.T, town string) {
				path := PatrolConfigFile(town)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			probe:   unreachableProbe,
			wantErr: "refusing to start: reading",
		},
		{
			// A state that does not parse names no owner. The refusal must come
			// before CheckTakeover, which would probe the candidates here.
			name: "an unparsable state.json refuses without reading an owner",
			setup: func(t *testing.T, town string) {
				writeGuardFile(t, town, "state.json", []byte("{not json"))
			},
			takeover: true,
			probe:    unreachableProbe,
			wantErr:  "does not parse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := guardTown(t)
			if tc.setup != nil {
				tc.setup(t, town)
			}
			err := guardStartup(town, guardSelf, tc.takeover, tc.probe(t), guardNow, tc.logPreexisted)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("guardStartup = %v, want the start to go on", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("guardStartup = %v, want a refusal containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestStartupCandidates pins the owners a legacy takeover must show stopped
// (gt-kpan5): the launchd job always, and the town's container only when the
// town's daemon settings name one, never from the default name.
func TestStartupCandidates(t *testing.T) {
	t.Parallel()
	const named = "scratch-town-daemon-1"
	cases := []struct {
		name     string
		settings string // written to PatrolConfigFile(town); "" writes no file
		want     []Owner
		wantErr  string // "" means the candidates are returned
	}{
		{
			name: "a town that names its container adds it after launchd",
			settings: `{"type":"daemon-patrol-config","version":1,` +
				`"container_name":"` + named + `","compose_project":"scratch-town"}`,
			want: []Owner{guardLaunchd, {Kind: agentconfig.SupervisorContainer, ID: named}},
		},
		{
			// The default name could be another town's container, so settings
			// that set other fields but no container_name name launchd alone.
			name:     "settings that name no container leave launchd alone",
			settings: `{"type":"daemon-patrol-config","version":1,"env":{"GT_SEAT_REFILL_MAX_PRIORITY":"50"}}`,
			want:     []Owner{guardLaunchd},
		},
		{
			name: "a town with no settings file names launchd alone",
			want: []Owner{guardLaunchd},
		},
		{
			name:     "a settings file that does not parse refuses",
			settings: `{not json`,
			wantErr:  "refusing to start: reading",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := guardTown(t)
			if tc.settings != "" {
				writeDaemonSettings(t, town, tc.settings)
			}
			got, err := startupCandidates(town)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("startupCandidates = %v, %v; want a refusal containing %q", got, err, tc.wantErr)
				}
				if got != nil {
					t.Fatalf("startupCandidates = %v with a refusal; want no candidates", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("startupCandidates = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("startupCandidates = %v, want %v", got, tc.want)
			}
		})
	}
}

// writeDaemonSettings writes body as the town's daemon settings file, in the
// shape ReadPatrolConfig decodes.
func writeDaemonSettings(t *testing.T, town, body string) {
	t.Helper()
	path := PatrolConfigFile(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// unreadJob fails the test if the probe reads a service manager job for an
// owner that is not judged from one.
func unreadJob(t *testing.T) templates.SupervisorReader {
	return func(kind string) templates.SupervisorState {
		t.Errorf("probe read the %s job, but this owner is not judged from it", kind)
		return templates.SupervisorState{Kind: kind, LastExit: -1, Err: errors.New("unexpected job read")}
	}
}

// jobAnswers is a service manager reader that reports st for any job.
func jobAnswers(st templates.SupervisorState) templates.SupervisorReader {
	return func(string) templates.SupervisorState { return st }
}

// TestStartupOwnerProbe pins what the guard's probe reports for each kind of
// recorded owner. Unknown never reads as stopped.
func TestStartupOwnerProbe(t *testing.T) {
	t.Parallel()
	container := Owner{Kind: agentconfig.SupervisorContainer, ID: "scratch-town-daemon-1"}
	launchd := guardLaunchd
	dockerAnswers := func(out string, err error) templates.ContainerDocker {
		return func(context.Context, []string) ([]byte, error) { return []byte(out), err }
	}
	absent := &templates.DockerInspectError{
		StderrText: "Error: No such object: scratch-town-daemon-1",
		Err:        errors.New("exit status 1"),
	}
	host := fixtureHostname("fixture-host")
	cases := []struct {
		name    string
		owner   Owner
		docker  templates.ContainerDocker
		host    func() (string, error)
		job     templates.SupervisorReader // nil: the job must not be read
		running bool
		known   bool
	}{
		{
			name: "a running container is running", owner: container,
			docker: dockerAnswers("running gastown:latest", nil), host: host,
			running: true, known: true,
		},
		{
			name: "a restarting container is running", owner: container,
			docker: dockerAnswers("restarting gastown:latest", nil), host: host,
			running: true, known: true,
		},
		{
			name: "a paused container is running", owner: container,
			docker: dockerAnswers("paused gastown:latest", nil), host: host,
			running: true, known: true,
		},
		{
			name: "an exited container is stopped", owner: container,
			docker: dockerAnswers("exited gastown:latest", nil), host: host,
			known: true,
		},
		{
			name: "a created container that never ran is stopped", owner: container,
			docker: dockerAnswers("created gastown:latest", nil), host: host,
			known: true,
		},
		{
			name: "a dead container is stopped", owner: container,
			docker: dockerAnswers("dead gastown:latest", nil), host: host,
			known: true,
		},
		{
			name: "a launchd job with a pid is running", owner: launchd,
			docker: dockerAnswers("", nil), host: host,
			job:     jobAnswers(templates.SupervisorState{Kind: "launchd", Loaded: true, PID: 4242, LastExit: -1}),
			running: true, known: true,
		},
		{
			name: "a loaded launchd job without a pid is stopped", owner: launchd,
			docker: dockerAnswers("", nil), host: host,
			job:   jobAnswers(templates.SupervisorState{Kind: "launchd", Loaded: true, LastExit: 0}),
			known: true,
		},
		{
			name: "a launchd job the manager does not know is stopped", owner: launchd,
			docker: dockerAnswers("", nil), host: host,
			job:   jobAnswers(templates.SupervisorState{Kind: "launchd", LastExit: -1}),
			known: true,
		},
		{
			name: "a launchd job the probe cannot read is unknown", owner: launchd,
			docker: dockerAnswers("", nil), host: host,
			job: jobAnswers(templates.SupervisorState{Kind: "launchd", LastExit: -1, Err: errors.New("launchctl unavailable")}),
		},
		{
			name:   "a manual owner whose host cannot be named is unknown",
			owner:  Owner{Kind: OwnerKindManual, ID: fmt.Sprintf("fixture-host:%d", os.Getpid())},
			docker: dockerAnswers("", nil),
			host:   func() (string, error) { return "", errors.New("no hostname") },
		},
		{
			name:  "a manual owner whose pid is not a number is unknown",
			owner: Owner{Kind: OwnerKindManual, ID: "fixture-host:abc"},
			host:  host,
		},
		{
			name:  "a manual owner whose pid is zero is unknown",
			owner: Owner{Kind: OwnerKindManual, ID: "fixture-host:0"},
			host:  host,
		},
		{
			name: "a container docker does not know is stopped", owner: container,
			docker: dockerAnswers("", absent), host: host,
			known: true,
		},
		{
			name: "a container docker cannot read is unknown", owner: container,
			docker: dockerAnswers("", errors.New("docker unavailable")), host: host,
		},
		{
			name: "a container status outside the known set is unknown", owner: container,
			docker: dockerAnswers("removing gastown:latest", nil), host: host,
		},
		{
			name:  "a manual owner whose pid is alive on this host is running",
			owner: Owner{Kind: OwnerKindManual, ID: fmt.Sprintf("fixture-host:%d", os.Getpid())},
			host:  host, running: true, known: true,
		},
		{
			// No process has a pid this large, so the signal probe finds none.
			name:  "a manual owner whose pid is gone is stopped",
			owner: Owner{Kind: OwnerKindManual, ID: "fixture-host:2147483600"},
			host:  host, known: true,
		},
		{
			name:  "a manual owner on another host is unknown",
			owner: Owner{Kind: OwnerKindManual, ID: "other-host:4242"},
			host:  host,
		},
		{
			name:  "a manual owner with no pid is unknown",
			owner: Owner{Kind: OwnerKindManual, ID: "fixture-host"},
			host:  host,
		},
		{
			// Another app's XPC label names no job of this town, so it is not
			// read from the service manager.
			name:  "a launchd owner under another label is unknown",
			owner: Owner{Kind: agentconfig.SupervisorLaunchd, ID: "application.x.y"},
			host:  host,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := tc.job
			if job == nil {
				job = unreadJob(t)
			}
			probe := startupOwnerProbe(tc.docker, tc.host, job)
			running, known := probe(tc.owner)
			if running != tc.running || known != tc.known {
				t.Fatalf("probe(%+v) = running %v known %v, want running %v known %v",
					tc.owner, running, known, tc.running, tc.known)
			}
		})
	}
}

// The systemd owner is the invocation id of the run it names. The probe judges
// that owner by the unit's state only while the unit runs that same invocation
// (gt-nirga).
func TestStartupOwnerProbeSystemd(t *testing.T) {
	t.Parallel()
	const invocation = "8f3c0a1e2b4d4c6e9a7f1b2c3d4e5f60"
	owner := Owner{Kind: agentconfig.SupervisorSystemd, ID: invocation}
	unit := func(st templates.SupervisorState) templates.SupervisorReader {
		return func(kind string) templates.SupervisorState {
			if kind != agentconfig.SupervisorSystemd {
				t.Errorf("read(%q), want the systemd job", kind)
			}
			return st
		}
	}
	cases := []struct {
		name    string
		owner   Owner
		state   templates.SupervisorState
		running bool
		known   bool
	}{
		{
			name: "the unit runs the owner's invocation", owner: owner,
			state:   templates.SupervisorState{Kind: "systemd", Loaded: true, PID: 1234, InvocationID: invocation},
			running: true, known: true,
		},
		{
			name: "the unit's invocation is the owner's but it is stopped", owner: owner,
			state: templates.SupervisorState{Kind: "systemd", Loaded: true, InvocationID: invocation},
			known: true,
		},
		{
			name: "the unit runs a different invocation", owner: owner,
			state: templates.SupervisorState{Kind: "systemd", Loaded: true, PID: 1234, InvocationID: "0123456789abcdef0123456789abcdef"},
		},
		{
			name: "the unit's invocation cannot be read", owner: owner,
			state: templates.SupervisorState{Kind: "systemd", Loaded: true, PID: 1234, Err: errors.New("systemctl failed")},
		},
		{
			name: "the unit reports no invocation", owner: owner,
			state: templates.SupervisorState{Kind: "systemd", Loaded: true, PID: 1234},
		},
		{
			name:  "an empty owner id never matches an empty invocation",
			owner: Owner{Kind: agentconfig.SupervisorSystemd},
			state: templates.SupervisorState{Kind: "systemd", Loaded: true, PID: 1234},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := startupOwnerProbe(nil, fixtureHostname("fixture-host"), unit(tc.state))
			running, known := probe(tc.owner)
			if running != tc.running || known != tc.known {
				t.Fatalf("probe(%+v) = running %v known %v, want running %v known %v",
					tc.owner, running, known, tc.running, tc.known)
			}
		})
	}
}

// A start over a fresh heartbeat from another owner returns the guard's refusal
// and writes no pid file, lock or state (gt-qpeei.27).
func TestRunRefusesAFreshHeartbeatFromAnotherOwner(t *testing.T) {
	t.Parallel()
	town := guardTown(t)
	before := seedGuardState(t, town, State{Running: true, Owner: ownedBy(guardLaunchd), LastHeartbeat: guardNow.Add(-time.Minute)})
	cfg := DefaultConfig(town)
	d := &Daemon{
		config:     cfg,
		logger:     log.New(io.Discard, "", 0),
		getenv:     ownerEnv(nil),
		hostname:   fixtureHostname("fixture-host"),
		clock:      clockwork.NewFakeClockAt(guardNow),
		ownerProbe: unreachableProbe(t),
	}
	// A start that got past the guard would run the whole loop; a canceled
	// context ends it at the first select instead of parking the test.
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()

	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "takeover window") {
		t.Fatalf("Run = %v, want the guard's refusal over the fresh heartbeat", err)
	}
	if _, err := os.Stat(cfg.PidFile); !os.IsNotExist(err) {
		t.Errorf("Run wrote %s before refusing", cfg.PidFile)
	}
	if _, err := os.Stat(filepath.Join(town, "daemon", "daemon.lock")); !os.IsNotExist(err) {
		t.Errorf("Run wrote daemon.lock before refusing")
	}
	if got, _ := os.ReadFile(StateFile(town)); !bytes.Equal(got, before) {
		t.Errorf("Run changed state.json before refusing")
	}
}

// A start on a town with no daemon files at all starts, and records itself
// as the owner (gt-qpeei.27).
func TestRunStartsOnATownWithNoDaemonFiles(t *testing.T) {
	t.Parallel()
	cfg, finish := startParkedRun(t, nil)
	defer func() { _ = finish() }()

	state, err := LoadState(cfg.TownRoot)
	if err != nil {
		t.Fatalf("LoadState after the start = %v", err)
	}
	if state.Owner == nil || state.Owner.Kind != OwnerKindManual {
		t.Fatalf("state.json owner = %+v after the start, want the hand-run owner that started", state.Owner)
	}
}

// A start over a leftover daemon.lock with no state.json refuses before any
// write (gt-qpeei.27).
func TestRunRefusesALeftoverLockWithNoState(t *testing.T) {
	t.Parallel()
	town := guardTown(t)
	writeGuardFile(t, town, "daemon.lock", nil)
	cfg := DefaultConfig(town)
	d := &Daemon{
		config:     cfg,
		logger:     log.New(io.Discard, "", 0),
		getenv:     ownerEnv(nil),
		hostname:   fixtureHostname("fixture-host"),
		ownerProbe: unreachableProbe(t),
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()

	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "state.json is missing") {
		t.Fatalf("Run = %v, want a refusal over the leftover lock", err)
	}
	if _, err := os.Stat(cfg.PidFile); !os.IsNotExist(err) {
		t.Errorf("Run wrote %s before refusing", cfg.PidFile)
	}
	if _, err := os.Stat(StateFile(town)); !os.IsNotExist(err) {
		t.Errorf("Run wrote state.json before refusing")
	}
}

// A start over a state.json that does not parse refuses without reading an
// owner (gt-qpeei.27).
func TestRunRefusesAnUnparsableStateWithoutReadingAnOwner(t *testing.T) {
	t.Parallel()
	town := guardTown(t)
	before := []byte("{not json")
	writeGuardFile(t, town, "state.json", before)
	cfg := DefaultConfig(town)
	d := &Daemon{
		config:     cfg,
		logger:     log.New(io.Discard, "", 0),
		getenv:     ownerEnv(nil),
		hostname:   fixtureHostname("fixture-host"),
		ownerProbe: unreachableProbe(t),
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()

	err := d.Run()
	if err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("Run = %v, want a refusal over the unparsable state", err)
	}
	if _, err := os.Stat(cfg.PidFile); !os.IsNotExist(err) {
		t.Errorf("Run wrote %s before refusing", cfg.PidFile)
	}
	if got, _ := os.ReadFile(StateFile(town)); !bytes.Equal(got, before) {
		t.Errorf("Run changed state.json before refusing")
	}
}

// runCanceledStart runs a start on town with env and probe under a canceled
// context, so a start that gets past the guard ends at its first select.
func runCanceledStart(t *testing.T, town string, env map[string]string, probe OwnerProbe) error {
	t.Helper()
	d := &Daemon{
		config:     DefaultConfig(town),
		logger:     log.New(io.Discard, "", 0),
		getenv:     ownerEnv(env),
		hostname:   fixtureHostname("fixture-host"),
		clock:      clockwork.NewFakeClockAt(guardNow),
		ownerProbe: probe,
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()
	return d.Run()
}

// GT_DAEMON_TAKEOVER reaches the guard from Run: set to "1", a fresh heartbeat
// whose owner the probe shows stopped starts, and one whose owner still runs
// refuses. Unset, the guard refuses before it reads any owner (gt-qpeei.27).
func TestRunTakeoverEnvReachesTheGuard(t *testing.T) {
	t.Parallel()
	fresh := State{Running: true, Owner: ownedBy(guardLaunchd), LastHeartbeat: guardNow.Add(-time.Minute)}

	t.Run("takeover with the owner stopped starts", func(t *testing.T) {
		t.Parallel()
		town := guardTown(t)
		seedGuardState(t, town, fresh)
		_, finish := startParkedRunIn(t, town, map[string]string{agentconfig.TakeoverEnv: "1"}, func(Owner) (bool, bool) { return false, true })
		defer func() { _ = finish() }()

		state, err := LoadState(town)
		if err != nil {
			t.Fatalf("LoadState after the start = %v", err)
		}
		if state.Owner == nil || state.Owner.Kind != OwnerKindManual {
			t.Fatalf("state.json owner = %+v after the takeover, want the hand-run owner that started", state.Owner)
		}
	})

	t.Run("takeover with the owner still running refuses", func(t *testing.T) {
		t.Parallel()
		town := guardTown(t)
		before := seedGuardState(t, town, fresh)
		err := runCanceledStart(t, town, map[string]string{agentconfig.TakeoverEnv: "1"}, func(Owner) (bool, bool) { return true, true })
		if err == nil || !strings.Contains(err.Error(), "still running") {
			t.Fatalf("Run = %v, want the probe's refusal that the owner is still running", err)
		}
		if got, _ := os.ReadFile(StateFile(town)); !bytes.Equal(got, before) {
			t.Errorf("Run changed state.json before refusing")
		}
	})

	t.Run("without takeover the guard refuses before reading the owner", func(t *testing.T) {
		t.Parallel()
		town := guardTown(t)
		seedGuardState(t, town, fresh)
		err := runCanceledStart(t, town, nil, unreachableProbe(t))
		if err == nil || !strings.Contains(err.Error(), "takeover window") {
			t.Fatalf("Run = %v, want the takeover-window refusal without the takeover env", err)
		}
	})
}

// A start after a clean stop by another owner that the probe shows not
// running goes on, and its state names this start as the owner (gt-qpeei.27).
func TestRunStartsAfterACleanStopTheProbeShowsStopped(t *testing.T) {
	t.Parallel()
	town := guardTown(t)
	seedGuardState(t, town, State{Running: false, Owner: ownedBy(guardLaunchd), LastHeartbeat: guardNow.Add(-time.Hour)})
	_, finish := startParkedRunIn(t, town, nil, func(Owner) (bool, bool) { return false, true })
	defer func() { _ = finish() }()

	state, err := LoadState(town)
	if err != nil {
		t.Fatalf("LoadState after the start = %v", err)
	}
	if state.Owner == nil || state.Owner.Kind != OwnerKindManual {
		t.Fatalf("state.json owner = %+v after the start, want the hand-run owner that started", state.Owner)
	}
}
