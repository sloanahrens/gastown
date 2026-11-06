package daemon

// disk_maintenance_disks is the disk half of the patrol: it reads the disks the
// town writes to, raises the 80/70 usage alerts and the unreadable alert, and
// tightens the registry keep while a disk is nearly full (gt-dl4nv.16). The
// control cluster's disk raises the same alerts but never tightens the keep
// (gt-gg9hv.58).
//
// A disk the patrol cannot measure is not a quiet disk: a failed read or
// output it cannot parse raises diskmaint:<disk>:unreadable and refuses the
// plan, rather than reading as an empty disk that keeps everything. The Mac and
// Docker Desktop disks are the exception in container mode: they come from the
// host agent's probe file, and a missing reading there omits them instead
// (gt-gg9hv.24.4).
//
// The active alert keys live in memory, keyed by alert rather than rebuilt from
// the escalation store at boot: a restart forgets them and re-raises an open
// alert once, which costs a repeat rather than a silence.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

const (
	// diskMaintAlertSource is the source every disk alert carries: the patrol
	// that read the disk.
	diskMaintAlertSource = "disk_maintenance"

	// The disks the patrol reads. The names are the alert keys' middle part,
	// so they are part of the alert identity: rename a disk and its open alert
	// is orphaned under the old name.
	diskMaintDiskMac           = "mac"
	diskMaintDiskColima        = "colima"
	diskMaintDiskDockerDesktop = "docker-desktop"
	// diskMaintDiskControl is the control cluster's node disk. Renaming it
	// orphans an open diskmaint:control alert, like the others.
	diskMaintDiskControl = "control"

	// diskMaintStillUnreadableInterval is how often the patrol logs that an
	// open unreadable alert's disk still cannot be read. The alert itself
	// fires once per outage, so this line is what tells an operator reading the
	// daemon log that the outage has not ended.
	diskMaintStillUnreadableInterval = time.Hour
)

// DFReader runs one disk's read and returns the output its parser reads. A
// failed read is an error, never an empty reading: the patrol must not mistake
// a disk it could not reach for a disk with room.
type DFReader func(ctx context.Context) ([]byte, error)

// diskMaintDisk is one watched disk: the name its alerts are keyed by, how the
// patrol reads it, and the parser its output needs. Exactly one of df and probe
// is set.
type diskMaintDisk struct {
	name string
	// df measures the disk with a command the daemon runs itself.
	df DFReader
	// parse turns the reader's output into the disk's used share; nil is
	// disks.ParseDF. It applies only to a df reading.
	parse func(string) (float64, error)
	// detail names the biggest consumers in the same output; nil names none. A
	// detail that cannot be read makes the reading an error.
	detail func(string) (string, error)
	// alertOnly marks a reading that alerts but neither tightens the keep nor
	// refuses a plan (see disks.Reading.AlertOnly).
	alertOnly bool
	// probe names the reading in the host agent's probe file, for the disks a
	// container daemon cannot reach from inside the container.
	probe func(hostprobe.Probes) hostprobe.Field[hostprobe.Disk]
}

// diskMaintDisks returns the disks the patrol reads: the town's own disks, then
// the control cluster's when a context is configured for it.
func (d *Daemon) diskMaintDisks(kubeconfig string) []diskMaintDisk {
	watched := d.diskMaintTownDisks(kubeconfig)
	if control, ok := d.diskMaintControl(kubeconfig); ok {
		watched = append(watched, control)
	}
	return watched
}

// diskMaintTownDisks returns the town's own disks, in report order: the Mac
// volume, the sandbox cluster's data disk, and the Docker Desktop disk the CI
// runner writes to.
//
// The sandbox disk keeps the name "colima": it is the alert key's middle part,
// and an open alert under a new name would be orphaned.
//
// In container mode the Mac and Docker Desktop readings come from the host
// agent's probe file instead of df: df -P / inside the container names the
// container's own root, and the runner container the patrol used to exec into
// is gone (gt-gg9hv.24). The sandbox disk is read the same way in both modes,
// through the town's kubeconfig (gt-gg9hv.24.3).
func (d *Daemon) diskMaintTownDisks(kubeconfig string) []diskMaintDisk {
	colima := diskMaintDisk{name: diskMaintDiskColima, df: d.diskMaintSandboxDisk(kubeconfig), parse: disks.ParseNodeStats}
	if d.containerMode {
		return []diskMaintDisk{
			{name: diskMaintDiskMac, probe: func(p hostprobe.Probes) hostprobe.Field[hostprobe.Disk] { return p.Disk }},
			colima,
			{name: diskMaintDiskDockerDesktop, probe: func(p hostprobe.Probes) hostprobe.Field[hostprobe.Disk] { return p.DockerDisk }},
		}
	}
	return []diskMaintDisk{
		{name: diskMaintDiskMac, df: d.diskMaintDF("df", "-P", "/")},
		colima,
		// The container is the host agent's: both readings measure one disk, so
		// they must name the same container (hostprobe.DockerDiskContainer).
		{name: diskMaintDiskDockerDesktop, df: d.diskMaintDF("docker", "exec",
			hostprobe.DockerDiskContainer, "df", "-P", "/")},
	}
}

// diskMaintSandboxDisk reads the sandbox cluster's data disk from the kubelet
// node stats, which works from inside the town's container: there is no colima
// binary there, and every node reports its own root filesystem (gt-gg9hv.24.3).
//
// It returns the summary of the node with the highest used share, which the
// disk's parser turns into the reading. A node the cluster lists whose summary
// cannot be read makes the read an error naming every such node, never a lower
// number.
func (d *Daemon) diskMaintSandboxDisk(kubeconfig string) DFReader {
	return d.diskMaintClusterDisk([]string{"--kubeconfig", kubeconfig})
}

// diskMaintClusterDisk reads a cluster's node disk the way the sandbox disk is
// read. kubectl is the flags every call to the cluster starts with, so a caller
// that needs a context names it there.
func (d *Daemon) diskMaintClusterDisk(kubectl []string) DFReader {
	return func(ctx context.Context) ([]byte, error) {
		nodes, err := d.diskMaintClusterNodes(ctx, kubectl)
		if err != nil {
			return nil, err
		}
		if len(nodes) == 0 {
			return nil, errors.New("the cluster lists no nodes")
		}
		var (
			worst    []byte
			worstPct float64
			failures []error
		)
		for _, node := range nodes {
			summary, err := d.diskMaintRunner(ctx, nil, "kubectl",
				diskMaintKubectl(kubectl, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/stats/summary")...)
			if err != nil {
				failures = append(failures, fmt.Errorf("node %s: %w", node, err))
				continue
			}
			pct, err := disks.ParseNodeStats(string(summary))
			if err != nil {
				failures = append(failures, fmt.Errorf("node %s: %w", node, err))
				continue
			}
			if worst == nil || pct > worstPct {
				worst, worstPct = summary, pct
			}
		}
		if len(failures) > 0 {
			return nil, errors.Join(failures...)
		}
		return worst, nil
	}
}

// diskMaintKubectl is the argv of one kubectl call: the cluster's flags, then
// the call's own arguments. It returns a fresh slice, so the flags are never
// shared between calls.
func diskMaintKubectl(kubectl []string, args ...string) []string {
	return append(append(make([]string, 0, len(kubectl)+len(args)), kubectl...), args...)
}

// diskMaintControl returns the control cluster's node disk, read the way the
// sandbox disk is, through the kubeconfig and the context tier_sweep.runner
// names. The town may run without a control cluster: an empty context, or a
// runner on a backend that is not kubernetes, reads nothing and logs once. The
// reading is alert-only: its unreadable or full state neither tightens the keep
// nor refuses a plan.
func (d *Daemon) diskMaintControl(kubeconfig string) (diskMaintDisk, bool) {
	runner := tierSweepRunnerConfig(d.patrolConfig)
	if runner == nil || runner.Backend != agentconfig.TierSweepRunnerKubernetes || runner.Context == "" {
		if d.diskMaintSchedule.firstRefusal("control:unconfigured") {
			d.logger.Printf("disk_maintenance: control disk not read: patrols.tier_sweep.runner names no kubernetes context")
		}
		return diskMaintDisk{}, false
	}
	return diskMaintDisk{
		name:      diskMaintDiskControl,
		df:        d.diskMaintClusterDisk(diskmaint.KubectlArgs(kubeconfig, runner.Context)),
		parse:     disks.ParseNodeStats,
		detail:    disks.TopConsumers,
		alertOnly: true,
	}, true
}

// diskMaintClusterNodes names the nodes a cluster lists, which are the nodes
// that hold its work.
func (d *Daemon) diskMaintClusterNodes(ctx context.Context, kubectl []string) ([]string, error) {
	out, err := d.diskMaintRunner(ctx, nil, "kubectl", diskMaintKubectl(kubectl, "get", "nodes", "-o", "name")...)
	if err != nil {
		return nil, fmt.Errorf("listing the cluster's nodes: %w", err)
	}
	var nodes []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, ok := strings.CutPrefix(line, "node/")
		if !ok {
			return nil, fmt.Errorf("kubectl get nodes named %q, want a node/name", line)
		}
		if !validSandboxNodeName(name) {
			return nil, fmt.Errorf("kubectl get nodes named %q, which is not a node name", name)
		}
		nodes = append(nodes, name)
	}
	return nodes, nil
}

// validSandboxNodeName reports whether name has the DNS subdomain shape a
// Kubernetes node name has. The name becomes part of a request path, so
// anything else is refused before it is fetched.
func validSandboxNodeName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// diskMaintDF runs one disk's df through the daemon's exec seam, so the unit
// tier never launches df or docker.
func (d *Daemon) diskMaintDF(name string, args ...string) DFReader {
	return func(ctx context.Context) ([]byte, error) {
		return d.diskMaintRunner(ctx, nil, name, args...)
	}
}

// diskMaintenanceReadings reads every watched disk once, through the injected
// reader a disk has in tests and its own command otherwise. A probe-backed disk
// is read from the host agent's file. A failed read or output the disk's parser
// refuses becomes that disk's reading error.
func (d *Daemon) diskMaintenanceReadings(ctx context.Context, now time.Time, injected map[string]DFReader, kubeconfig string) []disks.Reading {
	watched := d.diskMaintDisks(kubeconfig)
	host, hostErr := d.diskMaintHostReadings(watched, injected, now)
	if hostErr != nil {
		// The town never depends on the host agent (gt-1pyid): the two readings
		// are omitted and the pass plans without them, rather than raising an
		// unreadable alert for a disk the agent did not report.
		d.logger.Printf("disk_maintenance: host disks unknown: %v", hostErr)
	}
	readings := make([]disks.Reading, 0, len(watched))
	for _, disk := range watched {
		if fn, ok := injected[disk.name]; ok {
			readings = append(readings, readDF(ctx, disk, fn))
			continue
		}
		if disk.probe != nil {
			if r, ok := host[disk.name]; ok {
				readings = append(readings, r)
			}
			continue
		}
		readings = append(readings, readDF(ctx, disk, disk.df))
	}
	return readings
}

// readDF runs one disk's reader and parses its used share and, when the disk
// names them, its biggest consumers from the same output. A failure in either
// is that disk's reading error.
func readDF(ctx context.Context, disk diskMaintDisk, df DFReader) disks.Reading {
	r := disks.Reading{Name: disk.name, AlertOnly: disk.alertOnly}
	parse := disk.parse
	if parse == nil {
		parse = disks.ParseDF
	}
	out, err := df(ctx)
	if err != nil {
		r.Err = err
		return r
	}
	pct, err := parse(string(out))
	if err != nil {
		r.Err = err
		return r
	}
	r.UsedPct = pct
	if disk.detail != nil {
		detail, err := disk.detail(string(out))
		if err != nil {
			r.Err = fmt.Errorf("biggest consumers: %w", err)
			return r
		}
		r.Detail = detail
	}
	return r
}

// diskMaintHostReadings reads the probe-backed disks from the host agent's
// file, at the patrol's clock. Both share one file, so it is read once.
//
// A file that is absent, from another schema or older than fileHostProbeMaxAge,
// and a field the agent recorded as failed, each mean the host agent is not
// reporting: no reading is returned for either disk.
func (d *Daemon) diskMaintHostReadings(watched []diskMaintDisk, injected map[string]DFReader, now time.Time) (map[string]disks.Reading, error) {
	wanted := make([]diskMaintDisk, 0, len(watched))
	for _, disk := range watched {
		if _, ok := injected[disk.name]; ok {
			continue
		}
		if disk.probe != nil {
			wanted = append(wanted, disk)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	p, err := hostprobe.Read(hostprobe.Path(d.config.TownRoot), now, fileHostProbeMaxAge)
	if err != nil {
		return nil, err
	}
	readings := make(map[string]disks.Reading, len(wanted))
	for _, disk := range wanted {
		field := disk.probe(p)
		if field.Err != "" {
			return nil, fmt.Errorf("%s probe: %s", disk.name, field.Err)
		}
		readings[disk.name] = disks.Reading{Name: disk.name, UsedPct: field.Value.UsedPct}
	}
	return readings, nil
}

// diskMaintAlertTurnWait is how long a pass waits for its turn to apply the
// alerts before it reads the disks for itself and goes on: long enough for an
// ordinary application, short of the pass's own bound, so a notifier that does
// not answer cannot hold another pass's targets off for minutes.
const diskMaintAlertTurnWait = 30 * time.Second

// diskMaintAlerts is the patrol's memory of the disk alert keys it has open,
// and of when it last logged that each open unreadable key is still unreadable.
// A pass writes it and the next pass reads it. Whole applications -- the
// reading, the decisions and the sends -- run one after another across passes
// (see readAndApplyDiskAlerts), and the mutex keeps a single accessor safe on
// its own.
type diskMaintAlerts struct {
	// turn is the gate that makes applications run one after another: a buffered
	// channel of one, so a pass waiting for its turn can give up with its
	// context. acquireTurn, when set, replaces it (a test seam, so the ordering
	// test can be shown red without the gate).
	turn        chan struct{}
	turnOnce    sync.Once
	acquireTurn func(ctx context.Context) (release func(), err error)
	mu          sync.Mutex
	active      map[string]bool
	// stillLogged is the pass time of the last "still unreadable" line for a
	// key, or of the pass that first saw its disk unreadable. The clear forgets
	// it, so a new outage starts its own hour.
	stillLogged map[string]time.Time
}

// takeTurn waits for this pass's turn to apply the alerts. A pass whose context
// ends while it waits gives up, and nothing it was to decide is recorded.
func (a *diskMaintAlerts) takeTurn(ctx context.Context) (release func(), err error) {
	if a.acquireTurn != nil {
		return a.acquireTurn(ctx)
	}
	return a.takeGate(ctx)
}

// takeGate is the production wait for a turn.
func (a *diskMaintAlerts) takeGate(ctx context.Context) (release func(), err error) {
	a.turnOnce.Do(func() { a.turn = make(chan struct{}, 1) })
	select {
	case a.turn <- struct{}{}:
		return func() { <-a.turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readAndApplyDiskAlerts reads the disks and applies the alerts they call for,
// as one application that no other pass's overlaps: the later of two passes
// waits for the earlier, then reads and decides from the state the earlier one
// left, so a reading is never older than the state it is applied over and a
// raise and a clear for one key reach the notifier in the order they were
// decided (gt-gg9hv.48). The reading is returned for the combined pass's keep
// decision; false means the pass's context ended before its turn came and
// nothing was read.
func (d *Daemon) readAndApplyDiskAlerts(ctx context.Context, now time.Time) ([]disks.Reading, bool) {
	waitCtx, cancelWait := context.WithTimeout(ctx, diskMaintAlertTurnWait)
	release, err := d.diskMaintAlerts.takeTurn(waitCtx)
	cancelWait()
	if err != nil {
		if ctx.Err() != nil {
			d.logger.Printf("disk_maintenance: the disk alerts were not applied: %v", err)
			return nil, false // the pass itself is over
		}
		// Another pass has held the turn for the whole wait, a notifier that
		// is slow to answer: this pass reads the disks for its own decisions and
		// leaves the alerts to that one, so its targets are not held off.
		d.logger.Printf("disk_maintenance: another pass is applying the disk alerts; this pass reads the disks only: %v", err)
		return d.diskMaintenanceSeams().readDisks(ctx), true
	}
	defer release()
	readings := d.diskMaintenanceSeams().readDisks(ctx)
	d.applyDiskAlerts(ctx, readings, now)
	return readings, true
}

// snapshot returns a copy of the open keys, which is what disks.Evaluate
// compares the readings against.
func (a *diskMaintAlerts) snapshot() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	open := make(map[string]bool, len(a.active))
	for key, active := range a.active {
		open[key] = active
	}
	return open
}

// mark records a key as open, or forgets it once its alert is cleared.
func (a *diskMaintAlerts) mark(key string, open bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !open {
		delete(a.active, key)
		delete(a.stillLogged, key)
		return
	}
	if a.active == nil {
		a.active = map[string]bool{}
	}
	a.active[key] = true
}

// stillUnreadableDue reports whether the patrol may log that key is still
// unreadable at now, recording now when it may. The raise itself is the first
// notice, so an ongoing outage is repeated only after
// diskMaintStillUnreadableInterval.
func (a *diskMaintAlerts) stillUnreadableDue(key string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.stillLogged[key]; ok && now.Sub(last) < diskMaintStillUnreadableInterval {
		return false
	}
	if a.stillLogged == nil {
		a.stillLogged = map[string]time.Time{}
	}
	a.stillLogged[key] = now
	return true
}

// recordUnreadableRaised starts the repeat clock at the pass that tried to
// raise the alert, so the first reminder comes one interval later. It reports
// whether it replaced an earlier clock: a raise the notifier refused is retried
// on the next pass, and that retry is a reset the operator must be told about,
// because it means the hour starts again.
func (a *diskMaintAlerts) recordUnreadableRaised(key string, now time.Time) (again bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stillLogged == nil {
		a.stillLogged = map[string]time.Time{}
	}
	_, again = a.stillLogged[key]
	a.stillLogged[key] = now
	return again
}

// applyDiskAlerts raises and clears the alerts the readings call for, and
// records only the ones that landed. A raise the notifier refused stays
// unrecorded, so the next pass raises it again rather than treating a silence
// as an open alert; a clear that failed leaves its key open for the same
// reason.
//
// An unreadable disk raises its alert once per outage. While that alert stays
// open the pass logs the still-unreadable line at most once an hour, so an
// outage that lasts is visible without a repeat alert. A raise the notifier
// refused is retried on the next pass, and each retry restarts the hour; the
// retry gets its own line so those restarts are not silent. Every send runs
// under the pass's context, so a notifier that never answers is abandoned when
// the pass ends and its alert is decided again by the next one.
func (d *Daemon) applyDiskAlerts(ctx context.Context, readings []disks.Reading, now time.Time) {
	for _, alert := range disks.Evaluate(readings, d.diskMaintAlerts.snapshot()) {
		switch {
		case alert.Raise:
			err := d.escalateAlertCtx(ctx, alert.Key, diskMaintAlertSource, alert.Detail)
			if err != nil {
				d.logger.Printf("disk_maintenance: alert %s was not delivered: %v", alert.Key, err)
			} else {
				d.diskMaintAlerts.mark(alert.Key, true)
			}
			if alert.Unreadable && d.diskMaintAlerts.recordUnreadableRaised(alert.Key, now) {
				d.logger.Printf("disk_maintenance: disk %s still unreadable, alert raised again", alert.Name)
			}
		case alert.Clear:
			if err := d.clearAlertsCtx(ctx, alert.Detail, alert.Key); err != nil {
				d.logger.Printf("disk_maintenance: alert %s was not cleared: %v", alert.Key, err)
				continue // still open, and the next pass clears it again
			}
			d.diskMaintAlerts.mark(alert.Key, false)
		case alert.Still:
			if d.diskMaintAlerts.stillUnreadableDue(alert.Key, now) {
				d.logger.Printf("disk_maintenance: %s", alert.Detail)
			}
		}
	}
}
