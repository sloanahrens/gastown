package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// A container town's daemon is identified by its container, not by a pid: the
// number in daemon.pid and in daemon.lock comes from the container's
// namespace and names nothing, or an unrelated process, on this host. The
// doctor's daemon checks therefore read the town's daemon settings, the
// daemon's own daemon/state.json and the container supervisor's state, and
// read no pid (gt-qpeei.20).

// containerDaemonReading is a container town's two daemon reports: state.json,
// the daemon's own, and what the container supervisor says about the container.
type containerDaemonReading struct {
	state    *daemon.State
	stateErr error
	job      templates.SupervisorState
	// stale is the heartbeat age DaemonLivenessCheck calls stuck; zero is
	// staleAfter's default.
	stale time.Duration
}

// readContainerDaemon is the production reading; a check's seam replaces it
// with canned values in tests.
func readContainerDaemon(townRoot string) containerDaemonReading {
	return readContainerDaemonWith(runDockerInspect, townRoot)
}

// readContainerDaemonWith is readContainerDaemon with the docker command
// injected, so a test reads through the same code with a fake docker and no
// container.
func readContainerDaemonWith(run func(argv []string) ([]byte, error), townRoot string) containerDaemonReading {
	state, err := daemon.LoadState(townRoot)
	stale := 2 * config.LoadOperationalConfig(townRoot).GetDaemonConfig().RecoveryHeartbeatIntervalD()
	return containerDaemonReading{state: state, stateErr: err, job: containerJobState(townRoot, run), stale: stale}
}

// defaultDockerContext is the --context every container argv carries: the
// Docker Desktop context a container town's daemon runs in. Never the global
// context, which nothing here may depend on.
const defaultDockerContext = "desktop-linux"

// containerInspectFormat asks docker for the two fields the supervisor's
// state is made of: the state it reports for the container and the image it
// runs.
const containerInspectFormat = "{{.State.Status}} {{.Config.Image}}"

// containerAbsentMarker is what docker writes to stderr for a container it
// knows nothing about. That container is absent, which is docker's answer and
// not a read that failed.
const containerAbsentMarker = "No such object"

// dockerInspectError is a failed docker command's stderr, kept apart from the
// stdout the reading parses so a diagnostic is never read as state. Error()
// carries the exit alone, because captured output can hold a secret.
type dockerInspectError struct {
	stderr string
	err    error
}

func (e *dockerInspectError) Error() string { return e.err.Error() }

func (e *dockerInspectError) Unwrap() error { return e.err }

// runDockerInspect runs one docker command for the container reading.
func runDockerInspect(argv []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // fixed argv from inspectContainerJobState
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &dockerInspectError{stderr: stderr.String(), err: err}
	}
	return stdout.Bytes(), nil
}

// inspectContainerJobState is the container supervisor's state as one docker
// inspect of the container named containerName reports it. Only docker's own
// "No such object" makes an absent container: every other failure is unknown,
// never "not running", because the daemon may well be up and of the two wrong
// answers only one reports a failure the operator can see. This is the read
// internal/cmd.ContainerJobState makes for gt daemon and gt down, repeated here
// because doctor is a package internal/cmd imports, not the other way round.
func inspectContainerJobState(run func(argv []string) ([]byte, error), containerName string) templates.SupervisorState {
	argv := []string{"docker", "--context", defaultDockerContext, "inspect", "-f", containerInspectFormat, containerName}
	st := templates.SupervisorState{Kind: config.SupervisorContainer, LastExit: -1, Container: containerName}
	out, err := run(argv)
	if err != nil {
		var runErr *dockerInspectError
		if errors.As(err, &runErr) && strings.Contains(runErr.stderr, containerAbsentMarker) {
			return st
		}
		// The stderr is not quoted: captured output can hold a secret.
		st.Err = fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		return st
	}
	// A successful run that printed no status and image is a read this reader
	// cannot interpret, not a container that is absent.
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		st.Err = fmt.Errorf("%s: docker printed no status and image for %s", strings.Join(argv, " "), containerName)
		return st
	}
	st.Loaded = true
	st.Status = fields[0]
	st.Image = fields[1]
	return st
}

// containerJobState is the container supervisor's live state in the town at
// townRoot. The town's own settings are the only source of the container's
// name, so a scratch town inspects its scratch container and never the live
// one; a container town that names no container of its own is unknown, since
// an inherited default would report the live daemon's container as this town's.
// run may be nil (runDockerInspect).
func containerJobState(townRoot string, run func(argv []string) ([]byte, error)) templates.SupervisorState {
	if run == nil {
		run = runDockerInspect
	}
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return unreadContainerState(fmt.Errorf("reading %s: %w", daemon.PatrolConfigSource(townRoot), err))
	}
	if cfg == nil || cfg.ContainerNameSetting == "" {
		return unreadContainerState(errors.New("daemon.supervisor is container, but the town sets no daemon.container_name, so no container of this town's is named"))
	}
	if err := cfg.ValidateContainerNames(); err != nil {
		return unreadContainerState(err)
	}
	return inspectContainerJobState(run, cfg.ContainerName())
}

// unreadContainerState is the unknown reading of a container town whose
// settings do not name a container this binary may inspect; no docker command
// runs for it.
func unreadContainerState(err error) templates.SupervisorState {
	return templates.SupervisorState{Kind: config.SupervisorContainer, LastExit: -1, Err: err}
}

// daemonSupervisorKind is the town's configured daemon.supervisor kind, "" when
// it sets none and the host's own detection answers. A settings file that will
// not read is an error, never a town without a setting.
func daemonSupervisorKind(townRoot string) (string, error) {
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", daemon.PatrolConfigSource(townRoot), err)
	}
	return cfg.SupervisorKind()
}

// containerDaemonVerdict is the container-mode branch of a doctor daemon check.
// isContainer is false for a town under any other supervisor, whose check runs
// its own logic. For a container town result is the answer the check reports,
// or nil when the town's daemon and container are both up and the check's own
// subject decides the rest.
type containerDaemonVerdict struct {
	isContainer bool
	reading     containerDaemonReading
	result      *CheckResult
}

// judgeContainerDaemon reads the town's supervisor kind and, for a container
// town, its reading. read may be nil (readContainerDaemon).
func judgeContainerDaemon(name, townRoot string, read func(townRoot string) containerDaemonReading) containerDaemonVerdict {
	kind, err := daemonSupervisorKind(townRoot)
	if err != nil {
		// The setting is what puts a town on this path, but the compose file a
		// container town carries does not depend on it. A town that may be a
		// container town is judged here and is never handed the pid-reading
		// path; one with no compose file is a host town and keeps its own check.
		if !config.MayBeContainerTown(townRoot) {
			return containerDaemonVerdict{}
		}
		return containerDaemonVerdict{isContainer: true, result: &CheckResult{
			Name:    name,
			Status:  StatusWarning,
			Message: "unknown: could not read the town's daemon settings",
			Details: []string{err.Error()},
		}}
	}
	if kind != config.SupervisorContainer {
		return containerDaemonVerdict{}
	}
	if read == nil {
		read = readContainerDaemon
	}
	r := read(townRoot)
	return containerDaemonVerdict{isContainer: true, reading: r, result: r.refusal(name)}
}

// refusal is the failure the reading dictates, or nil when the container runs,
// state.json says the daemon runs under it, and its heartbeat is fresh. An
// unknown reading refuses too: a check that called it healthy would be
// inventing the answer the operator runs it for.
func (r containerDaemonReading) refusal(name string) *CheckResult {
	state := r.state
	if state == nil {
		state = &daemon.State{}
	}
	supervised := "Supervised: " + r.job.StatusLine(0)

	switch {
	case r.job.Err != nil:
		return &CheckResult{Name: name, Status: StatusWarning,
			Message: "unknown: could not read the container supervisor's state",
			Details: []string{r.job.Err.Error(), supervised}}
	case r.stateErr != nil:
		return &CheckResult{Name: name, Status: StatusWarning,
			Message: "unknown: could not read daemon/state.json",
			Details: []string{r.stateErr.Error()}}
	case !r.job.Loaded:
		if state.Running {
			return &CheckResult{Name: name, Status: StatusError,
				Message: "the container is not running, but daemon/state.json says the daemon is",
				Details: []string{supervised}}
		}
		return &CheckResult{Name: name, Status: StatusWarning,
			Message: "the daemon is not running: its container is stopped",
			Details: []string{supervised}}
	case state.Owner == nil || state.Owner.Kind != config.SupervisorContainer:
		owner := "no owner"
		if state.Owner != nil {
			owner = state.Owner.Kind + " " + state.Owner.ID
		}
		return &CheckResult{Name: name, Status: StatusError,
			Message: "daemon/state.json names " + owner + " as the daemon's owner, not this container",
			Details: []string{supervised}}
	case !state.Running:
		return &CheckResult{Name: name, Status: StatusWarning,
			Message: "the daemon is not running",
			Details: []string{supervised}}
	case state.LastHeartbeat.IsZero():
		return &CheckResult{Name: name, Status: StatusWarning,
			Message: "unknown: daemon/state.json has no heartbeat yet",
			Details: []string{supervised}}
	}

	if age := time.Since(state.LastHeartbeat); age > r.staleAfter() {
		return &CheckResult{Name: name, Status: StatusError,
			Message: fmt.Sprintf("daemon heartbeat is stale: last heartbeat %s ago (threshold %s)",
				age.Round(time.Second), r.staleAfter())}
	}
	return nil
}

// staleAfter is the heartbeat age this reading calls stale, the threshold
// DaemonLivenessCheck applies.
func (r containerDaemonReading) staleAfter() time.Duration {
	if r.stale > 0 {
		return r.stale
	}
	return 2 * config.DefaultRecoveryHeartbeatInterval
}

// okResult is the healthy result of a container town's check: the supervisor
// runs the container and the daemon's own state agrees, so a refusal above
// found nothing.
func (r containerDaemonReading) okResult(name string) *CheckResult {
	details := []string{"Supervised: " + r.job.StatusLine(0)}
	// A reading with no daemon state still reports the supervisor's view.
	if r.state != nil && !r.state.StartedAt.IsZero() {
		details = append(details, "Uptime: "+time.Since(r.state.StartedAt).Round(time.Second).String())
	}
	if r.state != nil && r.state.HeartbeatCount > 0 {
		details = append(details, "Heartbeats: "+strconv.FormatInt(r.state.HeartbeatCount, 10))
	}
	return &CheckResult{
		Name:    name,
		Status:  StatusOK,
		Message: "the daemon is running in its container",
		Details: details,
	}
}
