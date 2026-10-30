package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/procid"
	"github.com/steveyegge/gastown/internal/templates"
)

// guardStartup decides whether this daemon may start on townRoot. self is the
// owner DetectSelf named for this process, takeover is the GT_DAEMON_TAKEOVER
// flag, probe reads whether a recorded owner is running and now is the
// caller's clock. logPreexisted is whether daemon.log existed before this
// process wrote to it (see Daemon.logPreexisted). A nil error lets the start go
// on; any other error is the refusal, and the caller writes nothing.
//
// The guard only reads: state.json, the daemon's lock, and the town's daemon
// settings for the container name. A state.json that cannot be read or does
// not parse refuses here, without CheckTakeover, because a file that does not
// parse names no owner to judge.
func guardStartup(townRoot string, self Owner, takeover bool, probe OwnerProbe, now time.Time, logPreexisted bool) error {
	state, err := readStartupState(townRoot)
	if err != nil {
		return err
	}
	candidates, err := startupCandidates(townRoot)
	if err != nil {
		return err
	}
	return CheckTakeover(TakeoverInput{
		State:      state,
		PriorRun:   priorRunEvidence(townRoot, logPreexisted),
		Candidates: candidates,
		Now:        now,
		Self:       self,
		Takeover:   takeover,
		Probe:      probe,
	})
}

// GuardStart is the guard the CLI start paths call before they launch a daemon
// (gt-qpeei.28): the same decision Run makes in the process that is about to
// become the daemon, asked from the command that is about to start it. self is
// the owner the started daemon will name (the CLI is not it); takeover is the
// operator's --takeover; probe nil reads docker, launchd and systemd for real.
// daemon.log existing is the prior-run evidence Run takes from New, since the
// CLI has written nothing to it.
func GuardStart(townRoot string, self Owner, takeover bool, probe OwnerProbe, now time.Time) error {
	if probe == nil {
		probe = startupOwnerProbe(templates.RunDockerInspect, os.Hostname, nil)
	}
	_, err := os.Stat(DefaultConfig(townRoot).LogFile)
	return guardStartup(townRoot, self, takeover, probe, now, !errors.Is(err, fs.ErrNotExist))
}

// readStartupState reads state.json for the guard: nil when the file is
// missing, and an error when it cannot be read or does not parse.
func readStartupState(townRoot string) (*State, error) {
	path := StateFile(townRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("refusing to start: reading %s: %w", path, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("refusing to start: %s does not parse (%v), so it names no owner and another daemon may still hold the town", path, err)
	}
	return &state, nil
}

// priorRunEvidence reports whether a prior daemon left a sign on the town root
// that outlives a deleted state file: daemon.lock, or a daemon.log that existed
// before this process wrote to it. A stat error other than absence counts as
// evidence: it is not proof that the file is gone.
func priorRunEvidence(townRoot string, logPreexisted bool) bool {
	if logPreexisted {
		return true
	}
	_, err := os.Stat(filepath.Join(townRoot, "daemon", "daemon.lock"))
	return !errors.Is(err, fs.ErrNotExist)
}

// startupCandidates names the owners this town could have, which a legacy
// takeover must show stopped: the launchd job and, when the town's settings
// name one, its container. The container is never named from a default, since
// an inherited name could be another town's container.
func startupCandidates(townRoot string) ([]Owner, error) {
	cfg, err := ReadPatrolConfig(townRoot)
	if err != nil {
		return nil, fmt.Errorf("refusing to start: reading %s: %w", PatrolConfigSource(townRoot), err)
	}
	candidates := []Owner{{Kind: agentconfig.SupervisorLaunchd, ID: templates.LaunchdLabel}}
	if cfg != nil && cfg.ContainerNameSetting != "" {
		candidates = append(candidates, Owner{Kind: agentconfig.SupervisorContainer, ID: cfg.ContainerName()})
	}
	return candidates, nil
}

// startupOwnerProbe builds the probe the guard asks about a recorded owner from
// the readers the supervisor status already uses: docker inspect for a
// container (run, templates.RunDockerInspect in production), the service
// manager's job for launchd and systemd, and a same-host process check for a
// manual owner (hostname names this host). Whatever it cannot read is unknown,
// and unknown refuses. read reads a supervisor job; nil reads the service
// manager for real.
func startupOwnerProbe(run templates.ContainerDocker, hostname func() (string, error), read templates.SupervisorReader) OwnerProbe {
	if read == nil {
		read = templates.SupervisorJobState
	}
	return func(owner Owner) (running, known bool) {
		switch owner.Kind {
		case agentconfig.SupervisorContainer:
			return containerOwnerRunning(templates.ContainerJobState(run, templates.ContainerDockerContext, owner.ID)(owner.Kind))
		case agentconfig.SupervisorLaunchd:
			if owner.ID != templates.LaunchdLabel {
				return false, false
			}
			return jobOwnerRunning(read(owner.Kind))
		case agentconfig.SupervisorSystemd:
			return systemdOwnerRunning(owner.ID, read(owner.Kind))
		case OwnerKindManual:
			return manualOwnerRunning(owner.ID, hostname)
		}
		return false, false
	}
}

// containerOwnerRunning reads a container owner's docker state. A container
// docker does not know is stopped; a status outside the known set is unknown.
func containerOwnerRunning(st templates.SupervisorState) (running, known bool) {
	switch {
	case st.Err != nil:
		return false, false
	case !st.Loaded:
		return false, true
	}
	switch st.Status {
	case "running", "restarting", "paused":
		return true, true
	case "created", "exited", "dead":
		return false, true
	}
	return false, false
}

// jobOwnerRunning reads a service manager job's state: a job with a pid is
// running, a loaded job without one is stopped.
func jobOwnerRunning(st templates.SupervisorState) (running, known bool) {
	switch {
	case st.Err != nil:
		return false, false
	case !st.Loaded:
		return false, true
	}
	return st.PID != 0, true
}

// systemdOwnerRunning judges a systemd owner by the unit's current invocation
// id: a recorded id that is not the one the unit runs now belongs to an earlier
// or another run, and is not judged by the unit's state. An id that cannot be
// read matches nothing, so an empty unit id never matches an empty owner id
// (gt-iiqlb, gt-nirga).
func systemdOwnerRunning(id string, st templates.SupervisorState) (running, known bool) {
	if st.Err != nil || st.InvocationID == "" || st.InvocationID != id {
		return false, false
	}
	return jobOwnerRunning(st)
}

// manualOwnerRunning checks a hand-run owner, whose id is "host:pid". Only a
// process on this host can be seen, so another host's owner is unknown.
func manualOwnerRunning(id string, hostname func() (string, error)) (running, known bool) {
	i := strings.LastIndex(id, ":")
	if i < 0 {
		return false, false
	}
	pid, err := strconv.Atoi(id[i+1:])
	if err != nil || pid <= 0 {
		return false, false
	}
	here, err := hostname()
	if err != nil || here != id[:i] {
		return false, false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, false
	}
	return procid.ProbeAlive(process.Signal(syscall.Signal(0))), true
}
