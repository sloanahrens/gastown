package daemon

// The CI runner's docker-in-docker engine prune. It bounds the Docker data root in
// the runner's dind container: images, stopped containers, build cache and
// anonymous volumes are removed on an interval (ci_engine_prune_interval), under
// the disk maintenance patrol, through the same exec into the dind container and
// with the same dry-run default as the cache trim.
//
// Each run lists, plans, then removes. The list program prints the engine as
// JSON. The planner, a pure function over that listing, decides what goes, so the
// keep rule is tested in-process. The remove step is one docker command per kind
// the plan names (docker rm, docker rmi -f, docker volume rm), each over that
// kind's ids as argv with no shell between the plan and docker. What each command
// did is read back from docker: the ids it printed removed, and for any it did not
// print, one docker inspect that names which of them are still in the engine. An
// id still there fails the run; an id gone counts as removed, whatever the
// command's exit status said, since docker rmi -f exits 1 over an image a
// previous argument already freed. Every kind and the build cache prune run
// whatever an earlier one did, so one refused image never keeps the cache from
// being pruned. A dry run runs the list program alone. The keep rule is written once, in
// docs/guides/disk-maintenance.md: read it before changing what is removed.
//
// Build cache is not sized in a dry run, since docker has no dry run for it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

const (
	// ciEnginePruneName is the schedule key the engine prune's last run is recorded
	// under. It is not a target name, so it cannot collide with one.
	ciEnginePruneName = "ci_engine_prune"

	// ciEngineBaseRepo is the base image repository the rollback rule applies to.
	ciEngineBaseRepo = "ci-base"

	// ciEngineBaseLatestTag is the tag that names the base image the gate runs.
	ciEngineBaseLatestTag = ciEngineBaseRepo + ":latest"

	// ciEngineRoot is the Docker data root the engine's size is read from: the
	// claim the dind container mounts.
	ciEngineRoot = ciCacheDindMount

	// ciEngineUnusedAge is the age the keep rule gives a stopped container, an unused
	// anonymous volume and an unused build cache record. See the guide.
	ciEngineUnusedAge = 24 * time.Hour
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

// ciEngineRemoval is one docker command of the remove step: the kind it removes,
// the argv docker runs (the verb and the plan's ids, nothing else) and the ids
// docker must report removed for the step to count.
type ciEngineRemoval struct {
	kind string
	argv []string
	ids  []string
}

// ciEngineRemovals is the remove step as docker commands: one per kind the plan
// names, each over that kind's ids as argv. No shell runs and nothing counts: a
// kind with no ids is not sent, and what was removed is read from docker's output
// (ciEngineMissing), never from the number of ids passed.
func ciEngineRemovals(plan ciEnginePlan) []ciEngineRemoval {
	var out []ciEngineRemoval
	if len(plan.containers) > 0 {
		ids := make([]string, 0, len(plan.containers))
		for _, c := range plan.containers {
			ids = append(ids, c.ID)
		}
		out = append(out, ciEngineRemoval{kind: "container", argv: append([]string{"docker", "rm"}, ids...), ids: ids})
	}
	if len(plan.images) > 0 {
		ids := make([]string, 0, len(plan.images))
		for _, img := range plan.images {
			ids = append(ids, img.ID)
		}
		out = append(out, ciEngineRemoval{kind: "image", argv: append([]string{"docker", "rmi", "-f"}, ids...), ids: ids})
	}
	if len(plan.volumes) > 0 {
		names := make([]string, 0, len(plan.volumes))
		for _, v := range plan.volumes {
			names = append(names, v.Name)
		}
		out = append(out, ciEngineRemoval{kind: "volume", argv: append([]string{"docker", "volume", "rm"}, names...), ids: names})
	}
	return out
}

// presenceArgv is the docker inspect that prints which of ids the engine still
// holds: the id (the name for a volume) of each object found, one per line. It
// exits 1 when any id is gone, which is the case it exists to find, so a caller
// reads its output and not its status.
func (r ciEngineRemoval) presenceArgv(ids []string) []string {
	format := "{{.Id}}"
	if r.kind == "volume" {
		format = "{{.Name}}"
	}
	return append([]string{"docker", r.kind, "inspect", "--format", format}, ids...)
}

// ciEngineBuildCachePruneArgv is the build cache prune: everything unused for
// unusedAge. docker's until filter takes a duration, not a time.
func ciEngineBuildCachePruneArgv(unusedAge time.Duration) []string {
	return []string{"docker", "builder", "prune", "-af", "--filter", "until=" + ciEngineUntil(unusedAge)}
}

// ciEngineUntil spells an age as the duration docker's until filter parses: whole
// hours as "24h", otherwise whole minutes as "90m".
func ciEngineUntil(age time.Duration) string {
	if age%time.Hour == 0 {
		return strconv.FormatInt(int64(age/time.Hour), 10) + "h"
	}
	return strconv.FormatInt(int64(age/time.Minute), 10) + "m"
}

// ciEngineRemoved reads what docker reported removed. docker rm and docker volume
// rm print each removed id or name on its own line; docker rmi prints "Deleted: "
// before each image id it deleted and "Untagged: " before each tag it dropped,
// which names no removal. Any other line is not a removal.
func ciEngineRemoved(out []byte) map[string]bool {
	removed := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "", strings.HasPrefix(line, "Untagged: "):
		case strings.HasPrefix(line, "Deleted: "):
			removed[strings.TrimSpace(strings.TrimPrefix(line, "Deleted: "))] = true
		default:
			removed[line] = true
		}
	}
	return removed
}

// ciEngineMissing returns the ids of r that docker's output does not report
// removed, in the plan's order.
func ciEngineMissing(r ciEngineRemoval, out []byte) []string {
	removed := ciEngineRemoved(out)
	var missing []string
	for _, id := range r.ids {
		if !removed[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

// ciEnginePresent reads the output of a presence inspect for a kind of object: the
// set of ids (names, for a volume) docker printed. A line that is not in the
// shape docker prints that kind's id in names nothing, so text docker prints
// beside the ids never marks an object present.
func ciEnginePresent(kind string, out []byte) map[string]bool {
	present := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		var ok bool
		switch kind {
		case "container":
			ok = ciEngineContainerIDRE.MatchString(line)
		case "image":
			ok = ciEngineImageIDRE.MatchString(line)
		case "volume":
			ok = ciEngineVolumeNameRE.MatchString(line)
		}
		if ok {
			present[line] = true
		}
	}
	return present
}

var (
	// ciEngineContainerIDRE is a full container id, as docker inspect prints it.
	ciEngineContainerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// ciEngineImageIDRE is a full image id, as docker inspect prints it.
	ciEngineImageIDRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// ciEngineVolumeNameRE is a volume name docker accepts. The check keeps a name
	// from reaching the remove program's argv as anything but a name.
	ciEngineVolumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	// ciEngineAnonymousVolumeRE is an anonymous volume's name: the 64 hex digits
	// docker gives one. Named volumes never match.
	ciEngineAnonymousVolumeRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
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
// now, under the keep rule in docs/guides/disk-maintenance.md. It refuses a listing
// it cannot read in full: an id or name that is not in docker's shape, a time it
// cannot parse, or a missing data root figure. A refusal is a failed run, never an
// empty plan.
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

	// ci-base in keepRepos is the default and names the rollback's repository, not a
	// shield: its images follow the base rule above, so a default list does not keep
	// every old base. Any other kept repository protects its images, ci-base tags
	// included.
	protected := slices.DeleteFunc(slices.Clone(keepRepos), func(r string) bool { return r == ciEngineBaseRepo })

	removable := map[string]bool{}
	for id, img := range images {
		if usedImage[id] {
			continue
		}
		repos := ciEngineRepoTags(img.RepoTags)
		if anyOf(repos, protected) {
			continue
		}
		if slices.Contains(repos, ciEngineBaseRepo) {
			// A base the rollback rule does not keep still waits out the age, like
			// any other image: a build just made and not yet tagged latest is not
			// swept away under the runner (gt-i02983, review round 3).
			if !baseKeep[id] && now.Sub(created[id]) > age {
				removable[id] = true
			}
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

	for _, v := range l.Volumes {
		if !ciEngineVolumeNameRE.MatchString(v.Name) {
			return plan, errors.New("the engine listing names a volume that is not a volume name")
		}
	}
	for _, v := range l.Volumes {
		t, err := parseCIEngineTime(v.CreatedAt)
		if err != nil {
			return plan, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if !ciEngineAnonymousVolumeRE.MatchString(v.Name) || usedVolume[v.Name] {
			continue
		}
		if now.Sub(t) > ciEngineUnusedAge {
			plan.volumes = append(plan.volumes, v)
		}
	}
	slices.SortFunc(plan.volumes, func(a, b ciEngineVolume) int { return strings.Compare(a.Name, b.Name) })
	return plan, nil
}

// ciEngineBaseKeep returns the ci-base image ids the rollback rule keeps. The rule
// is in docs/guides/disk-maintenance.md.
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
	// base is newest first, so the first id older than the latest is the one built
	// just before it.
	for _, id := range base {
		if created[id].Before(created[latest]) {
			keep[id] = true
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
// any other output is a failed run. The decoder's own error is not returned: it
// quotes the bytes it refused, which are the engine's output.
func parseCIEngineListing(out []byte) (ciEngineListing, error) {
	var l ciEngineListing
	if err := json.Unmarshal(out, &l); err != nil {
		return ciEngineListing{}, errors.New("the engine listing does not decode as the list program's JSON")
	}
	return l, nil
}

// ciEngineExitStatus names a failed exec by its exit status alone. ciEngineFailure
// starts with it and adds the bounded first stderr line; the pod lookup logs no
// more than this, since kubectl's stderr there is never logged.
func ciEngineExitStatus(err error) string {
	var status interface{ ExitCode() int }
	if errors.As(err, &status) {
		return fmt.Sprintf("exit status %d", status.ExitCode())
	}
	return "no exit status"
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
	_, configured, err := ciCacheTrimContext(d.patrol())
	if !configured {
		return
	}
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	every, _, _, err := ciEnginePruneSettings(d.patrol())
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.diskMaintSchedule.trimDue(ciEnginePruneName, now, every) {
		return
	}
	mode, err := diskMaintenanceMode(d.patrol())
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
	every, _, _, err := ciEnginePruneSettings(d.patrol())
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
// verdict. A run that cannot list the engine or plan from the listing removes
// nothing and says why. Once it removes, every kind and the build cache prune run
// whatever an earlier one did; a kind docker left in place, or that could not be
// checked, fails the run after the rest have run, and the next run plans again
// from what is left.
func (d *Daemon) ciEnginePrune(ctx context.Context, mode string) bool {
	s := d.diskMaintenanceSeams()
	_, maxAge, keepRepos, err := ciEnginePruneSettings(d.patrol())
	if err != nil {
		d.ciEnginePruneLogf("settings", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false
	}
	kubeContext, _, err := ciCacheTrimContext(d.patrol())
	if err != nil {
		d.ciEnginePruneLogf("context", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false
	}
	kubeconfig := diskMaintKubeconfig(d.config.TownRoot, s.getenv)
	pod, err := d.ciCacheTrimPod(ctx, kubeconfig, kubeContext)
	if err != nil {
		// The lookup's error can carry kubectl's stderr, so only the sentinel's text
		// or the exit status is logged.
		if errors.Is(err, errCICacheTrimNotRunning) {
			d.ciEnginePruneLogf("pod not running", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		} else {
			d.ciEnginePruneLogf("pod lookup", "disk_maintenance: ci engine prune: the runner's pod lookup failed (%s); nothing pruned", ciEngineExitStatus(err))
		}
		return false
	}

	// A failed exec's log line names its exit status and the first line of its
	// stderr, bounded; ciEngineFailure never lets stdout or a second line through.
	stdout, stderr, err := d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext,
		"exec", "-n", ciCacheNamespace, pod, "-c", ciCacheContainer, "--",
		"sh", "-c", ciEngineListScript, "ci-engine-list", ciEngineRoot)
	if err != nil {
		d.ciEnginePruneLogf("list failed", "disk_maintenance: ci engine prune: the listing exec into the %s container failed (%s); nothing pruned",
			ciCacheContainer, ciEngineFailure(err, stderr))
		return false
	}
	listing, err := parseCIEngineListing(stdout)
	if err != nil {
		d.ciEnginePruneLogf("listing unreadable", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false
	}
	now := s.now()
	plan, err := planCIEnginePrune(listing, now, maxAge, keepRepos)
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
		d.logger.Printf("disk_maintenance: ci engine prune dry_run: would remove %s, of %s, and prune build cache (not sized in a dry run); nothing removed",
			counts, before)
		return true
	}

	// Every kind and the build cache prune run whatever an earlier one did; each
	// failure is logged on its own line and the run fails once they have all run.
	failed := false
	for _, r := range ciEngineRemovals(plan) {
		if !d.ciEngineRemoveKind(ctx, kubeconfig, kubeContext, pod, r) {
			failed = true
		}
	}
	if _, stderr, err := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, ciEngineBuildCachePruneArgv(ciEngineUnusedAge)...); err != nil {
		d.ciEnginePruneLogf("build cache prune failed", "disk_maintenance: ci engine prune: the build cache prune in the %s container failed (%s); the run is retried",
			ciCacheContainer, ciEngineFailure(err, stderr))
		failed = true
	}
	afterKiB, err := d.ciEngineDataRootKiB(ctx, kubeconfig, kubeContext, pod)
	if err != nil {
		d.ciEnginePruneLogf("size unreadable", "disk_maintenance: ci engine prune: the data root's size after could not be read: %v; the run is retried", err)
		return false
	}
	if failed {
		d.logger.Printf("disk_maintenance: ci engine prune: a removal failed; the engine is %s, from %s; the run is retried", polecatimage.Human(afterKiB*1024), before)
		return false
	}
	d.logger.Printf("disk_maintenance: ci engine prune: removed %s, %s to %s",
		counts, before, polecatimage.Human(afterKiB*1024))
	return true
}

// ciEngineRemoveKind runs one kind's removal and reports whether every planned id
// is gone from the engine afterwards. The command's exit status is not the
// answer: docker rmi -f exits 1 over an id an earlier argument already freed. The
// answer is docker's own report, then one presence inspect over the ids it did
// not print. A failure is logged here, naming the kind, the exit status and the
// first line of docker's stderr.
func (d *Daemon) ciEngineRemoveKind(ctx context.Context, kubeconfig, kubeContext, pod string, r ciEngineRemoval) bool {
	stdout, stderr, runErr := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, r.argv...)
	missing := ciEngineMissing(r, stdout)
	if len(missing) == 0 {
		return true
	}
	// The inspect exits 1 whenever one of the ids is gone, so only a failure to run
	// it at all, not a status, stops the answer.
	seen, seenErr, inspectErr := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, r.presenceArgv(missing)...)
	if !ciEngineRan(inspectErr) {
		d.ciEnginePruneLogf("presence "+r.kind, "disk_maintenance: ci engine prune: the %s presence check could not run (%s); the run is retried", r.kind, ciEngineFailure(inspectErr, seenErr))
		return false
	}
	present := ciEnginePresent(r.kind, seen)
	left := 0
	for _, id := range missing {
		if present[id] {
			left++
		}
	}
	if left == 0 {
		return true
	}
	d.ciEnginePruneLogf("not removed "+r.kind, "disk_maintenance: ci engine prune: docker left %d of %d planned %s(s) in the %s container (the removal ended: %s); the run is retried",
		left, len(r.ids), r.kind, ciCacheContainer, ciEngineFailure(runErr, stderr))
	return false
}

// ciEngineRan reports whether an exec reached its program: it succeeded or the
// program exited with a status. An error with no status means it never ran.
func ciEngineRan(err error) bool {
	var status interface{ ExitCode() int }
	return err == nil || errors.As(err, &status)
}

// ciEngineStderrMax is the longest first stderr line a failure line carries, in
// characters.
const ciEngineStderrMax = 200

// ciEngineKubectlErrorPrefixes are the lines kubectl prints for its own failures
// about the pod, the context or the connection, and its warnings. kubectl relays the
// engine's stderr on the same stream, so these are skipped to reach the program's own
// line. The lines klog writes for kubectl are matched by ciEngineKlogHeader instead.
var ciEngineKubectlErrorPrefixes = []string{
	"error: ",
	"Error from server",
	"Unable to connect to the server",
	"The connection to the server",
	"Warning: ",
}

// ciEngineKlogHeader matches the header klog writes before each line it logs: a
// severity letter, the date as MMDD, then the time with its fraction.
var ciEngineKlogHeader = regexp.MustCompile(`^[IWEF]\d{4} \d{2}:\d{2}:\d{2}\.\d+ `)

// ciEngineFailure names how an engine exec ended for the log: its exit status, then,
// for a failed exec, the first line of its stderr. A nil error is exit status 0 with
// no stderr, since a command that succeeded has nothing to say about a failure.
func ciEngineFailure(err error, stderr []byte) string {
	if err == nil {
		return "exit status 0"
	}
	status := ciEngineExitStatus(err)
	if line := ciEngineStderrLine(stderr); line != "" {
		return status + ", stderr: " + line
	}
	return status
}

// ciEngineStderrLine is the first line of an engine program's stderr, bounded for the
// log. kubectl's own lines and blank lines are skipped; control and format characters,
// bidi overrides and isolates among them, become spaces; the line is cut at
// ciEngineStderrMax characters.
func ciEngineStderrLine(stderr []byte) string {
	for _, raw := range strings.Split(string(stderr), "\n") {
		if ciEngineKubectlLine(raw) {
			continue
		}
		line := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return ' '
			}
			return r
		}, raw))
		if line == "" {
			continue
		}
		if runes := []rune(line); len(runes) > ciEngineStderrMax {
			return string(runes[:ciEngineStderrMax])
		}
		return line
	}
	return ""
}

// ciEngineKubectlLine reports whether a stderr line is one kubectl printed itself.
func ciEngineKubectlLine(line string) bool {
	for _, prefix := range ciEngineKubectlErrorPrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return ciEngineKlogHeader.MatchString(line)
}

// ciEngineExec runs one program in the runner's engine container with argv as
// given: kubectl exec hands docker the words themselves, so no shell reads them.
func (d *Daemon) ciEngineExec(ctx context.Context, kubeconfig, kubeContext, pod string, argv ...string) ([]byte, []byte, error) {
	args := append([]string{"exec", "-n", ciCacheNamespace, pod, "-c", ciCacheContainer, "--"}, argv...)
	return d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext, args...)
}

// ciEngineDataRootKiB reads the data root's size in KiB through du -sk, whose
// first field is the figure. The error names the failure the way every engine exec
// does: the exit status and the first bounded stderr line, never stdout.
func (d *Daemon) ciEngineDataRootKiB(ctx context.Context, kubeconfig, kubeContext, pod string) (int64, error) {
	out, stderr, err := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, "du", "-sk", ciEngineRoot)
	if err != nil {
		return 0, fmt.Errorf("du of the data root failed (%s)", ciEngineFailure(err, stderr))
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, errors.New("du printed no figure")
	}
	kib, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || kib < 0 {
		return 0, fmt.Errorf("du printed %q, not a KiB figure", fields[0])
	}
	return kib, nil
}

// ciEnginePruneLogf logs one refusal or failure line of the engine prune, at most
// once an hour for the same kind of fault, as the cache trim's lines are held. The
// hold keys on kind, so the engine's lines do not share the cache trim's.
func (d *Daemon) ciEnginePruneLogf(kind, format string, args ...any) {
	if d.diskMaintSchedule.allowLog("ci-engine-prune "+kind, d.diskMaintenanceSeams().now(), cacheTrimRefusalLogInterval) {
		d.logger.Printf("%s", fmt.Sprintf(format, args...))
	}
}
