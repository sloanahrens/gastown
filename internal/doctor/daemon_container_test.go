package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// writeContainerComposeFile gives the town the compose file only a container
// town has, which is what keeps an unreadable setting on the container path.
func writeContainerComposeFile(t *testing.T, townRoot string) {
	t.Helper()
	path := config.ContainerComposeFile(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An unknown daemon.supervisor value is unknown, not a town the host detects
// for: reading it as a host kind would hand a container town the pid path. The
// town carries the compose file a container town has.
func TestJudgeContainerDaemon_UnknownSupervisorSettingFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeContainerComposeFile(t, townRoot)
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

// The container repair starts nothing on this host: the daemon begins only
// through its container supervisor, and the fix names that command instead of
// spawning `gt daemon run` beside the container's own daemon.
func TestDaemonCheck_Fix_ContainerStartsNothingHere(t *testing.T) {
	t.Parallel()
	townRoot, read := containerDaemonTown(t, containerOwnedState(time.Now().Add(-time.Minute)), containerRunning())

	check := NewDaemonCheck()
	check.containerReading = read

	err := check.Fix(&CheckContext{TownRoot: townRoot})
	if err == nil {
		t.Fatal("Fix = nil, want a refusal naming the supervisor's command")
	}
	if !strings.Contains(err.Error(), "gt daemon restart") {
		t.Errorf("Fix = %v, want it to name 'gt daemon restart'", err)
	}
}

// A settings file that will not read, in a town that carries a container
// compose file, is reported as unknown, not as a town under the host's
// supervisor: an unreadable setting must not choose the pid path for a town
// that may be a container town.
func TestJudgeContainerDaemon_UnreadableSettingsIsUnknown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeContainerComposeFile(t, townRoot)
	path := config.TownSettingsPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"daemon": {"type": "daemon-patrol-config", "version": 99}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	v := judgeContainerDaemon("daemon", townRoot, nil)
	if !v.isContainer || v.result == nil {
		t.Fatalf("judgeContainerDaemon = %+v, want a container verdict with a result", v)
	}
	if v.result.Status == StatusOK || !strings.Contains(v.result.Message, "unknown") {
		t.Errorf("result = (%v, %q), want an unknown failure", v.result.Status, v.result.Message)
	}
}

// A settings file that will not read, in a town with no container compose
// file, is a host town with a damaged file: its checks keep the pid-reading
// path they always had, and the verdict does not change for it.
func TestJudgeContainerDaemon_UnreadableSettingsWithoutComposeIsAHostTown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := config.TownSettingsPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"daemon": {"type": "daemon-patrol-config", "version": 99}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	v := judgeContainerDaemon("daemon", townRoot, nil)
	if v.isContainer || v.result != nil {
		t.Fatalf("judgeContainerDaemon = %+v, want the zero verdict a host town gets", v)
	}
}

// containerTownOnDisk is a container town whose daemon/state.json holds state,
// so a reading that loads the file — not a canned value — sees it.
func containerTownOnDisk(t *testing.T, state *daemon.State) string {
	t.Helper()
	townRoot, _ := containerDaemonTown(t, state, containerRunning())
	if err := daemon.SaveState(townRoot, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	return townRoot
}

// fakeDockerInspect is a docker CLI that never runs: it answers with the given
// stdout or error and records the argv each call carried.
type fakeDockerInspect struct {
	stdout string
	err    error
	argv   [][]string
}

func (f *fakeDockerInspect) run(argv []string) ([]byte, error) {
	f.argv = append(f.argv, argv)
	return []byte(f.stdout), f.err
}

// read is the production container reading with only the docker subprocess
// faked: readContainerDaemonWith, the function readContainerDaemon itself calls.
func (f *fakeDockerInspect) read(townRoot string) containerDaemonReading {
	return readContainerDaemonWith(f.run, townRoot)
}

// Each of the four daemon checks reports a healthy container town from the
// production reading: state.json as the container's daemon wrote it, and one
// docker inspect of the container the town's settings name answering that it
// runs. Nothing but the docker CLI is faked.
func TestDaemonChecks_ContainerReadsHealthyFromDockerInspect(t *testing.T) {
	t.Parallel()
	townRoot := containerTownOnDisk(t, containerOwnedState(time.Now().Add(-time.Minute)))
	docker := &fakeDockerInspect{stdout: "running sha256:abc123\n"}

	for _, c := range containerChecks(t, docker.read) {
		result := c.check.Run(&CheckContext{TownRoot: townRoot})
		if result.Status != StatusOK {
			t.Errorf("%s: Status = %v (%s), want StatusOK", c.check.Name(), result.Status, result.Message)
			continue
		}
		if !strings.Contains(result.Message, c.ok) {
			t.Errorf("%s: Message = %q, want it to say %q", c.check.Name(), result.Message, c.ok)
		}
	}

	want := []string{"docker", "--context", "desktop-linux", "inspect", "-f", "{{.State.Status}} {{.Config.Image}}", "scratch-daemon-1"}
	if len(docker.argv) == 0 {
		t.Fatal("no docker inspect ran; a healthy verdict must come from reading the container")
	}
	for _, argv := range docker.argv {
		if !slices.Equal(argv, want) {
			t.Errorf("docker argv = %q, want %q", argv, want)
		}
	}
}

// The container reading is docker's answer, field by field: a container docker
// reports is loaded with its status and image; the container docker says it
// does not have is absent, not unreadable; and every other failure — an
// unreachable daemon, output this reader cannot interpret — is unknown, never
// "not running".
func TestContainerJobState_DockerInspectAnswers(t *testing.T) {
	t.Parallel()
	absent := &dockerInspectError{
		stderr: "Error response from daemon: No such object: scratch-daemon-1",
		err:    errors.New("exit status 1"),
	}
	unreachable := &dockerInspectError{
		stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock",
		err:    errors.New("exit status 1"),
	}

	for _, tc := range []struct {
		name    string
		docker  fakeDockerInspect
		loaded  bool
		status  string
		image   string
		wantErr bool
	}{
		{name: "running", docker: fakeDockerInspect{stdout: "running sha256:abc123\n"},
			loaded: true, status: "running", image: "sha256:abc123"},
		{name: "exited", docker: fakeDockerInspect{stdout: "exited sha256:abc123\n"},
			loaded: true, status: "exited", image: "sha256:abc123"},
		{name: "container absent", docker: fakeDockerInspect{err: absent}},
		{name: "docker unreachable", docker: fakeDockerInspect{err: unreachable}, wantErr: true},
		{name: "no status printed", docker: fakeDockerInspect{stdout: "\n"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot, _ := containerDaemonTown(t, containerOwnedState(time.Now().Add(-time.Minute)), containerRunning())
			docker := tc.docker

			st := containerJobState(townRoot, docker.run)

			if tc.wantErr != (st.Err != nil) {
				t.Fatalf("Err = %v, wantErr %v", st.Err, tc.wantErr)
			}
			if st.Loaded != tc.loaded {
				t.Errorf("Loaded = %v, want %v", st.Loaded, tc.loaded)
			}
			if st.Status != tc.status || st.Image != tc.image {
				t.Errorf("Status, Image = %q, %q, want %q, %q", st.Status, st.Image, tc.status, tc.image)
			}
			if st.Kind != config.SupervisorContainer {
				t.Errorf("Kind = %q, want %q", st.Kind, config.SupervisorContainer)
			}
			if len(docker.argv) != 1 {
				t.Fatalf("docker ran %d times, want 1", len(docker.argv))
			}
		})
	}
}

// A container town that names no container of its own, or names one outside
// the shape docker accepts, is unknown and runs no docker command: the
// container name an unset town inherits is the live town's, and reporting
// another town's container healthy would be an answer made up.
func TestContainerJobState_TownWithoutItsOwnContainerIsUnknown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  *config.DaemonPatrolConfig
	}{
		{name: "no container named", cfg: &config.DaemonPatrolConfig{
			Supervisor: config.SupervisorContainer,
		}},
		{name: "name outside the docker shape", cfg: &config.DaemonPatrolConfig{
			Supervisor:            config.SupervisorContainer,
			ContainerNameSetting:  "-rf",
			ComposeProjectSetting: "scratch",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if err := daemon.SavePatrolConfig(townRoot, tc.cfg); err != nil {
				t.Fatalf("SavePatrolConfig: %v", err)
			}
			docker := &fakeDockerInspect{}

			st := containerJobState(townRoot, docker.run)

			if st.Err == nil {
				t.Fatalf("Err = nil, want an unknown reading")
			}
			if !strings.Contains(st.Err.Error(), "container_name") {
				t.Errorf("Err = %q, want it to name daemon.container_name", st.Err)
			}
			if len(docker.argv) != 0 {
				t.Errorf("docker ran %d times, want none: %q", len(docker.argv), docker.argv)
			}
		})
	}
}

// A town whose daemon settings will not read names no container to inspect:
// the reading is unknown, naming the file, and no docker command runs.
func TestContainerJobState_UnreadableSettingsIsUnknown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := config.TownSettingsPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"daemon": {"type": "daemon-patrol-config", "version": 99}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	docker := &fakeDockerInspect{}

	st := containerJobState(townRoot, docker.run)

	if st.Err == nil || !strings.Contains(st.Err.Error(), "reading") {
		t.Fatalf("Err = %v, want a read failure naming the settings file", st.Err)
	}
	if len(docker.argv) != 0 {
		t.Errorf("docker ran %d times, want none: %q", len(docker.argv), docker.argv)
	}
}
