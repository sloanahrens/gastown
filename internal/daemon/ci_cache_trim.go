package daemon

// The CI runner's cache trim (gt-gg9hv.57). The forgejo runner runs the CI gate
// jobs inside its dind container, and their caches are named volumes in that
// container's Docker data root. Nothing trimmed those volumes, and a full
// control-VM data disk got the runner evicted with the rest of the node's pods.
//
// The disk_maintenance patrol runs this trim on its own interval
// (ci_cache_trim_interval), in any window and whether or not the town is busy,
// as the shared cache trim does (gt-gg9hv.48). Each run lists the runner's pod
// once, then execs once per volume in ciCacheVolumes, in table order, into the
// pod's dind container. A volume with no cap (ci-gocache) gets the age rule
// alone: a constant script removes the volume's entries that have gone unwritten
// for ci_cache_max_age. A capped volume (ci-npm) gets the same removal only while
// it is over its cap; at or under the cap the script removes nothing. Each exec
// reports the volume's size before and after. ci-gomod is not in the table: its
// trim is a sibling's. Nothing outside a table root is named by an exec.
//
// Dry run is the patrol's default, as for every maintenance target: the exec runs
// a script that lists what it would remove and removes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

const (
	// ciCacheNamespace, ciCacheDeployment, ciCacheContainer and ciCachePodLabel name
	// the runner whose dind container holds the cache.
	// TestCICacheTrimConstantsMatchTheRunnerManifest holds them equal to
	// deploy/k8s/forgejo-runner/testdata/deployment.yaml.
	ciCacheNamespace  = "forgejo"
	ciCacheDeployment = "forgejo-runner"
	ciCacheContainer  = "dind"
	ciCachePodLabel   = "app.kubernetes.io/name=" + ciCacheDeployment

	// ciCacheDindMount is where the dind container mounts its data claim: the
	// Docker data root the named volumes live under.
	ciCacheDindMount = "/var/lib/docker"

	// The named volumes the trim runs over, as the runner names them
	// (gocacheVolume and npmCacheVolume in deploy/k8s/forgejo-runner/runner.go).
	// TestCICacheTrimConstantsMatchTheRunnerManifest holds them equal to the
	// runner's rendered config.
	ciCacheGocacheVolume = "ci-gocache"
	ciCacheNpmVolume     = "ci-npm"

	// ciCacheNpmCapKiB is the size above which the npm cache is trimmed: 1 GiB. It
	// is a constant: no setting names it.
	ciCacheNpmCapKiB = 1 << 20

	// ciCacheMissingPathExit is the status the script exits with when the volume
	// path is not in the container. kubectl exec passes a remote command's status
	// through, so this is told apart from a failure of kubectl itself.
	ciCacheMissingPathExit = 3

	// ciCacheTrimName is the schedule key the CI trim's last run is recorded under.
	// It is not a target name, so it cannot collide with one.
	ciCacheTrimName = "ci_cache_trim"

	// ciCacheNoOpKey is the attention item key of the uncapped volume's trim that
	// removed no file while the volume stayed over ci_cache_warn_bytes. A capped
	// volume's key is this one with its name after it (ciCacheNoOpKeyOf).
	ciCacheNoOpKey = "ci-cache-trim-noop"
)

// ciCacheVolume is one named volume the trim runs over. root is the one path the
// exec reads and removes under: a constant, with no path built from a setting or
// from a reply. capKiB is the size above which the volume is trimmed; 0 means no
// cap, so the age rule alone applies.
type ciCacheVolume struct {
	name   string
	root   string
	capKiB int64
}

// ciCacheVolumes is the table a pass runs, in order.
var ciCacheVolumes = []ciCacheVolume{
	{name: ciCacheGocacheVolume, root: ciCacheVolumeRoot(ciCacheGocacheVolume)},
	{name: ciCacheNpmVolume, root: ciCacheVolumeRoot(ciCacheNpmVolume), capKiB: ciCacheNpmCapKiB},
}

// ciCacheVolumeRoot is the data path of the named volume in the dind container's
// Docker data root.
func ciCacheVolumeRoot(name string) string {
	return ciCacheDindMount + "/volumes/" + name + "/_data"
}

// ciCacheNoOpKeyOf is the attention item key of a volume's no-op reading. The
// uncapped volume keeps the key it has always had; a capped volume names itself.
func ciCacheNoOpKeyOf(v ciCacheVolume) string {
	if v.capKiB == 0 {
		return ciCacheNoOpKey
	}
	return ciCacheNoOpKey + "-" + v.name
}

// ciCacheExecTarget is the cluster and the runner pod a pass's execs go to.
type ciCacheExecTarget struct {
	kubeconfig  string
	kubeContext string
	pod         string
}

// ciCacheListScript is the dry run's program: it lists the files the apply would
// remove and removes nothing. Its arguments are the volume root and the age in
// minutes, passed as argv data. Only files inside a subdirectory of the root are
// candidates, so the root's own files (Go's trim.txt bookkeeping) are left to Go.
const ciCacheListScript = `set -eu
root=$1; age=$2
[ -d "$root" ] || { echo "volume path not found" >&2; exit 3; }
list=$(mktemp)
trap 'rm -f "$list"' EXIT
before=$(du -sk "$root" | cut -f1)
find "$root" -mindepth 2 -type f -mmin +"$age" -print > "$list"
removed=$(wc -l < "$list" | tr -d ' ')
echo "before_kib=$before"
echo "removed=$removed"
echo "after_kib=$before"
`

// ciCacheTrimScript is the apply's program: the same listing as the dry run's,
// with the removal, then the empty directories the removal left below the top
// level, and the size after. Its arguments are the volume root and the age in
// minutes. The top level's own directories are the cache's 256 two-hex-digit
// shards, which Go creates once and does not recreate: removing an emptied one
// loses every write that hashes into it (gt-qaqvf4).
const ciCacheTrimScript = `set -eu
root=$1; age=$2
[ -d "$root" ] || { echo "volume path not found" >&2; exit 3; }
list=$(mktemp)
trap 'rm -f "$list"' EXIT
before=$(du -sk "$root" | cut -f1)
find "$root" -mindepth 2 -type f -mmin +"$age" -print -delete > "$list"
find "$root" -mindepth 2 -type d -empty -delete
removed=$(wc -l < "$list" | tr -d ' ')
after=$(du -sk "$root" | cut -f1)
echo "before_kib=$before"
echo "removed=$removed"
echo "after_kib=$after"
`

// ciCacheCappedTrimScript is the capped volume's apply program. Its arguments are
// the volume root, the age in minutes and the cap in KiB. At or under the cap it
// removes nothing and reports the size as before and after; over the cap it does
// what ciCacheTrimScript does, and reports the same three lines.
const ciCacheCappedTrimScript = `set -eu
root=$1; age=$2; cap=$3
[ -d "$root" ] || { echo "volume path not found" >&2; exit 3; }
before=$(du -sk "$root" | cut -f1)
if [ "$before" -le "$cap" ]; then
echo "before_kib=$before"
echo "removed=0"
echo "after_kib=$before"
exit 0
fi
list=$(mktemp)
trap 'rm -f "$list"' EXIT
find "$root" -mindepth 2 -type f -mmin +"$age" -print -delete > "$list"
find "$root" -mindepth 2 -type d -empty -delete
removed=$(wc -l < "$list" | tr -d ' ')
after=$(du -sk "$root" | cut -f1)
echo "before_kib=$before"
echo "removed=$removed"
echo "after_kib=$after"
`

// errCICacheTrimNoContext is the refusal for a runner that names no control
// cluster context: the trim cannot say which cluster to exec into.
var errCICacheTrimNoContext = errors.New("patrols.tier_sweep.runner.context names no control cluster: the runner must be the kubernetes backend with a context")

// errCICacheTrimNotRunning is the failed run for a runner with no pod whose dind
// container is running: there is no cache to trim in a container that is not up.
var errCICacheTrimNotRunning = fmt.Errorf("the %s container of %s is not running in %s", ciCacheContainer, ciCacheDeployment, ciCacheNamespace)

// ciCacheTrimReply is the three figures a run's script reports: the volume's size
// before and after in KiB, and the files it removed or, in a dry run, would
// remove.
type ciCacheTrimReply struct {
	beforeKiB int64
	removed   int64
	afterKiB  int64
}

// parseCICacheTrimReply reads the script's reply, which must be exactly the
// three key=value lines in order. Any other output is an error: a reply read as
// zero figures would read as a trim that removed nothing from an empty volume.
func parseCICacheTrimReply(out []byte) (ciCacheTrimReply, error) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 3 {
		return ciCacheTrimReply{}, fmt.Errorf("the reply has %d line(s), want three", len(lines))
	}
	var values [3]int64
	for i, key := range []string{"before_kib", "removed", "after_kib"} {
		k, v, ok := strings.Cut(lines[i], "=")
		if !ok || k != key {
			return ciCacheTrimReply{}, fmt.Errorf("the reply's line %d is not %s=N", i+1, key)
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return ciCacheTrimReply{}, fmt.Errorf("the reply's %s is not a whole number", key)
		}
		values[i] = n
	}
	return ciCacheTrimReply{beforeKiB: values[0], removed: values[1], afterKiB: values[2]}, nil
}

// ciCacheTrimSettings reads the CI trim's three settings from the patrol's
// block, returning the default for each one that is empty. A value the patrol
// cannot act on is an error naming its key.
func ciCacheTrimSettings(cfg *DaemonPatrolConfig) (time.Duration, time.Duration, int64, error) {
	var settings agentconfig.DiskMaintenanceConfig
	if c := diskMaintenanceSettings(cfg); c != nil {
		settings = *c
	}
	every, err := settings.CICacheTrimEvery()
	if err != nil {
		return 0, 0, 0, err
	}
	age, err := settings.CICacheMaxAgeDuration()
	if err != nil {
		return 0, 0, 0, err
	}
	warn, err := settings.CICacheWarnLimit()
	if err != nil {
		return 0, 0, 0, err
	}
	return every, age, warn, nil
}

// ciCacheTrimContext is the kubectl context of the control cluster the runner
// lives on: tier_sweep.runner.context, which a kubernetes sweep runs on. The
// second result is false when the town runs no kubernetes runner, so there is no
// control cluster and the trim is not configured. A kubernetes runner with a
// context that is empty, or that leads with a dash (the context reaches kubectl's
// argv), is refused rather than guessed at.
func ciCacheTrimContext(cfg *DaemonPatrolConfig) (string, bool, error) {
	runner := tierSweepRunnerConfig(cfg)
	if runner == nil || runner.Backend != agentconfig.TierSweepRunnerKubernetes {
		return "", false, nil
	}
	if runner.Context == "" || strings.HasPrefix(runner.Context, "-") {
		return "", true, errCICacheTrimNoContext
	}
	return runner.Context, true, nil
}

// ciCacheAgeText names an age the way the setting reads: "12h" for whole hours,
// "90m" for whole minutes.
func ciCacheAgeText(age time.Duration) string {
	if age%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(age/time.Hour))
	}
	return fmt.Sprintf("%dm", int64(age/time.Minute))
}

// ciCacheTrimTick is the CI trim's own tick. It runs before the quiet guard and
// on its own interval, so a busy town dispatches it as the shared cache trim is
// dispatched (gt-gg9hv.48). Its single-flight guard keeps one pass in flight, and
// the pass re-reads the due check when it starts. A setting the patrol cannot act
// on is refused once and runs nothing.
func (d *Daemon) runCICacheTrimTick() {
	s := d.diskMaintenanceSeams()
	now := s.now()
	// A town with no kubernetes runner has no control cluster to trim, so the
	// trim is not configured here and says nothing.
	if _, configured, _ := ciCacheTrimContext(d.patrolConfig); !configured {
		return
	}
	every, _, _, err := ciCacheTrimSettings(d.patrolConfig)
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.diskMaintSchedule.trimDue(ciCacheTrimName, now, every) {
		return
	}
	mode, err := diskMaintenanceMode(d.patrolConfig)
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.ciCacheTrimRunning.CompareAndSwap(false, true) {
		return // a pass is still running
	}
	s.dispatch(func() {
		defer d.ciCacheTrimRunning.Store(false)
		d.runCICacheTrimPass(mode)
	})
}

// runCICacheTrimPass is the pass the tick dispatches. The due check is read again
// here: a run that concluded after the tick's decision has spent the interval, so
// the trim must not run a second time in it.
func (d *Daemon) runCICacheTrimPass(mode string) {
	ctx, cancel := d.diskMaintenanceContext()
	defer cancel()
	s := d.diskMaintenanceSeams()
	every, _, _, err := ciCacheTrimSettings(d.patrolConfig)
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.diskMaintSchedule.trimDue(ciCacheTrimName, s.now(), every) {
		return
	}
	verdict := d.ciCacheTrim(ctx, mode)
	concluded := s.now()
	if verdict {
		d.diskMaintSchedule.record(ciCacheTrimName, concluded)
		return
	}
	// A run that could not reach a verdict is tried again soon, not after the
	// full interval, as the cache trim's failed run is (gt-gg9hv.24.6).
	d.diskMaintSchedule.recordRetry(ciCacheTrimName, concluded.Add(cacheTrimRetryInterval))
}

// ciCacheTrim runs one pass over the volume table and reports whether every volume
// reached a verdict. The runner's pod is listed once; each volume then gets its
// own exec, in table order, so a failure on one volume is logged under that
// volume and does not stop the next. A run that removed files, or found nothing
// to remove, is a verdict. A volume whose exec or reply failed is a failed run
// that says why and removes nothing: a missing path or a stopped container is
// never read as a volume with nothing to trim.
func (d *Daemon) ciCacheTrim(ctx context.Context, mode string) bool {
	s := d.diskMaintenanceSeams()
	_, maxAge, warn, err := ciCacheTrimSettings(d.patrolConfig)
	if err != nil {
		d.ciCacheTrimLogf("settings", "disk_maintenance: ci cache trim: %v; nothing trimmed", err)
		return false
	}
	kubeContext, _, err := ciCacheTrimContext(d.patrolConfig)
	if err != nil {
		d.ciCacheTrimLogf("context", "disk_maintenance: ci cache trim: %v; nothing trimmed", err)
		return false
	}
	kubeconfig := diskMaintKubeconfig(d.config.TownRoot, s.getenv)
	pod, err := d.ciCacheTrimPod(ctx, kubeconfig, kubeContext)
	if err != nil {
		kind := "pod listing"
		if errors.Is(err, errCICacheTrimNotRunning) {
			kind = "pod not running"
		}
		d.ciCacheTrimLogf(kind, "disk_maintenance: ci cache trim: %v; nothing trimmed", err)
		return false
	}

	at := ciCacheExecTarget{kubeconfig: kubeconfig, kubeContext: kubeContext, pod: pod}
	verdict := true
	for _, v := range ciCacheVolumes {
		if !d.ciCacheTrimVolume(ctx, mode, at, v, maxAge, warn) {
			verdict = false
		}
	}
	return verdict
}

// ciCacheTrimProgram is the program a volume's exec runs, and the arguments that
// follow its root and age. A dry run lists for every volume. An apply on a capped
// volume passes the cap, and an apply on an uncapped volume runs the age rule
// alone, exactly as the trim always has.
func ciCacheTrimProgram(mode string, v ciCacheVolume) (string, []string) {
	switch {
	case mode != agentconfig.DiskMaintenanceModeApply:
		return ciCacheListScript, nil
	case v.capKiB > 0:
		return ciCacheCappedTrimScript, []string{strconv.FormatInt(v.capKiB, 10)}
	default:
		return ciCacheTrimScript, nil
	}
}

// ciCacheTrimVolume runs the trim over one volume and reports whether it reached a
// verdict. A volume whose exec or reply failed is a failed run that names the
// volume and removes nothing.
func (d *Daemon) ciCacheTrimVolume(ctx context.Context, mode string, at ciCacheExecTarget, v ciCacheVolume, maxAge time.Duration, warn int64) bool {
	minutes := int64(maxAge / time.Minute)
	script, extra := ciCacheTrimProgram(mode, v)
	args := append([]string{"exec", "-n", ciCacheNamespace, at.pod, "-c", ciCacheContainer, "--",
		"sh", "-c", script, "ci-cache-trim", v.root, strconv.FormatInt(minutes, 10)}, extra...)
	stdout, stderr, err := d.ciCacheTrimKubectlRun(ctx, at.kubeconfig, at.kubeContext, args...)
	if err != nil {
		var status interface{ ExitCode() int }
		if errors.As(err, &status) && status.ExitCode() == ciCacheMissingPathExit {
			d.ciCacheTrimLogf("volume path missing "+v.name, "disk_maintenance: ci cache trim: %s: the volume path %s is not in the %s container; nothing trimmed", v.name, v.root, ciCacheContainer)
			return false
		}
		d.ciCacheTrimLogf("exec failed "+v.name, "disk_maintenance: ci cache trim: %s: the exec into the %s container failed: %v: %s; nothing trimmed",
			v.name, ciCacheContainer, err, lastLine(string(stderr)))
		return false
	}
	reply, err := parseCICacheTrimReply(stdout)
	if err != nil {
		d.ciCacheTrimLogf("reply unreadable "+v.name, "disk_maintenance: ci cache trim: %s: the volume's reply could not be read: %v; nothing trimmed", v.name, err)
		return false
	}

	before := polecatimage.Human(reply.beforeKiB * 1024)
	after := polecatimage.Human(reply.afterKiB * 1024)
	ageText := ciCacheAgeText(time.Duration(minutes) * time.Minute)
	switch {
	case mode == agentconfig.DiskMaintenanceModeApply:
		d.logger.Printf("disk_maintenance: ci cache trim: %s: removed %d files, %s to %s (older than %s)",
			v.name, reply.removed, before, after, ageText)
	case v.capKiB > 0:
		d.logger.Printf("disk_maintenance: ci cache trim dry_run: %s: would remove %d files, of %s (older than %s) while it is over its cap of %s; nothing removed",
			v.name, reply.removed, before, ageText, polecatimage.Human(v.capKiB*1024))
	default:
		d.logger.Printf("disk_maintenance: ci cache trim dry_run: %s: would remove %d files, of %s (older than %s); nothing removed",
			v.name, reply.removed, before, ageText)
	}
	d.ciCacheTrimMeasure(mode, v, reply, warn)
	return true
}

// ciCacheTrimMeasure raises or clears the volume's no-op item from a trim's
// reading. The threshold is the warn figure for an uncapped volume and the
// volume's cap for a capped one. A trim that removed no file while the volume is
// over the threshold raises the item, and a reading at or under it clears the
// item. A reading over the threshold after removals leaves the item as it is. A
// dry run measures nothing, since it removes nothing by design.
func (d *Daemon) ciCacheTrimMeasure(mode string, v ciCacheVolume, reply ciCacheTrimReply, warn int64) {
	if mode != agentconfig.DiskMaintenanceModeApply {
		return
	}
	key := ciCacheNoOpKeyOf(v)
	threshold, limit := warn, "the warn figure of "
	if v.capKiB > 0 {
		threshold, limit = v.capKiB*1024, "its cap of "
	}
	after := reply.afterKiB * 1024
	if after <= threshold {
		d.cacheTrimNoOp.clear(key)
		return
	}
	if reply.removed != 0 {
		return
	}
	d.cacheTrimNoOp.raise(attention.Item{
		Key:      key,
		Kind:     attention.KindCacheTrimNoOp,
		Severity: attention.SeverityLow,
		Summary: fmt.Sprintf("CI cache trim of %s removed no file while the volume holds %s, over %s%s: nothing there is older than the max age, or the trim is not reaching it",
			v.name, polecatimage.Human(after), limit, polecatimage.Human(threshold)),
	})
}

// ciCacheTrimLogf logs one refusal or failure line of the CI trim, at most once an
// hour for the same kind of fault. The hold keys on kind, not the expanded line: a
// failure carries the failing command's own output, which differs from one retry
// to the next, so a key on the expanded line would let every retry through. A
// key on the format would merge distinct faults that share a format, so each call
// names its kind.
func (d *Daemon) ciCacheTrimLogf(kind, format string, args ...any) {
	if d.diskMaintSchedule.allowLog("ci-cache-trim "+kind, d.diskMaintenanceSeams().now(), cacheTrimRefusalLogInterval) {
		d.logger.Printf("%s", fmt.Sprintf(format, args...))
	}
}

// ciCacheTrimPod names a runner pod whose dind container is running, the one the
// exec goes into. A reply with no item list refuses, because an unreadable reply
// would read as no runner at all.
func (d *Daemon) ciCacheTrimPod(ctx context.Context, kubeconfig, kubeContext string) (string, error) {
	out, stderr, err := d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext,
		"get", "pods", "-n", ciCacheNamespace, "-l", ciCachePodLabel, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("the runner's pods could not be read: %w: %s", err, lastLine(string(stderr)))
	}
	var list struct {
		Items *[]struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				ContainerStatuses []struct {
					Name  string `json:"name"`
					State struct {
						Running *struct{} `json:"running"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("the runner's pods could not be decoded: %w", err)
	}
	if list.Items == nil {
		return "", errors.New("the runner's pods could not be decoded: the reply names no item list")
	}
	for _, pod := range *list.Items {
		for _, c := range pod.Status.ContainerStatuses {
			if c.Name == ciCacheContainer && c.State.Running != nil && pod.Metadata.Name != "" {
				return pod.Metadata.Name, nil
			}
		}
	}
	return "", errCICacheTrimNotRunning
}

// ciCacheTrimKubectlRun runs one kubectl command against the control cluster.
// It names both the kubeconfig and the context, and runs with the same minimal
// environment the cache trim's kubectl does.
func (d *Daemon) ciCacheTrimKubectlRun(ctx context.Context, kubeconfig, kubeContext string, args ...string) ([]byte, []byte, error) {
	argv := append(diskmaint.KubectlArgs(kubeconfig, kubeContext), args...)
	cmd := exec.CommandContext(ctx, "kubectl", argv...) //nolint:gosec // G204: a fixed program and fixed verbs; the context is refused when it leads with a dash
	cmd.Env = cacheTrimKubectlEnv(d.diskMaintenanceSeams().environ())
	return d.runCmd(cmd)
}
