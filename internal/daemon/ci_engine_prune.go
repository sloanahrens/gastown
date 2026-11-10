package daemon

// The CI runner's docker-in-docker engine prune (gt-i02983). The runner's dind
// container keeps its Docker data root on the claim forgejo-runner-dind, which
// local-path does not bound, so the engine filled the control node's data disk
// and the node evicted the runner. ci_cache_trim trims one volume; this prunes the
// rest of the engine on its own interval (ci_engine_prune_interval), under the
// same patrol, the same exec into the runner's dind container and the same
// dry-run default.
//
// Each run is two execs. The list program prints the engine as JSON: the data
// root's size and the containers, images and volumes as docker inspects them. The
// planner, a pure function over that listing, decides what goes, so the keep rule
// is tested in-process. The remove program removes only the ids and names the
// planner named, passed as argv data, and prints the cache trim's three-figure
// reply. A dry run runs the list program alone and removes nothing.
//
// Build cache is pruned by docker itself (builder prune, unused for 24 hours), and
// is not sized in a dry run: docker has no dry run for it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

const (
	// ciEnginePruneName is the schedule key the engine prune's last run is recorded
	// under. It is not a target name, so it cannot collide with one.
	ciEnginePruneName = "ci_engine_prune"

	// ciEngineBaseRepo is the repository whose images keep a rollback: the latest
	// tag's id and the one before it. Every other ci-base id goes.
	ciEngineBaseRepo = "ci-base"

	// ciEngineBaseLatestTag is the tag that names the base image the gate runs.
	ciEngineBaseLatestTag = ciEngineBaseRepo + ":latest"

	// ciEngineRoot is the Docker data root the engine's size is read from: the
	// claim the dind container mounts.
	ciEngineRoot = ciCacheDindMount

	// ciEngineUnusedAge is how long a stopped container, an unused anonymous volume
	// and an unused build cache record must sit before the prune takes them.
	ciEngineUnusedAge = 24 * time.Hour

	// ciEngineBuildCacheFilter is the builder prune's age filter: build cache
	// unused for ciEngineUnusedAge.
	ciEngineBuildCacheFilter = "until=24h"
)

// ciEngineListScript is the list program. It prints one JSON object: the data
// root's size in KiB and the three inspected lists. It changes nothing.
const ciEngineListScript = `set -euf
root=$1
containers=$(docker ps -aq --no-trunc)
images=$(docker image ls -aq --no-trunc)
volumes=$(docker volume ls -q)
printf '{"data_root_kib":%s,"containers":' "$(du -sk "$root" | cut -f1)"
if [ -n "$containers" ]; then docker container inspect $containers; else printf '[]'; fi
printf ',"images":'
if [ -n "$images" ]; then docker image inspect $images; else printf '[]'; fi
printf ',"volumes":'
if [ -n "$volumes" ]; then docker volume inspect $volumes; else printf '[]'; fi
printf '}\n'
`

// ciEngineRemoveScript is the remove program. Its arguments are the data root and
// three space-separated lists: container ids, image ids and volume names, each
// one the planner validated. The removal stops at the first failure (set -e), so
// the reply is printed only when every named item went. The build cache is pruned
// last, unused for 24 hours. Its reply is the cache trim's three figures.
const ciEngineRemoveScript = `set -euf
root=$1; containers=$2; images=$3; volumes=$4
before=$(du -sk "$root" | cut -f1)
removed=0
for id in $containers; do docker rm "$id" >/dev/null; removed=$((removed + 1)); done
for id in $images; do docker rmi -f "$id" >/dev/null; removed=$((removed + 1)); done
for name in $volumes; do docker volume rm "$name" >/dev/null; removed=$((removed + 1)); done
docker builder prune -af --filter ` + ciEngineBuildCacheFilter + ` >/dev/null
after=$(du -sk "$root" | cut -f1)
echo "before_kib=$before"
echo "removed=$removed"
echo "after_kib=$after"
`

var (
	// ciEngineContainerIDRE is a full container id, as docker inspect prints it.
	ciEngineContainerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// ciEngineImageIDRE is a full image id, as docker inspect prints it.
	ciEngineImageIDRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// ciEngineVolumeNameRE is a volume name docker accepts. The check keeps a name
	// from reaching the remove program's argv as anything but a name.
	ciEngineVolumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	// ciEngineAnonymousVolumeRE is an anonymous volume's name: the 32 hex digits
	// docker gives one. Named volumes never match, so ci-gocache, ci-gomod and
	// ci-npm are never candidates.
	ciEngineAnonymousVolumeRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ciEngineListing is the JSON the list program prints.
type ciEngineListing struct {
	// DataRootKiB is a pointer so a listing without the figure is refused, not read
	// as an empty data root.
	DataRootKiB *int64              `json:"data_root_kib"`
	Containers  []ciEngineContainer `json:"containers"`
	Images      []ciEngineImage     `json:"images"`
	Volumes     []ciEngineVolume    `json:"volumes"`
}

// ciEngineContainer is the part of a container inspect the planner reads.
type ciEngineContainer struct {
	ID      string `json:"Id"`
	Image   string `json:"Image"`
	Created string `json:"Created"`
	State   struct {
		Status string `json:"Status"`
	} `json:"State"`
	Mounts []struct {
		Type string `json:"Type"`
		Name string `json:"Name"`
	} `json:"Mounts"`
}

// ciEngineImage is the part of an image inspect the planner reads. Layers is the
// image's rootfs chain: a parent image's layers are a strict prefix of its
// children's, which is how docker tells a parent it cannot remove.
type ciEngineImage struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Created  string   `json:"Created"`
	RootFS   struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

// ciEngineVolume is the part of a volume inspect the planner reads.
type ciEngineVolume struct {
	Name      string `json:"Name"`
	CreatedAt string `json:"CreatedAt"`
}

// ciEnginePlan is what a run removes, in the order the remove program takes it:
// containers first, so the images they used are free; images most layers first,
// so a child goes before its parent; volumes by name.
type ciEnginePlan struct {
	containers []ciEngineContainer
	images     []ciEngineImage
	volumes    []ciEngineVolume
}

// count is the number of items the plan removes.
func (p ciEnginePlan) count() int {
	return len(p.containers) + len(p.images) + len(p.volumes)
}

// planCIEnginePrune decides what the engine prune removes from a listing taken at
// now. It refuses a listing it cannot read in full: an id or name that is not in
// docker's shape, a time it cannot parse, or a missing data root figure. A refusal
// is a failed run, never an empty plan.
//
// The keep rule:
//   - a container is removed when it is stopped and was created more than the
//     unused age ago;
//   - an image used by a container that stays is kept;
//   - a ci-base image is kept only when it is the latest tag's id or the id before
//     it, and is removed otherwise, with no age gate;
//   - any other image with a tag in keepRepos is kept;
//   - any other image is removed when it was created more than age ago;
//   - an image that is a parent of a kept image is kept, as docker would refuse;
//   - an anonymous volume is removed when no container that stays mounts it, no
//     volume named <name>-env exists, and it was created more than the unused age
//     ago.
func planCIEnginePrune(l ciEngineListing, now time.Time, age time.Duration, keepRepos []string) (ciEnginePlan, error) {
	var plan ciEnginePlan
	if l.DataRootKiB == nil {
		return plan, errors.New("the engine listing names no data root size")
	}

	usedImage := map[string]bool{}
	usedVolume := map[string]bool{}
	for _, c := range l.Containers {
		if !ciEngineContainerIDRE.MatchString(c.ID) || !ciEngineImageIDRE.MatchString(c.Image) {
			return plan, errors.New("the engine listing names a container with an id docker does not print")
		}
		created, err := parseCIEngineTime(c.Created)
		if err != nil {
			return plan, fmt.Errorf("container %s: %w", c.ID[:12], err)
		}
		if ciEngineStopped(c.State.Status) && now.Sub(created) > ciEngineUnusedAge {
			plan.containers = append(plan.containers, c)
			continue
		}
		usedImage[c.Image] = true
		for _, m := range c.Mounts {
			if m.Type == "volume" {
				usedVolume[m.Name] = true
			}
		}
	}

	images := make(map[string]ciEngineImage, len(l.Images))
	created := make(map[string]time.Time, len(l.Images))
	for _, img := range l.Images {
		if !ciEngineImageIDRE.MatchString(img.ID) {
			return plan, errors.New("the engine listing names an image with an id docker does not print")
		}
		t, err := parseCIEngineTime(img.Created)
		if err != nil {
			return plan, fmt.Errorf("image %s: %w", img.ID[:19], err)
		}
		images[img.ID] = img
		created[img.ID] = t
	}

	baseKeep := ciEngineBaseKeep(images, created)

	removable := map[string]bool{}
	for id, img := range images {
		if usedImage[id] {
			continue
		}
		repos := ciEngineRepoTags(img.RepoTags)
		if slices.Contains(repos, ciEngineBaseRepo) {
			if !baseKeep[id] {
				removable[id] = true
			}
			continue
		}
		if anyOf(repos, keepRepos) {
			continue
		}
		if now.Sub(created[id]) > age {
			removable[id] = true
		}
	}

	// A candidate that is a parent of an image that stays cannot go: docker refuses
	// a parent with dependent children. Removing one candidate can free another, so
	// the check repeats until no candidate changes.
	for changed := true; changed; {
		changed = false
		for id := range removable {
			for otherID, other := range images {
				if removable[otherID] || otherID == id {
					continue
				}
				if layersStrictPrefix(images[id].RootFS.Layers, other.RootFS.Layers) {
					delete(removable, id)
					changed = true
					break
				}
			}
		}
	}

	for id := range removable {
		plan.images = append(plan.images, images[id])
	}
	slices.SortFunc(plan.images, func(a, b ciEngineImage) int {
		if d := len(b.RootFS.Layers) - len(a.RootFS.Layers); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
	slices.SortFunc(plan.containers, func(a, b ciEngineContainer) int { return strings.Compare(a.ID, b.ID) })

	names := make(map[string]bool, len(l.Volumes))
	for _, v := range l.Volumes {
		if !ciEngineVolumeNameRE.MatchString(v.Name) {
			return plan, errors.New("the engine listing names a volume that is not a volume name")
		}
		names[v.Name] = true
	}
	for _, v := range l.Volumes {
		t, err := parseCIEngineTime(v.CreatedAt)
		if err != nil {
			return plan, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if !ciEngineAnonymousVolumeRE.MatchString(v.Name) || names[v.Name+"-env"] || usedVolume[v.Name] {
			continue
		}
		if now.Sub(t) > ciEngineUnusedAge {
			plan.volumes = append(plan.volumes, v)
		}
	}
	slices.SortFunc(plan.volumes, func(a, b ciEngineVolume) int { return strings.Compare(a.Name, b.Name) })
	return plan, nil
}

// ciEngineBaseKeep returns the ci-base image ids the prune keeps: the latest tag's
// id and the newest id after it. With no latest tag, the newest ci-base id is the
// latest, so a town that has not tagged one still keeps its newest base.
func ciEngineBaseKeep(images map[string]ciEngineImage, created map[string]time.Time) map[string]bool {
	keep := map[string]bool{}
	var base []string
	for id, img := range images {
		if slices.Contains(ciEngineRepoTags(img.RepoTags), ciEngineBaseRepo) {
			base = append(base, id)
		}
	}
	// Newest first, the id breaking a tie so the choice is the same on every run.
	slices.SortFunc(base, func(a, b string) int {
		if c := created[b].Compare(created[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	if len(base) == 0 {
		return keep
	}
	latest := base[0]
	for _, id := range base {
		if slices.Contains(images[id].RepoTags, ciEngineBaseLatestTag) {
			latest = id
		}
	}
	keep[latest] = true
	for _, id := range base {
		if id != latest {
			keep[id] = true // the previous id: the newest after the latest
			break
		}
	}
	return keep
}

// ciEngineRepoTags returns the repository of each tag. A tag names its repository
// before the last colon, when that colon follows the last slash, so a registry
// port is not taken for a tag.
func ciEngineRepoTags(tags []string) []string {
	repos := make([]string, 0, len(tags))
	for _, tag := range tags {
		repo := tag
		if i := strings.LastIndex(tag, ":"); i > strings.LastIndex(tag, "/") {
			repo = tag[:i]
		}
		repos = append(repos, repo)
	}
	return repos
}

// anyOf reports whether any element of names is in set.
func anyOf(names, set []string) bool {
	for _, n := range names {
		if slices.Contains(set, n) {
			return true
		}
	}
	return false
}

// layersStrictPrefix reports whether parent is a strict prefix of child: the
// layers docker stacks for a parent image, with the child's own above them. An
// image with no layers is a prefix of every image with some, which keeps it.
func layersStrictPrefix(parent, child []string) bool {
	if len(parent) >= len(child) {
		return false
	}
	return slices.Equal(parent, child[:len(parent)])
}

// ciEngineStopped reports whether a container status is one docker's stopped
// containers carry: created, exited or dead.
func ciEngineStopped(status string) bool {
	switch status {
	case "created", "exited", "dead":
		return true
	}
	return false
}

// parseCIEngineTime reads a time docker prints, RFC 3339 with its fraction. A
// zero time is refused: it would read as created long ago, and every age check
// would then remove the item.
func parseCIEngineTime(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New("a creation time is not RFC 3339")
	}
	if t.IsZero() {
		return time.Time{}, errors.New("a creation time is zero")
	}
	return t, nil
}

// parseCIEngineListing reads the list program's JSON, which must decode fully:
// any other output is a failed run.
func parseCIEngineListing(out []byte) (ciEngineListing, error) {
	var l ciEngineListing
	if err := json.Unmarshal(out, &l); err != nil {
		return ciEngineListing{}, fmt.Errorf("the engine listing does not decode: %w", err)
	}
	return l, nil
}

// ciEnginePruneSettings reads the engine prune's three settings from the patrol's
// block, returning the default for each that is empty. A value the patrol cannot
// act on is an error naming its key.
func ciEnginePruneSettings(cfg *DaemonPatrolConfig) (time.Duration, time.Duration, []string, error) {
	var settings agentconfig.DiskMaintenanceConfig
	if c := diskMaintenanceSettings(cfg); c != nil {
		settings = *c
	}
	every, err := settings.CIEnginePruneEvery()
	if err != nil {
		return 0, 0, nil, err
	}
	age, err := settings.CIEnginePruneAgeDuration()
	if err != nil {
		return 0, 0, nil, err
	}
	repos, err := settings.CIEngineKeepRepoList()
	if err != nil {
		return 0, 0, nil, err
	}
	return every, age, repos, nil
}

// runCIEnginePruneTick is the engine prune's own tick. It runs before the quiet
// guard and on its own interval, as the CI cache trim does. Its single-flight
// guard keeps one pass in flight, and the pass re-reads the due check when it
// starts.
func (d *Daemon) runCIEnginePruneTick() {
	s := d.diskMaintenanceSeams()
	now := s.now()
	_, configured, err := ciCacheTrimContext(d.patrolConfig)
	if !configured {
		return
	}
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	every, _, _, err := ciEnginePruneSettings(d.patrolConfig)
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.diskMaintSchedule.trimDue(ciEnginePruneName, now, every) {
		return
	}
	mode, err := diskMaintenanceMode(d.patrolConfig)
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.ciEnginePruneRunning.CompareAndSwap(false, true) {
		return // a pass is still running
	}
	s.dispatch(func() {
		defer d.ciEnginePruneRunning.Store(false)
		d.runCIEnginePrunePass(mode)
	})
}

// runCIEnginePrunePass is the pass the tick dispatches. The due check is read again
// here, so a run that concluded after the tick's decision does not run twice.
func (d *Daemon) runCIEnginePrunePass(mode string) {
	ctx, cancel := d.diskMaintenanceContext()
	defer cancel()
	s := d.diskMaintenanceSeams()
	every, _, _, err := ciEnginePruneSettings(d.patrolConfig)
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.diskMaintSchedule.trimDue(ciEnginePruneName, s.now(), every) {
		return
	}
	verdict := d.ciEnginePrune(ctx, mode)
	concluded := s.now()
	if verdict {
		d.diskMaintSchedule.record(ciEnginePruneName, concluded)
		return
	}
	// A run that could not reach a verdict is tried again soon, not after the full
	// interval, as the cache trim's failed run is.
	d.diskMaintSchedule.recordRetry(ciEnginePruneName, concluded.Add(cacheTrimRetryInterval))
}

// ciEnginePrune runs the engine prune once and reports whether it reached a
// verdict. A run that cannot list the engine, plan from the listing, or read the
// remove program's reply is a failed run that says why. A run that fails before
// the remove program removes nothing; one whose remove program fails partway has
// removed the items the program took before the failure, and the next run plans
// again from what is left.
func (d *Daemon) ciEnginePrune(ctx context.Context, mode string) bool {
	s := d.diskMaintenanceSeams()
	_, maxAge, keepRepos, err := ciEnginePruneSettings(d.patrolConfig)
	if err != nil {
		d.ciEnginePruneLogf("settings", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false
	}
	kubeContext, _, err := ciCacheTrimContext(d.patrolConfig)
	if err != nil {
		d.ciEnginePruneLogf("context", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false
	}
	kubeconfig := diskMaintKubeconfig(d.config.TownRoot, s.getenv)
	pod, err := d.ciCacheTrimPod(ctx, kubeconfig, kubeContext)
	if err != nil {
		kind := "pod listing"
		if errors.Is(err, errCICacheTrimNotRunning) {
			kind = "pod not running"
		}
		d.ciEnginePruneLogf(kind, "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false
	}

	// The captured output stays out of the log: a program's stderr can carry what
	// the engine holds, so the line names the exec and its exit status only.
	stdout, _, err := d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext,
		"exec", "-n", ciCacheNamespace, pod, "-c", ciCacheContainer, "--",
		"sh", "-c", ciEngineListScript, "ci-engine-list", ciEngineRoot)
	if err != nil {
		d.ciEnginePruneLogf("list failed", "disk_maintenance: ci engine prune: the listing exec into the %s container failed: %v; nothing pruned",
			ciCacheContainer, err)
		return false
	}
	listing, err := parseCIEngineListing(stdout)
	if err != nil {
		d.ciEnginePruneLogf("listing unreadable", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false
	}
	plan, err := planCIEnginePrune(listing, s.now(), maxAge, keepRepos)
	if err != nil {
		d.ciEnginePruneLogf("plan refused", "disk_maintenance: ci engine prune: the listing could not be planned: %v; nothing pruned", err)
		return false
	}

	before := polecatimage.Human(*listing.DataRootKiB * 1024)
	counts := fmt.Sprintf("%d containers, %d images, %d volumes", len(plan.containers), len(plan.images), len(plan.volumes))
	if mode != agentconfig.DiskMaintenanceModeApply {
		for _, c := range plan.containers {
			d.logger.Printf("disk_maintenance: ci engine prune dry_run: would remove container %s", c.ID[:12])
		}
		for _, img := range plan.images {
			d.logger.Printf("disk_maintenance: ci engine prune dry_run: would remove image %s %v", strings.TrimPrefix(img.ID, "sha256:")[:12], img.RepoTags)
		}
		for _, v := range plan.volumes {
			d.logger.Printf("disk_maintenance: ci engine prune dry_run: would remove volume %s", v.Name)
		}
		d.logger.Printf("disk_maintenance: ci engine prune dry_run: would remove %s, of %s, and prune build cache unused for 24h (not sized in a dry run); nothing removed",
			counts, before)
		return true
	}

	ids := make([]string, 0, len(plan.containers))
	for _, c := range plan.containers {
		ids = append(ids, c.ID)
	}
	images := make([]string, 0, len(plan.images))
	for _, img := range plan.images {
		images = append(images, img.ID)
	}
	volumes := make([]string, 0, len(plan.volumes))
	for _, v := range plan.volumes {
		volumes = append(volumes, v.Name)
	}
	stdout, _, err = d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext,
		"exec", "-n", ciCacheNamespace, pod, "-c", ciCacheContainer, "--",
		"sh", "-c", ciEngineRemoveScript, "ci-engine-remove", ciEngineRoot,
		strings.Join(ids, " "), strings.Join(images, " "), strings.Join(volumes, " "))
	if err != nil {
		d.ciEnginePruneLogf("remove failed", "disk_maintenance: ci engine prune: the remove exec into the %s container failed: %v; the run is retried",
			ciCacheContainer, err)
		return false
	}
	reply, err := parseCIEnginePruneReply(stdout)
	if err != nil {
		d.ciEnginePruneLogf("reply unreadable", "disk_maintenance: ci engine prune: the remove reply could not be read: %v; the run is retried", err)
		return false
	}
	if reply.removed != int64(plan.count()) {
		d.ciEnginePruneLogf("count mismatch", "disk_maintenance: ci engine prune: the remove reply names %d removals, the plan named %d; the run is retried",
			reply.removed, plan.count())
		return false
	}
	d.logger.Printf("disk_maintenance: ci engine prune: removed %s, %s to %s",
		counts, before, polecatimage.Human(reply.afterKiB*1024))
	return true
}

// parseCIEnginePruneReply reads the remove program's reply: the cache trim's three
// figures, read by the same strict reader, so a reply that is not exactly those
// three lines is never read as zeros.
func parseCIEnginePruneReply(out []byte) (ciCacheTrimReply, error) {
	return parseCICacheTrimReply(out)
}

// ciEnginePruneLogf logs one refusal or failure line of the engine prune, at most
// once an hour for the same kind of fault, as the cache trim's lines are held. The
// hold keys on kind, so the engine's lines do not share the cache trim's.
func (d *Daemon) ciEnginePruneLogf(kind, format string, args ...any) {
	if d.diskMaintSchedule.allowLog("ci-engine-prune "+kind, d.diskMaintenanceSeams().now(), cacheTrimRefusalLogInterval) {
		d.logger.Printf("%s", fmt.Sprintf(format, args...))
	}
}
