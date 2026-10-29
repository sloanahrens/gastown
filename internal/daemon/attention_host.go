package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hostprobe"
	"github.com/steveyegge/gastown/internal/townconfig"
	"github.com/steveyegge/gastown/internal/util"
)

// The host collectors' thresholds (gt-oxr3p), the ones the operator's
// docker-watch.sh and health-watch.sh used before the daemon took the watching.
// Compiled in like the queue's others.
const (
	attentionDiskWarnUsedPct = 80.0
	attentionDiskWarnFreeGB  = 12.0
	attentionDiskCritUsedPct = 90.0
	attentionDiskCritFreeGB  = 5.0
	// attentionMemLowFreePct is the free-memory percentage below which the host
	// is short of memory.
	attentionMemLowFreePct = 15.0
	// attentionHostProbeTimeout bounds one external host probe, so a wedged
	// docker daemon costs one tick's probe and not the whole tick.
	attentionHostProbeTimeout = 20 * time.Second
	// attentionDockerProbeDownBeats is how many consecutive ticks docker ps
	// must fail before the stack is reported down. One failed tick stays an
	// info line: a probe that times out once is not an outage.
	attentionDockerProbeDownBeats = 3
)

// The host rules' stable ids. They are the item keys, so an ack and a reader
// find a rule by the same name.
const (
	hostRuleDiskWarn   = attention.RuleHostDiskWarn
	hostRuleDiskCrit   = attention.RuleHostDiskCrit
	hostRuleMemLow     = attention.RuleHostMemLow
	hostRuleDockerDown = attention.RuleHostDockerDown
	// hostProbeFailedPrefix keys the item a failed probe raises; the probe's
	// name follows.
	hostProbeFailedPrefix = "host.probe_failed:"
)

// hostStackContainers are the containers that must always be running: the
// Forgejo stack the landing pipeline depends on.
var hostStackContainers = []string{
	"forgejo-forgejo-1",
	"forgejo-db-1",
	"forgejo-runner-daemon-1",
	"forgejo-docker-in-docker-1",
}

// The history-man database is required only while its rig is live: the operator
// turns hm-postgres off on purpose when the hm rig is parked.
const (
	hostHMRig       = "hm"
	hostHMContainer = "hm-postgres"
)

// requiredStackContainers is the containers in force this tick: the Forgejo
// four, plus hm-postgres unless the hm rig is parked. A park state that cannot
// be read counts as parked, because a false alarm for a container whose rig may
// be parked costs more than a missed one.
func (s *attentionSources) requiredStackContainers() []string {
	req := append([]string(nil), hostStackContainers...)
	if parked, err := s.host.RigParked(hostHMRig); err == nil && !parked {
		req = append(req, hostHMContainer)
	}
	return req
}

// The names a failed probe is reported under, as the operator would type the
// command.
const (
	hostProbeDisk   = "disk usage"
	hostProbeMem    = "memory_pressure"
	hostProbeDocker = "docker ps"
)

// hostDisk is one volume's usage as the rules read it.
type hostDisk struct {
	UsedPct float64
	FreeGB  float64
}

// hostProbes are the host reads the collectors make. Production picks between
// the two below in attentionHostProbes; tests inject fakes.
type hostProbes interface {
	// Disk is the usage of the volume holding the town root.
	Disk() (hostDisk, error)
	// MemFreePct is the host's free memory as a percentage of total.
	MemFreePct(ctx context.Context) (float64, error)
	// Containers are the names of the running Docker containers.
	Containers(ctx context.Context) ([]string, error)
	// RigParked reports whether the rig is parked. An error means the state
	// could not be read.
	RigParked(rig string) (bool, error)
}

// attentionHostProbes picks the host reads: in container mode the probe file a
// host agent publishes, because the container cannot measure the host; in
// every other mode the machine the daemon runs on. getenv is a seam so a test
// sets the mode without touching the process environment.
func attentionHostProbes(townRoot string, getenv func(string) string) hostProbes {
	if getenv(config.ContainerModeEnv) == config.SupervisorContainer {
		return newFileHostProbes(townRoot)
	}
	return systemHostProbes{townRoot: townRoot}
}

// systemHostProbes reads the machine the daemon runs on.
type systemHostProbes struct {
	townRoot string
}

func (p systemHostProbes) Disk() (hostDisk, error) {
	info, err := util.GetDiskSpace(p.townRoot)
	if err != nil {
		return hostDisk{}, err
	}
	return hostDisk{UsedPct: info.UsedPercent, FreeGB: info.AvailableGB()}, nil
}

func (p systemHostProbes) RigParked(rig string) (bool, error) {
	rec, err := townconfig.ParkState(p.townRoot, rig)
	return rec != nil, err
}

func (systemHostProbes) MemFreePct(ctx context.Context) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, attentionHostProbeTimeout)
	defer cancel()
	return memFreePctFrom(exec.CommandContext(ctx, "memory_pressure").Output())
}

// memFreePctFrom reads the free-memory figure from memory_pressure's result. A
// failed run is an error, not a reading.
func memFreePctFrom(out []byte, err error) (float64, error) {
	if err != nil {
		return 0, err
	}
	return hostprobe.ParseMemoryPressure(string(out))
}

func (systemHostProbes) Containers(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, attentionHostProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", err, ee.Stderr)
		}
		return nil, err
	}
	return parseContainerNames(string(out)), nil
}

// parseContainerNames splits docker ps --format {{.Names}} output into names.
func parseContainerNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// collectHostDisk raises host.disk_crit or host.disk_warn, whichever the town
// root's volume is at (never both): >=90% used or <5 GB free is critical,
// >=80% or <12 GB is a warning. The directory sizes that say what filled the
// disk are not computed here, because walking the town is too slow for a
// heartbeat; the summary points at the runbook for that.
func (s *attentionSources) collectHostDisk(ctx context.Context) ([]attention.Item, error) {
	d, err := s.host.Disk()
	if err != nil {
		return s.hostProbeFailed(hostProbeDisk, err, attention.KindHostDisk), nil
	}
	rule, sev := "", attention.SeverityLow
	switch {
	case d.UsedPct >= attentionDiskCritUsedPct || d.FreeGB < attentionDiskCritFreeGB:
		rule, sev = hostRuleDiskCrit, attention.SeverityHigh
	case d.UsedPct >= attentionDiskWarnUsedPct || d.FreeGB < attentionDiskWarnFreeGB:
		rule = hostRuleDiskWarn
	default:
		return nil, nil
	}
	return []attention.Item{{
		Key:      rule,
		Kind:     attention.KindHostDisk,
		Severity: sev,
		Summary:  fmt.Sprintf("disk %.0f%% used, %.1f GB free; see docker-watch runbook", d.UsedPct, d.FreeGB),
	}}, nil
}

// collectHostMem raises host.mem_low when less than 15% of the host's memory
// is free.
func (s *attentionSources) collectHostMem(ctx context.Context) ([]attention.Item, error) {
	free, err := s.host.MemFreePct(ctx)
	if err != nil {
		return s.hostProbeFailed(hostProbeMem, err, attention.KindHostMem), nil
	}
	if free >= attentionMemLowFreePct {
		return nil, nil
	}
	return []attention.Item{{
		Key:      hostRuleMemLow,
		Kind:     attention.KindHostMem,
		Severity: attention.SeverityLow,
		Summary:  fmt.Sprintf("memory %.0f%% free", free),
	}}, nil
}

// collectHostDocker raises host.docker_stack_down when any required container
// (requiredStackContainers) is not among the running ones, naming the missing.
//
// A docker ps that fails cannot name the missing containers, and a failure that
// held for attentionDockerProbeDownBeats ticks in a row is the stack down in
// all but name: Docker Desktop has quit or the daemon is wedged. Such a failure
// raises host.docker_stack_down itself, because the info-only probe item never
// wakes anyone. A failure that comes and goes stays a probe item.
func (s *attentionSources) collectHostDocker(ctx context.Context) ([]attention.Item, error) {
	running, err := s.host.Containers(ctx)
	if err != nil {
		// A docker ps that died with the tick's own context (the heartbeat ran
		// out of time) says nothing about docker, so it is a probe failure but
		// not a beat toward the stack being down.
		if ctx.Err() == nil {
			s.dockerProbeFails++
		}
		items := s.hostProbeFailed(hostProbeDocker, err, attention.KindHostDocker)
		if s.dockerProbeFails < attentionDockerProbeDownBeats {
			return items, nil
		}
		// The stack-down item the probe kept from an earlier tick is replaced,
		// not joined: one key, one item.
		kept := items[:0]
		for _, it := range items {
			if it.Key != hostRuleDockerDown {
				kept = append(kept, it)
			}
		}
		return append(kept, attention.Item{
			Key:      hostRuleDockerDown,
			Kind:     attention.KindHostDocker,
			Severity: attention.SeverityHigh,
			Summary: fmt.Sprintf("docker ps has failed for %d heartbeats, so the stack is unreachable: %s",
				s.dockerProbeFails, oneLine(err.Error())),
		}), nil
	}
	s.dockerProbeFails = 0
	up := make(map[string]bool, len(running))
	for _, n := range running {
		up[n] = true
	}
	required := s.requiredStackContainers()
	var missing []string
	for _, n := range required {
		if !up[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	return []attention.Item{{
		Key:      hostRuleDockerDown,
		Kind:     attention.KindHostDocker,
		Severity: attention.SeverityHigh,
		Summary:  fmt.Sprintf("%d of %d stack containers not running: %s", len(missing), len(required), strings.Join(missing, ", ")),
	}}, nil
}

// hostProbeFailed is what a collector returns when its probe failed: one
// info-level item naming the probe, and the previous tick's items of the
// collector's own kind. The probe's rule could not be judged, so it keeps
// whatever it last held rather than clearing a real alarm (townhealth's
// UNKNOWN rule). The other probes are other collectors and are unaffected.
func (s *attentionSources) hostProbeFailed(probe string, err error, kind attention.Kind) []attention.Item {
	out := prevOfKind(s.prev, kind)
	return append(out, attention.Item{
		Key:      hostProbeFailedPrefix + probe,
		Kind:     attention.KindHostProbe,
		Severity: attention.SeverityLow,
		Summary:  fmt.Sprintf("host probe %s failed: %s", probe, oneLine(err.Error())),
	})
}

// oneLine collapses s to its first line, capped, for a queue row.
func oneLine(s string) string {
	const max = 160
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
