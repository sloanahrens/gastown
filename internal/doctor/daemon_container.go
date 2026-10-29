package doctor

import (
	"errors"
	"fmt"
	"strconv"
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

// errContainerStateUnread is the container supervisor's state until a reader
// that asks docker exists (templates.ContainerJobState has no production
// caller yet), so a production reading is always unknown here. A check reports
// it as unknown, which it never counts as healthy, and a test supplies the
// reading through containerDaemonReading.
var errContainerStateUnread = errors.New("the container supervisor's state is not read yet")

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
	state, err := daemon.LoadState(townRoot)
	stale := 2 * config.LoadOperationalConfig(townRoot).GetDaemonConfig().RecoveryHeartbeatIntervalD()
	return containerDaemonReading{state: state, stateErr: err, job: containerJobState(), stale: stale}
}

// containerJobState is the container supervisor's live state. Until the docker
// reader lands it is unknown rather than "not running": the container may be
// up, and of the two wrong answers only one reports a failure the operator can
// see.
func containerJobState() templates.SupervisorState {
	return templates.SupervisorState{Kind: config.SupervisorContainer, LastExit: -1, Err: errContainerStateUnread}
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
