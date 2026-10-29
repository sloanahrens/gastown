package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// containerDaemonTown is a town whose daemon.supervisor is container, with
// state.json and the supervisor's answer as the given check would read them.
// The reading is canned: no container here, and no clock to wait on.
func containerDaemonTown(t *testing.T, state *daemon.State, job templates.SupervisorState) (string, func(string) containerDaemonReading) {
	t.Helper()
	townRoot := t.TempDir()
	cfg := &config.DaemonPatrolConfig{
		Supervisor:            config.SupervisorContainer,
		ContainerNameSetting:  "scratch-daemon-1",
		ComposeProjectSetting: "scratch",
	}
	if err := daemon.SavePatrolConfig(townRoot, cfg); err != nil {
		t.Fatalf("SavePatrolConfig: %v", err)
	}
	// A liveness baseline as a previous run leaves it, with room for the
	// count above to have advanced: without one, that check records a
	// starting point and reports unknown, which hides its healthy branch.
	writeLivenessBaseline(t, townRoot, 5, time.Now().Add(-10*time.Minute))
	read := func(string) containerDaemonReading {
		return containerDaemonReading{state: state, job: job, stale: 6 * time.Minute}
	}
	return townRoot, read
}

// containerOwnedState is state.json as the container's own daemon writes it.
func containerOwnedState(heartbeat time.Time) *daemon.State {
	return &daemon.State{
		Running:        true,
		LastHeartbeat:  heartbeat,
		HeartbeatCount: 12,
		Owner:          &daemon.Owner{Kind: config.SupervisorContainer, ID: "scratch-daemon-1"},
	}
}

// containerRunning is the supervisor's answer for a container that is up.
func containerRunning() templates.SupervisorState {
	return templates.SupervisorState{Kind: config.SupervisorContainer, Loaded: true, LastExit: -1}
}

// containerCheck is one of the four daemon checks and the message it reports
// for a healthy container town.
type containerCheck struct {
	check Check
	ok    string
}

// containerChecks builds the four daemon checks with the container reading
// wired to read. DaemonCheck and DaemonJobCheck also get a running seam that
// fails the test: in a container town neither may read a pid, and the
// container branch of each runs before the seam is reached (gt-qpeei.20).
func containerChecks(t *testing.T, read func(string) containerDaemonReading) []containerCheck {
	t.Helper()
	pidRead := func(string) (bool, int, error) {
		t.Error("read a pid for a container town")
		return false, 0, nil
	}
	const inContainer = "running in its container"
	daemonCheck := NewDaemonCheck()
	daemonCheck.containerReading = read
	daemonCheck.running = pidRead
	jobCheck := NewDaemonJobCheck()
	jobCheck.containerReading = read
	jobCheck.running = pidRead
	liveness := NewDaemonLivenessCheck()
	liveness.containerReading = read
	respawn := NewDaemonRespawnCheck()
	respawn.containerReading = read
	return []containerCheck{
		{daemonCheck, inContainer},
		{jobCheck, inContainer},
		// The liveness check keeps its own heartbeat message; the container
		// verdict only has to let it through.
		{liveness, "heartbeat is fresh"},
		{respawn, inContainer},
	}
}

// Every one of the four daemon checks reports a healthy container town from
// state.json and the supervisor, and never from a pid.
func TestDaemonChecks_ContainerReportsHealthyFromState(t *testing.T) {
	t.Parallel()
	townRoot, read := containerDaemonTown(t, containerOwnedState(time.Now().Add(-time.Minute)), containerRunning())

	for _, c := range containerChecks(t, read) {
		result := c.check.Run(&CheckContext{TownRoot: townRoot})
		if result.Status != StatusOK {
			t.Errorf("%s: Status = %v (%s), want StatusOK", c.check.Name(), result.Status, result.Message)
			continue
		}
		if !strings.Contains(result.Message, c.ok) {
			t.Errorf("%s: Message = %q, want it to say %q", c.check.Name(), result.Message, c.ok)
		}
	}
}

// Each of the four checks reports the failure its reading names — never OK —
// when the container is not running, its heartbeat is stale, the supervisor's
// state cannot be read, or the daemon that wrote state.json is not the
// container's. The status is pinned so a refusal that is removed or quietly
// downgraded turns the row red.
func TestDaemonChecks_ContainerFailsWhenNotHealthy(t *testing.T) {
	t.Parallel()
	fresh := func() *daemon.State { return containerOwnedState(time.Now().Add(-time.Minute)) }
	stopped := func() templates.SupervisorState {
		return templates.SupervisorState{Kind: config.SupervisorContainer, LastExit: 0}
	}
	unreadable := func() templates.SupervisorState {
		return templates.SupervisorState{Kind: config.SupervisorContainer, LastExit: -1, Err: errors.New("docker inspect: no answer")}
	}

	for _, tc := range []struct {
		name  string
		state *daemon.State
		job   templates.SupervisorState
		want  CheckStatus
	}{
		{name: "container not running", state: fresh(), job: stopped(), want: StatusError},
		{name: "heartbeat stale", state: containerOwnedState(time.Now().Add(-30 * time.Minute)), job: containerRunning(), want: StatusError},
		{name: "supervisor state unknown", state: fresh(), job: unreadable(), want: StatusWarning},
		{name: "no owner in state.json", state: &daemon.State{}, job: containerRunning(), want: StatusError},
		{name: "launchd owns the daemon", state: &daemon.State{
			Running:        true,
			LastHeartbeat:  time.Now().Add(-time.Minute),
			HeartbeatCount: 3,
			Owner:          &daemon.Owner{Kind: config.SupervisorLaunchd, ID: templates.LaunchdLabel},
		}, job: containerRunning(), want: StatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot, read := containerDaemonTown(t, tc.state, tc.job)

			for _, c := range containerChecks(t, read) {
				result := c.check.Run(&CheckContext{TownRoot: townRoot})
				if result.Status != tc.want {
					t.Errorf("%s: Status = %v (%s), want %v", c.check.Name(), result.Status, result.Message, tc.want)
				}
			}
		})
	}
}

// An unknown daemon.supervisor value is unknown, not a town the host detects
// for: reading it as a host kind would hand a container town the pid path.
func TestJudgeContainerDaemon_UnknownSupervisorSettingFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := daemon.SavePatrolConfig(townRoot, &config.DaemonPatrolConfig{Supervisor: "podman"}); err != nil {
		t.Fatalf("SavePatrolConfig: %v", err)
	}

	v := judgeContainerDaemon("daemon", townRoot, nil)
	if !v.isContainer || v.result == nil {
		t.Fatalf("judgeContainerDaemon = %+v, want a container verdict with a result", v)
	}
	if v.result.Status == StatusOK {
		t.Errorf("Status = StatusOK, want a failure for an unknown supervisor value")
	}
}

// A town under another supervisor keeps its own path: the verdict is not a
// container town, and the check reads what it always read.
func TestJudgeContainerDaemon_HostKindIsNotAContainerTown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := daemon.SavePatrolConfig(townRoot, &config.DaemonPatrolConfig{Supervisor: config.SupervisorLaunchd}); err != nil {
		t.Fatalf("SavePatrolConfig: %v", err)
	}

	if v := judgeContainerDaemon("daemon", townRoot, nil); v.isContainer {
		t.Errorf("judgeContainerDaemon = %+v, want the host's own path", v)
	}
}

// The container repair goes through the supervisor: a fix never spawns `gt
// daemon run` on this host beside the container's own daemon.
func TestDaemonCheck_Fix_ContainerStartsThroughSupervisor(t *testing.T) {
	t.Parallel()
	townRoot, read := containerDaemonTown(t, containerOwnedState(time.Now().Add(-time.Minute)), containerRunning())

	check := NewDaemonCheck()
	check.containerReading = read
	var verbs []string
	check.gtCommand = func(town string, verb string) error {
		if town != townRoot {
			t.Errorf("ran gt daemon %s in %s, want %s", verb, town, townRoot)
		}
		verbs = append(verbs, verb)
		return nil
	}

	if err := check.Fix(&CheckContext{TownRoot: townRoot}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(verbs) != 1 || verbs[0] != "restart" {
		t.Errorf("Fix ran %v, want [restart] through the container supervisor", verbs)
	}
}

// No seam means the production reading, which reads state.json and reports the
// container supervisor as unread until the docker reader lands: it never
// reports a container town's daemon healthy on a reading nothing made.
func TestDaemonCheck_ContainerWithoutAReaderIsNotHealthy(t *testing.T) {
	t.Parallel()
	townRoot, _ := containerDaemonTown(t, containerOwnedState(time.Now().Add(-time.Minute)), containerRunning())

	result := NewDaemonCheck().Run(&CheckContext{TownRoot: townRoot})
	if result.Status == StatusOK {
		t.Errorf("Status = StatusOK (%s), want a failure while the container's state cannot be read", result.Message)
	}
}
