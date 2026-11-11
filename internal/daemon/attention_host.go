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
	// attentionForgejoProbeDownBeats is how many consecutive ticks the Forgejo
	// health endpoint must not answer before the instance is reported down. One
	// failed tick stays an info line: a probe that times out once is not an
	// outage.
	attentionForgejoProbeDownBeats = 3
	// attentionForgejoHealthTimeout bounds the health request. Forgejo answers
	// its health endpoint at once, so five seconds of silence is the instance
	// not answering rather than a slow one.
	attentionForgejoHealthTimeout = 5 * time.Second
)

// The host rules' stable ids. They are the item keys, so an ack and a reader
// find a rule by the same name.
const (
	hostRuleDiskWarn     = attention.RuleHostDiskWarn
	hostRuleDiskCrit     = attention.RuleHostDiskCrit
	hostRuleNodeDiskWarn = attention.RuleHostNodeDiskWarn
	hostRuleNodeDiskCrit = attention.RuleHostNodeDiskCrit
	hostRuleMemLow       = attention.RuleHostMemLow
	hostRuleForgejoDown  = attention.RuleHostForgejoDown
	// hostProbeFailedPrefix keys the item a failed probe raises; the probe's
	// name follows.
	hostProbeFailedPrefix = "host.probe_failed:"
)

// The names a failed probe is reported under, as the operator would type the
// command.
const (
	hostProbeDisk     = "disk usage"
	hostProbeNodeDisk = "control node disk"
	hostProbeMem      = "memory_pressure"
	hostProbeForgejo  = "forgejo /api/healthz"
)

// hostDisk is one volume's usage as the rules read it.
type hostDisk struct {
	UsedPct float64
	FreeGB  float64
}

// errHostNodeNotConfigured says the town runs no control cluster whose node
// disk could be read, so there is nothing to watch. It is not a probe failure:
// a host with no control cluster is not an outage, and the rules raise nothing.
// Container mode runs in the control cluster, so there the same answer is a
// failed read (fileHostProbes.NodeDisks).
var errHostNodeNotConfigured = errors.New("the town runs no control cluster, so there is no control node disk to read")

// hostProbes are the host reads the collectors make. attentionHostProbes
// selects the production one: fileHostProbes (attention_host_file.go) in
// container mode, systemHostProbes otherwise. Tests inject fakes.
type hostProbes interface {
	// Disk is the usage of the volume holding the town root.
	Disk() (hostDisk, error)
	// NodeDisks are the control nodes' data disks, one read per node
	// (controlNodeDisk). A nil error is a set of reads: each is a reading, or
	// its own failure in hostNodeRead.Err, and each node is ruled on alone.
	// errHostNodeNotConfigured says the town runs no control cluster to read,
	// and is only returned outside container mode; any other error is a read
	// that failed as a whole.
	NodeDisks(ctx context.Context) ([]hostNodeRead, error)
	// MemFreePct is the host's free memory as a percentage of total.
	MemFreePct(ctx context.Context) (float64, error)
	// ForgejoHealthy asks the town's Forgejo its health endpoint. A nil error
	// is a healthy answer; errForgejoNotConfigured says the town runs no
	// Forgejo to watch; errForgejoSettingUnreadable says the origin could not
	// be resolved, so the instance is unknown; any other error is the instance
	// not answering.
	ForgejoHealthy(ctx context.Context) error
}

// attentionHostProbes picks the host reads: in container mode the probe file a
// host agent publishes, because the container cannot measure the host; in
// every other mode the machine the daemon runs on. getenv is a seam so a test
// sets the mode without touching the process environment. forgejoBaseURL is
// the town's Forgejo origin (townForgejoBaseURL), which both modes ask over
// the network rather than read from the host. nodeDisks reads the control
// nodes' disks; only container mode uses it, and every other mode reports the
// node disk not configured.
func attentionHostProbes(townRoot string, getenv func(string) string, forgejoBaseURL func() (string, error), nodeDisks func(context.Context) ([]hostNodeRead, error)) hostProbes {
	if getenv(config.ContainerModeEnv) == config.SupervisorContainer {
		return newFileHostProbes(townRoot, forgejoBaseURL, nodeDisks)
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

// NodeDisks reports the control nodes' disks as not configured: the nodes are
// read only in container mode, through the control cluster
// (fileHostProbes.NodeDisks).
func (systemHostProbes) NodeDisks(context.Context) ([]hostNodeRead, error) {
	return nil, errHostNodeNotConfigured
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

// Containers are the names of the running Docker containers. Only the boot
// gate's docker probe reads them now (newBootProbes), outside container mode;
// the attention collectors do not.
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

// errForgejoSettingUnreadable says the town's Forgejo origin could not be
// resolved: a settings tier that exists cannot be read or parsed, or names a
// remote URL that is not a URL. It is neither an outage nor an absence: the
// watch knows neither where Forgejo is nor that there is none, so the collector
// keeps whatever it last held rather than raising or clearing (gt-gg9hv.36).
var errForgejoSettingUnreadable = errors.New("the Forgejo setting could not be read")

// ForgejoHealthy asks the town's Forgejo its health endpoint, the one read that
// says the instance every landing gate waits on is up (gt-gg9hv.36). The
// request carries no credential: the health endpoint needs none, and the URL is
// the origin from the landing worker's setting, so no user info can reach it.
// A URL source that fails is errForgejoSettingUnreadable: the origin is
// unknown, which the collector keeps the last answer on rather than reading as
// an absence.
func (p systemHostProbes) ForgejoHealthy(ctx context.Context) error {
	if p.forgejoBaseURL == nil {
		return errors.New("the host probes were built with no Forgejo URL source")
	}
	base, err := p.forgejoBaseURL()
	if err != nil {
		return fmt.Errorf("%w: %w", errForgejoSettingUnreadable, err)
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

// errForgejoRigListUnknown says the landing worker's rig list was empty or
// could not be read, so whether the town runs a Forgejo is unknown rather than
// absent. A town whose rigs are not listed is not a town without Forgejo: the
// reading failed, so the collector keeps its last answer (gt-gg9hv.36).
var errForgejoRigListUnknown = errors.New("the landing worker's rig list is unreadable or empty, so the town's Forgejo origin is unknown")

// forgejoOriginFromRemoteURL is the origin of the instance a Forgejo remote
// URL names: scheme and host, with no path, query or user info, so the URL a
// summary carries holds no credential.
//
// The error names the rig and the field, and the host only when it can be
// recovered after the user info is stripped. A remote URL can carry a
// credential in its user info, so no other part of it appears in the error or
// in anything built from it (gt-gg9hv.36).
func forgejoOriginFromRemoteURL(rigName, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host, nil
	}
	if host := forgejoHostFromUnparseableRemote(raw); host != "" {
		return "", fmt.Errorf("remote origin for rig %s is unparseable (host %s)", rigName, host)
	}
	return "", fmt.Errorf("remote origin for rig %s is unparseable", rigName)
}

// forgejoHostFromUnparseableRemote is the host a remote URL names when the URL
// itself does not parse, and "" when none can be recovered. It cuts the user
// info first, at the last '@' of the authority, because that is where a
// credential sits; then it keeps the result only if it is shaped like a host
// with an optional numeric port: a dotted name or a bracketed literal, every
// character one such a host can hold. Anything else is dropped rather than
// guessed at, so a malformed URL cannot put a token into the message.
func forgejoHostFromUnparseableRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == ':' || r == '[' || r == ']':
		default:
			return ""
		}
	}
	host, port := s, ""
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return ""
		}
		host = s[:end+1]
		if rest := s[end+1:]; rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return ""
			}
			port = rest[1:]
		}
	} else if i := strings.LastIndexByte(s, ':'); i >= 0 {
		host, port = s[:i], s[i+1:]
		if host == "" {
			return ""
		}
	}
	if port != "" && !allDigits(port) {
		return ""
	}
	if !strings.HasPrefix(host, "[") && !strings.Contains(host, ".") {
		return ""
	}
	return s
}

// allDigits says s is a non-empty run of ASCII digits.
func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// townForgejoBaseURL is the origin of the town's Forgejo, read from the
// landing worker's own setting: the merge_queue.forgejo.remote_url of the rigs
// the landing worker serves, resolved by the reader its landing path uses
// (rig.ResolveForgejoConfigWithError). The first rig in name order that names
// one answers for the town, because a town's rigs land through one instance;
// the order is sorted rather than the worker's, so a town whose rigs disagree
// does not watch a different instance from tick to tick. A rig list read
// cleanly where no rig names a remote URL returns "" and no error, because the
// town has no Forgejo to watch. An empty or unreadable rig list is an error,
// never that absence: the list itself failed, so the town's instance is
// unknown. A setting the run cannot read, or a remote URL that is not a URL, is
// an error too, never a skip to the next rig: the instance the town believes it
// lands through is unknown until the setting is fixed, and the collector keeps
// its last answer rather than clearing an alarm.
func (d *Daemon) townForgejoBaseURL() (string, error) {
	// The worker's own list when it has no allowlist is the daemon's cached
	// one, so sort a copy.
	rigs := append([]string(nil), landingWorkerRigs(d.patrolConfig, d.getKnownRigs())...)
	sort.Strings(rigs)
	if len(rigs) == 0 {
		return "", errForgejoRigListUnknown
	}
	var base string
	for _, name := range rigs {
		fj, err := rig.ResolveForgejoConfigWithError(d.repoFileOpener(), d.config.TownRoot, name)
		if err != nil {
			return "", err
		}
		if base != "" || fj == nil || strings.TrimSpace(fj.RemoteURL) == "" {
			continue
		}
		origin, err := forgejoOriginFromRemoteURL(name, fj.RemoteURL)
		if err != nil {
			return "", err
		}
		base = origin
	}
	return base, nil
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

// collectHostNodeDisk raises host.node_disk_crit or host.node_disk_warn, whichever
// the control nodes' data disks are at (never both for one item), with the
// thresholds of the town root's disk rule. The node's image filesystem holds the
// container runtime's store and the local-path claims, so a full one gets the
// node's pods evicted (gt-x1po96).
//
// Each node is ruled on its own figures. The one rule item names the node at
// the highest level, the fullest first among equals, since the rule's key is
// one item. A node that could not be read is its own info item, keyed by the
// node, and the other nodes are still ruled. A town with no control cluster
// raises nothing; a read that failed as a whole, including a container-mode town
// with no kubernetes runner, is an info item.
func (s *attentionSources) collectHostNodeDisk(ctx context.Context) ([]attention.Item, error) {
	reads, err := s.host.NodeDisks(ctx)
	if errors.Is(err, errHostNodeNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return s.hostProbeFailed(hostProbeNodeDisk, err, attention.KindHostNodeDisk), nil
	}
	var (
		worst  *hostNodeDisk
		probes []attention.Item
	)
	for i := range reads {
		r := &reads[i]
		if r.Err != nil {
			probes = append(probes, hostProbeItem(hostProbeNodeDisk+" "+r.Node, r.Err))
			continue
		}
		if worst == nil || nodeDiskWorse(r.Disk, worst.Disk) {
			worst = &r.hostNodeDisk
		}
	}
	var rated []attention.Item
	if worst != nil {
		if it, ok := nodeDiskItem(*worst); ok {
			rated = append(rated, it)
		}
	}
	// A node that could not be read may hold a rule it raised on an earlier
	// tick, and its last reading is not a clear. One rule stands at a time: of
	// the fresh rule and the held ones, the worst level is kept, so a held
	// warning never sits beside a fresh critical, nor a fresh warning beside a
	// held critical (gt-x1po96, review round 3).
	if len(probes) > 0 {
		for _, held := range prevOfKind(s.prev, attention.KindHostNodeDisk) {
			rated = worseNodeDiskRule(rated, held)
		}
	}
	return append(rated, probes...), nil
}

// worseNodeDiskRule returns rated with candidate folded in: the single rule item
// at the worse level of the two (critical over warning), the existing one on a
// tie, so the result never holds two node-disk rules.
func worseNodeDiskRule(rated []attention.Item, candidate attention.Item) []attention.Item {
	if len(rated) == 0 {
		return []attention.Item{candidate}
	}
	if nodeDiskRuleLevel(candidate.Key) > nodeDiskRuleLevel(rated[0].Key) {
		return []attention.Item{candidate}
	}
	return rated
}

// nodeDiskRuleLevel is the level a node-disk rule key stands for.
func nodeDiskRuleLevel(key string) int {
	switch key {
	case hostRuleNodeDiskCrit:
		return nodeDiskLevelCrit
	case hostRuleNodeDiskWarn:
		return nodeDiskLevelWarn
	}
	return nodeDiskLevelQuiet
}

// nodeDiskItem is the rule item a node's disk raises, if its level is over a
// threshold: host.node_disk_crit at the critical level, host.node_disk_warn at
// the warning level, and none for a quiet disk.
func nodeDiskItem(n hostNodeDisk) (attention.Item, bool) {
	switch nodeDiskLevel(n.Disk) {
	case nodeDiskLevelCrit:
		return nodeDiskRuleItem(hostRuleNodeDiskCrit, attention.SeverityHigh, n), true
	case nodeDiskLevelWarn:
		return nodeDiskRuleItem(hostRuleNodeDiskWarn, attention.SeverityLow, n), true
	}
	return attention.Item{}, false
}

// nodeDiskRuleItem is the rule item for node n under key rule.
func nodeDiskRuleItem(rule string, sev attention.Severity, n hostNodeDisk) attention.Item {
	return attention.Item{
		Key:      rule,
		Kind:     attention.KindHostNodeDisk,
		Severity: sev,
		Summary: fmt.Sprintf("control node %s data disk %.0f%% used, %.1f GB free; see docs/guides/control-cluster-disk.md",
			n.Node, n.Disk.UsedPct, n.Disk.FreeGB),
	}
}

// The levels a control node's disk is at, by the town root's thresholds. The
// rule item is the level's rule.
const (
	nodeDiskLevelQuiet = iota
	nodeDiskLevelWarn
	nodeDiskLevelCrit
)

// nodeDiskLevel is how far a disk is past the thresholds: critical at 90% used
// or under 5 GB free, a warning at 80% used or under 12 GB free.
func nodeDiskLevel(d hostDisk) int {
	switch {
	case d.UsedPct >= attentionDiskCritUsedPct || d.FreeGB < attentionDiskCritFreeGB:
		return nodeDiskLevelCrit
	case d.UsedPct >= attentionDiskWarnUsedPct || d.FreeGB < attentionDiskWarnFreeGB:
		return nodeDiskLevelWarn
	}
	return nodeDiskLevelQuiet
}

// nodeDiskWorse reports whether disk a is at a higher level than b, or at the
// same level and fuller.
func nodeDiskWorse(a, b hostDisk) bool {
	la, lb := nodeDiskLevel(a), nodeDiskLevel(b)
	return la > lb || (la == lb && a.UsedPct > b.UsedPct)
}

// hasItemKey reports whether items holds an item with key.
func hasItemKey(items []attention.Item, key string) bool {
	for _, it := range items {
		if it.Key == key {
			return true
		}
	}
	return false
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

// collectHostForgejo raises host.forgejo_down when the town's Forgejo has not
// answered its health endpoint for attentionForgejoProbeDownBeats ticks in a
// row. One unanswered tick is an info probe line, because a single timeout is
// not an outage, and a run of them is the instance down, which wakes because
// the info line never does. A healthy answer, or a town that runs no Forgejo,
// returns nothing, so an item either condition cleared is dropped.
//
// A setting the run could not read is neither: it is not a failure beat, so
// the run of unread beats restarts and the setting never raises, and an item an
// earlier tick raised stays open with the reason in its summary (gt-gg9hv.36).
func (s *attentionSources) collectHostForgejo(ctx context.Context) ([]attention.Item, error) {
	err := s.host.ForgejoHealthy(ctx)
	if err == nil || errors.Is(err, errForgejoNotConfigured) {
		s.forgejoProbeFails = 0
		return nil, nil
	}
	if errors.Is(err, errForgejoSettingUnreadable) {
		// The run of failures is broken by a tick that could not read the
		// instance, so it restarts when readings resume.
		s.forgejoProbeFails = 0
		return s.forgejoSettingUnreadable(err), nil
	}
	// A probe that died with the tick's own context (the heartbeat ran out of
	// time) says nothing about the instance, so it is not a beat toward it
	// being down.
	if ctx.Err() == nil {
		s.forgejoProbeFails++
	}
	items := s.hostProbeFailed(hostProbeForgejo, err, attention.KindHostForgejo)
	if s.forgejoProbeFails < attentionForgejoProbeDownBeats {
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

// forgejoUnreadableReason marks where a kept item's summary says why its rule
// could not be judged, so a tick that keeps it again replaces the reason
// instead of stacking one on each heartbeat.
const forgejoUnreadableReason = "; the Forgejo setting could not be read: "

// forgejoSettingUnreadable is what the collector returns when the origin could
// not be read: the info probe line, and the instance-down item an earlier tick
// raised kept open with this tick's reason in its summary. It never raises,
// because a setting the run could not read says nothing about the instance.
func (s *attentionSources) forgejoSettingUnreadable(err error) []attention.Item {
	items := s.hostProbeFailed(hostProbeForgejo, err, attention.KindHostForgejo)
	out := items[:0]
	for _, it := range items {
		if it.Key == hostRuleForgejoDown {
			it.Summary = withForgejoUnreadableReason(it.Summary, oneLine(err.Error()))
		}
		out = append(out, it)
	}
	return out
}

// withForgejoUnreadableReason appends reason to a kept alarm's summary, after
// dropping the reason a previous unreadable tick appended.
func withForgejoUnreadableReason(summary, reason string) string {
	if i := strings.Index(summary, forgejoUnreadableReason); i >= 0 {
		summary = summary[:i]
	}
	return summary + forgejoUnreadableReason + reason
}

// hostProbeFailed is what a collector returns when its probe failed: one
// info-level item naming the probe, and the previous tick's items of the
// collector's own kind. The probe's rule could not be judged, so it keeps
// whatever it last held rather than clearing a real alarm (townhealth's
// UNKNOWN rule). The other probes are other collectors and are unaffected.
func (s *attentionSources) hostProbeFailed(probe string, err error, kind attention.Kind) []attention.Item {
	return append(prevOfKind(s.prev, kind), hostProbeItem(probe, err))
}

// hostProbeItem is the info item a failed probe raises, keyed by the probe's
// name. A probe that fails on several nodes names each node in its own probe
// name, so each failure is its own item.
func hostProbeItem(probe string, err error) attention.Item {
	return attention.Item{
		Key:      hostProbeFailedPrefix + probe,
		Kind:     attention.KindHostProbe,
		Severity: attention.SeverityLow,
		Summary:  fmt.Sprintf("host probe %s failed: %s", probe, oneLine(err.Error())),
	}
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
