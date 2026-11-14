package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/templates"
)

// podRecorded is the container owner state.json records, the one a pod's start is
// judged against.
var podRecorded = Owner{Kind: agentconfig.SupervisorContainer, ID: "scratch-town-daemon-1"}

// podSelf is the owner the pod names for itself: container mode, its own name.
var podSelf = Owner{Kind: agentconfig.SupervisorContainer, ID: "gastown-daemon-pod"}

// podEngine is the container runner a pod has: no engine answers, so every
// docker read fails with stderr that is not docker's "No such object" verdict.
func podEngine(context.Context, []string) ([]byte, error) {
	return nil, &templates.DockerInspectError{
		StderrText: "Cannot connect to the Docker daemon",
		Err:        errors.New("exit status 1"),
	}
}

// engineReports is a container runner whose engine answers with out.
func engineReports(out string) templates.ContainerDocker {
	return func(context.Context, []string) ([]byte, error) { return []byte(out), nil }
}

// engineErrors is a container runner whose engine is reachable but whose read
// fails with an error that is not docker's "No such object" verdict. The probe
// reads it as unknown, the same as an absent engine.
func engineErrors(context.Context, []string) ([]byte, error) {
	return nil, &templates.DockerInspectError{
		StderrText: "Error response from daemon: server error",
		Err:        errors.New("exit status 1"),
	}
}

// unreadableJobs is a service manager reader that cannot read any job.
func unreadableJobs(kind string) templates.SupervisorState {
	return templates.SupervisorState{Kind: kind, LastExit: -1, Err: errors.New("job unreadable")}
}

// podProbe is the probe a pod's start asks: the production probe with the given
// container runner and job reader. A container owner is never judged from the
// job reader, so a container test passes unreadJob(t), which fails the test if
// the probe reads a job for it.
func podProbe(t *testing.T, run templates.ContainerDocker, jobs templates.SupervisorReader) OwnerProbe {
	t.Helper()
	return startupOwnerProbe(run, fixtureHostname("gastown-pod"), jobs)
}

// runPodStart runs Run as the pod names itself on town, with the pod's engine
// unreachable, and returns the daemon log Run wrote and Run's error. A start that
// gets past the guard ends at its first select, because the context is canceled.
// The start-time host work is stubbed as startParkedRunIn stubs it, so the run
// touches no tmux session and no patrol.
func runPodStart(t *testing.T, town string, env map[string]string) (string, error) {
	t.Helper()
	kv := map[string]string{
		agentconfig.ContainerModeEnv: agentconfig.SupervisorContainer,
		agentconfig.ContainerNameEnv: podSelf.ID,
	}
	for k, v := range env {
		kv[k] = v
	}
	var logs bytes.Buffer
	d := &Daemon{
		config:     DefaultConfig(town),
		logger:     log.New(&logs, "", 0),
		getenv:     ownerEnv(kv),
		hostname:   fixtureHostname("gastown-pod"),
		clock:      clockwork.NewFakeClockAt(guardNow),
		ownerProbe: podProbe(t, podEngine, unreadJob(t)),
	}
	d.disabledPatrols = map[string]bool{
		"landing_infra":    true,
		"resource_sampler": true,
		"spec_dispatch":    true,
	}
	d.seams.heartbeatWork = func(*Daemon, *State, bool) {}
	d.seams.cleanupLegacySockets = func(string) (int, int) { return 0, 0 }
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()
	err := d.Run()
	return logs.String(), err
}

// A pod with no engine refuses to start over a recorded container owner, and the
// refusal names that owner, says this daemon cannot ask an engine, and names
// GT_DAEMON_TAKEOVER and the cut-over step that sets it. It never reads as an
// engine failure to repair.
func TestStartupOwnerContainerUnknownRefusalNamesTheTakeover(t *testing.T) {
	t.Parallel()
	// A fresh heartbeat refuses on the takeover window before any owner is read,
	// so only the states that reach the probe can name the engine.
	cases := []struct {
		name  string
		state State
	}{
		{
			name:  "a stale heartbeat",
			state: State{Running: true, Owner: ownedBy(podRecorded), LastHeartbeat: guardNow.Add(-time.Hour)},
		},
		{
			name:  "a clean stop",
			state: State{Running: false, Owner: ownedBy(podRecorded), LastHeartbeat: guardNow.Add(-time.Hour)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := guardTown(t)
			seedGuardState(t, town, tc.state)
			var took []Owner
			err := guardStartup(town, podSelf, false, podProbe(t, podEngine, unreadJob(t)), guardNow, false,
				func(o Owner) { took = append(took, o) })
			if err == nil {
				t.Fatal("guardStartup = nil, want the refusal: a pod cannot show a container owner stopped")
			}
			for _, want := range []string{`container owner "scratch-town-daemon-1"`, "cannot ask an engine", "GT_DAEMON_TAKEOVER", "cut-over step", "GT_DAEMON_TAKEOVER also passes a failed engine call"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to contain %q", err, want)
				}
			}
			if len(took) != 0 {
				t.Errorf("a refused start reported a takeover of %+v", took)
			}
		})
	}
}

// With GT_DAEMON_TAKEOVER set, the same unreadable container owner lets the start
// go on, and the daemon log names the owner it started over.
func TestStartupOwnerTakeoverProceedsAndLogs(t *testing.T) {
	t.Parallel()
	fresh := State{Running: true, Owner: ownedBy(podRecorded), LastHeartbeat: guardNow.Add(-time.Minute)}

	t.Run("the guard passes and reports the owner", func(t *testing.T) {
		t.Parallel()
		town := guardTown(t)
		seedGuardState(t, town, fresh)
		var took []Owner
		err := guardStartup(town, podSelf, true, podProbe(t, podEngine, unreadJob(t)), guardNow, false,
			func(o Owner) { took = append(took, o) })
		if err != nil {
			t.Fatalf("guardStartup = %v, want the takeover to let the start go on", err)
		}
		if len(took) != 1 || took[0] != podRecorded {
			t.Fatalf("takeover reported %+v, want the recorded owner %+v", took, podRecorded)
		}
	})

	t.Run("a reachable engine whose read fails passes and reports the owner", func(t *testing.T) {
		t.Parallel()
		town := guardTown(t)
		seedGuardState(t, town, fresh)
		var took []Owner
		err := guardStartup(town, podSelf, true, podProbe(t, engineErrors, unreadJob(t)), guardNow, false,
			func(o Owner) { took = append(took, o) })
		if err != nil {
			t.Fatalf("guardStartup = %v, want a failed engine call passed by the takeover", err)
		}
		if len(took) != 1 || took[0] != podRecorded {
			t.Fatalf("takeover reported %+v, want the recorded owner %+v", took, podRecorded)
		}
	})

	t.Run("Run logs the takeover with the owner", func(t *testing.T) {
		t.Parallel()
		town := guardTown(t)
		seedGuardState(t, town, fresh)
		logs, err := runPodStart(t, town, map[string]string{agentconfig.TakeoverEnv: "1"})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want nil or the context-cancel result of the parked start", err)
		}
		if !strings.Contains(logs, `Daemon takeover: container owner "scratch-town-daemon-1" could not be read`) {
			t.Fatalf("daemon log = %q, want the takeover line naming the recorded owner", logs)
		}
	})

	t.Run("Run without the takeover logs no takeover and refuses", func(t *testing.T) {
		t.Parallel()
		town := guardTown(t)
		seedGuardState(t, town, State{Running: true, Owner: ownedBy(podRecorded), LastHeartbeat: guardNow.Add(-time.Hour)})
		logs, err := runPodStart(t, town, nil)
		if err == nil || !strings.Contains(err.Error(), "cannot ask an engine") {
			t.Fatalf("Run = %v, want the pod refusal", err)
		}
		if strings.Contains(logs, "Daemon takeover") {
			t.Errorf("daemon log = %q, want no takeover line for a refused start", logs)
		}
	})
}

// The entry point the CLI start paths call logs the takeover too: GuardStart
// passes an unreadable container owner under the takeover and writes the line
// that names it, so a takeover is never silent on that path.
func TestStartupOwnerGuardStartLogsTheTakeover(t *testing.T) {
	t.Parallel()
	town := guardTown(t)
	seedGuardState(t, town, State{Running: true, Owner: ownedBy(podRecorded), LastHeartbeat: guardNow.Add(-time.Minute)})
	var logs bytes.Buffer
	err := GuardStart(town, podSelf, true, podProbe(t, podEngine, unreadJob(t)), guardNow, log.New(&logs, "", 0))
	if err != nil {
		t.Fatalf("GuardStart = %v, want the takeover to let the start go on", err)
	}
	if want := `Daemon takeover: container owner "scratch-town-daemon-1" could not be read`; !strings.Contains(logs.String(), want) {
		t.Fatalf("GuardStart log = %q, want the takeover line %q", logs.String(), want)
	}
}

// A takeover with nothing to log to refuses rather than pass the recorded
// container owner unlogged, so the takeover is never silent.
func TestStartupOwnerTakeoverWithoutLogRefuses(t *testing.T) {
	t.Parallel()
	town := guardTown(t)
	seedGuardState(t, town, State{Running: true, Owner: ownedBy(podRecorded), LastHeartbeat: guardNow.Add(-time.Minute)})
	err := guardStartup(town, podSelf, true, podProbe(t, podEngine, unreadJob(t)), guardNow, false, nil)
	if err == nil || !strings.Contains(err.Error(), "has no log to name it in") {
		t.Fatalf("guardStartup = %v, want the refusal that the takeover has no log", err)
	}
}

// GuardStart given a nil logger refuses the takeover over the recorded container
// owner rather than panic in the log call: a nil logger is no log to name the
// owner in, so the takeover is refused like any other unlogged takeover.
func TestStartupOwnerGuardStartNilLoggerRefuses(t *testing.T) {
	t.Parallel()
	town := guardTown(t)
	seedGuardState(t, town, State{Running: true, Owner: ownedBy(podRecorded), LastHeartbeat: guardNow.Add(-time.Minute)})
	err := GuardStart(town, podSelf, true, podProbe(t, podEngine, unreadJob(t)), guardNow, nil)
	if err == nil || !strings.Contains(err.Error(), "has no log to name it in") {
		t.Fatalf("GuardStart(nil logger) = %v, want the refusal that the takeover has no log", err)
	}
}

// A launchd, systemd or manual owner the probe cannot read is refused with the
// takeover set, exactly as before: the pod's container pass does not reach them.
func TestStartupOwnerOtherKindsUnchanged(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		owner Owner
	}{
		{name: "launchd", owner: guardLaunchd},
		{name: "systemd", owner: Owner{Kind: agentconfig.SupervisorSystemd, ID: "invocation-1"}},
		{name: "manual", owner: Owner{Kind: OwnerKindManual, ID: "other-host:42"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := guardTown(t)
			seedGuardState(t, town, State{Running: true, Owner: ownedBy(tc.owner), LastHeartbeat: guardNow.Add(-time.Minute)})
			var took []Owner
			err := guardStartup(town, podSelf, true, podProbe(t, podEngine, unreadableJobs), guardNow, false,
				func(o Owner) { took = append(took, o) })
			if err == nil || !strings.Contains(err.Error(), "is unknown") {
				t.Fatalf("guardStartup = %v, want the unknown refusal for a %s owner", err, tc.name)
			}
			if strings.Contains(err.Error(), "cannot ask an engine") {
				t.Errorf("refusal = %q, want no container wording for a %s owner", err, tc.name)
			}
			if len(took) != 0 {
				t.Errorf("takeover passed a %s owner it could not read: %+v", tc.name, took)
			}
		})
	}
}

// A reachable engine that reports the recorded container running refuses the
// start, with or without the takeover: a running owner is never passed.
func TestStartupOwnerRunningContainerStillRefuses(t *testing.T) {
	t.Parallel()
	for _, takeover := range []bool{false, true} {
		t.Run("takeover "+map[bool]string{false: "absent", true: "set"}[takeover], func(t *testing.T) {
			t.Parallel()
			town := guardTown(t)
			seedGuardState(t, town, State{Running: true, Owner: ownedBy(podRecorded), LastHeartbeat: guardNow.Add(-time.Hour)})
			var took []Owner
			err := guardStartup(town, podSelf, takeover, podProbe(t, engineReports("running gastown:latest"), unreadJob(t)), guardNow, false,
				func(o Owner) { took = append(took, o) })
			if err == nil || !strings.Contains(err.Error(), "still running") {
				t.Fatalf("guardStartup = %v, want the refusal that the owner is still running", err)
			}
			if len(took) != 0 {
				t.Errorf("takeover passed a running owner: %+v", took)
			}
		})
	}
}
