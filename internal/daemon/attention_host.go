package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hostprobe"
	"github.com/steveyegge/gastown/internal/rig"
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
	// must fail, or the Forgejo health endpoint must not answer, before the
	// stack or the instance is reported down. One failed tick stays an info
	// line: a probe that times out once is not an outage.
	attentionDockerProbeDownBeats = 3
	// attentionForgejoHealthTimeout bounds the health request. Forgejo answers
	// its health endpoint at once, so five seconds of silence is the instance
	// not answering rather than a slow one.
	attentionForgejoHealthTimeout = 5 * time.Second
)

// The host rules' stable ids. They are the item keys, so an ack and a reader
// find a rule by the same name.
const (
	hostRuleDiskWarn    = attention.RuleHostDiskWarn
	hostRuleDiskCrit    = attention.RuleHostDiskCrit
	hostRuleMemLow      = attention.RuleHostMemLow
	hostRuleDockerDown  = attention.RuleHostDockerDown
	hostRuleForgejoDown = attention.RuleHostForgejoDown
	// hostProbeFailedPrefix keys the item a failed probe raises; the probe's
	// name follows.
	hostProbeFailedPrefix = "host.probe_failed:"
)

// hostStackContainers are the Docker Desktop containers the town cannot run
// without (gt-gg9hv.36): Forgejo's database, and Dolt, which every rig's beads
// live in. Forgejo and its CI runner run in the gt-control cluster now:
// Forgejo is watched by its health endpoint (ForgejoHealthy) and the runner by
// the landing_infra patrol's run-queue probe, so neither is a container name
// here.
var hostStackContainers = []string{
	"forgejo-db-1",
	"gt-dolt",
}

// The history-man database is required only while its rig is live: the operator
// turns hm-postgres off on purpose when the hm rig is parked.
const (
	hostHMRig       = "hm"
	hostHMContainer = "hm-postgres"
)

// requiredStackContainers is the containers in force this tick: hostStackContainers,
// plus hm-postgres unless the hm rig is parked. A park state that cannot
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
	hostProbeDisk    = "disk usage"
	hostProbeMem     = "memory_pressure"
	hostProbeDocker  = "docker ps"
	hostProbeForgejo = "forgejo /api/healthz"
)

// hostDisk is one volume's usage as the rules read it.
type hostDisk struct {
	UsedPct float64
	FreeGB  float64
}

// hostProbes are the host reads the collectors make. attentionHostProbes
// selects the production one: fileHostProbes (attention_host_file.go) in
// container mode, systemHostProbes otherwise. Tests inject fakes.
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
	// ForgejoHealthy asks the town's Forgejo its health endpoint. A nil error
	// is a healthy answer; errForgejoNotConfigured says the town runs no
	// Forgejo to watch, and any other error is the instance not answering.
	ForgejoHealthy(ctx context.Context) error
}

// attentionHostProbes picks the host reads: in container mode the probe file a
// host agent publishes, because the container cannot measure the host; in
// every other mode the machine the daemon runs on. getenv is a seam so a test
// sets the mode without touching the process environment. forgejoBaseURL is
// the town's Forgejo origin (townForgejoBaseURL), which both modes ask over
// the network rather than read from the host.
func attentionHostProbes(townRoot string, getenv func(string) string, forgejoBaseURL func() (string, error)) hostProbes {
	if getenv(config.ContainerModeEnv) == config.SupervisorContainer {
		return newFileHostProbes(townRoot, forgejoBaseURL)
	}
	return systemHostProbes{townRoot: townRoot, forgejoBaseURL: forgejoBaseURL}
}

// systemHostProbes reads the machine the daemon runs on.
type systemHostProbes struct {
	townRoot string
	// forgejoBaseURL is the origin of the town's Forgejo, resolved when the
	// health probe runs so a setting change is read on the next tick. Nil
	// means the probe was built without one, which is a wiring error and not
	// a healthy answer.
	forgejoBaseURL func() (string, error)
	// forgejoHTTP makes the health request. Nil means a client whose timeout
	// is attentionForgejoHealthTimeout; tests inject one with a fake
	// transport.
	forgejoHTTP *http.Client
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

// forgejoHealthPath is the endpoint the town's Forgejo answers when it is up.
const forgejoHealthPath = "/api/healthz"

// errForgejoNotConfigured says the town runs no Forgejo, so there is no health
// to watch. It is not a probe failure: a town whose rigs land through GitHub
// is not an outage, which is how the landing_infra probe reads the same
// absence.
var errForgejoNotConfigured = errors.New("no rig the landing worker serves names a Forgejo remote URL, so the town has no Forgejo to watch")

// ForgejoHealthy asks the town's Forgejo its health endpoint, the one read that
// says the instance every landing gate waits on is up (gt-gg9hv.36). The
// request carries no credential: the health endpoint needs none, and the URL is
// the origin from the landing worker's setting, so no user info can reach it.
func (p systemHostProbes) ForgejoHealthy(ctx context.Context) error {
	if p.forgejoBaseURL == nil {
		return errors.New("the host probes were built with no Forgejo URL source")
	}
	base, err := p.forgejoBaseURL()
	if err != nil {
		return fmt.Errorf("resolving the town's Forgejo URL: %w", err)
	}
	if base == "" {
		return errForgejoNotConfigured
	}
	healthURL := strings.TrimSuffix(base, "/") + forgejoHealthPath
	ctx, cancel := context.WithTimeout(ctx, attentionForgejoHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("forgejo health %s: %w", healthURL, err)
	}
	client := p.forgejoHTTP
	if client == nil {
		client = &http.Client{Timeout: attentionForgejoHealthTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("forgejo health %s: %w", healthURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("forgejo health %s answered %s", healthURL, resp.Status)
	}
	return nil
}

// forgejoOriginFromRemoteURL is the origin of the instance a Forgejo remote
// URL names: scheme and host, with no path, query or user info, so the URL a
// summary carries holds no credential.
func forgejoOriginFromRemoteURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("forgejo remote %q is not a URL with a scheme and host, so the instance it names cannot be asked for its health", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// townForgejoBaseURL is the origin of the town's Forgejo, read from the
// landing worker's own setting: the merge_queue.forgejo.remote_url of the rigs
// the landing worker serves, resolved by the reader its landing path uses
// (rig.ResolveForgejoConfigIn). The first rig in name order that names one
// answers for the town, because a town's rigs land through one instance; the
// order is sorted rather than the worker's, so a town whose rigs disagree does
// not watch a different instance from tick to tick. A town where no such rig
// names a remote URL returns "" and no error, because it has no Forgejo to
// watch. A rig that names a remote URL that is not a URL is an error, never a
// skip to the next rig: an instance the town believes it lands through and
// cannot name is a setting to fix, not an absence.
func (d *Daemon) townForgejoBaseURL() (string, error) {
	// The worker's own list when it has no allowlist is the daemon's cached
	// one, so sort a copy.
	rigs := append([]string(nil), landingWorkerRigs(d.patrolConfig, d.getKnownRigs())...)
	sort.Strings(rigs)
	for _, name := range rigs {
		fj := rig.ResolveForgejoConfigIn(d.repoFileOpener(), d.config.TownRoot, name)
		if fj == nil || strings.TrimSpace(fj.RemoteURL) == "" {
			continue
		}
		return forgejoOriginFromRemoteURL(fj.RemoteURL)
	}
	return "", nil
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

// collectHostForgejo raises host.forgejo_down when the town's Forgejo has not
// answered its health endpoint for attentionDockerProbeDownBeats ticks in a
// row. It reads the health answer the same way collectHostDocker reads docker
// ps: one unanswered tick is an info probe line, because a single timeout is
// not an outage, and a run of them is the instance down, which wakes because
// the info line never does. A healthy answer, or a town that runs no Forgejo,
// returns nothing, so an item either condition cleared is dropped.
func (s *attentionSources) collectHostForgejo(ctx context.Context) ([]attention.Item, error) {
	err := s.host.ForgejoHealthy(ctx)
	if err == nil || errors.Is(err, errForgejoNotConfigured) {
		s.forgejoProbeFails = 0
		return nil, nil
	}
	// A probe that died with the tick's own context (the heartbeat ran out of
	// time) says nothing about the instance, so it is not a beat toward it
	// being down.
	if ctx.Err() == nil {
		s.forgejoProbeFails++
	}
	items := s.hostProbeFailed(hostProbeForgejo, err, attention.KindHostForgejo)
	if s.forgejoProbeFails < attentionDockerProbeDownBeats {
		return items, nil
	}
	// The instance-down item the probe kept from an earlier tick is replaced,
	// not joined: one key, one item.
	kept := items[:0]
	for _, it := range items {
		if it.Key != hostRuleForgejoDown {
			kept = append(kept, it)
		}
	}
	return append(kept, attention.Item{
		Key:      hostRuleForgejoDown,
		Kind:     attention.KindHostForgejo,
		Severity: attention.SeverityHigh,
		Summary: fmt.Sprintf("forgejo health has failed %d heartbeats in a row, so every landing gate is waiting: %s",
			s.forgejoProbeFails, oneLine(err.Error())),
	}), nil
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
