package daemon

// The CI runner's docker-in-docker engine prune. It bounds the Docker data root in
// the runner's dind container: images, stopped containers, build cache and
// anonymous volumes are removed on an interval (ci_engine_prune_interval), under
// the disk maintenance patrol, through the same exec into the dind container and
// with the same dry-run default as the cache trim.
//
// Each run lists, plans, then removes. The list program prints the engine as
// JSON. The planner, a pure function over that listing, decides what goes, so the
// keep rule is tested in-process. A run removes at most ci_engine_prune_max_per_kind
// containers, the ones stopped longest ago first, as many volumes, the oldest
// created first, and as many images, most layers first; the rest wait for a later
// run. A container the cap keeps also keeps the image it was created from and the
// volumes it mounts out of the plan. The remove step is one docker command per
// kind the plan names (docker rm, docker rmi -f, docker volume rm), each over that
// kind's ids as argv with no shell between the plan and docker. The listing is older
// than the remove step, and docker rmi -f removes an image a created container
// holds, so right before the image command the run re-reads the containers'
// image ids and drops the planned images a container now references.
// A re-read that fails removes no image this run (gt-vlu7sc). What each command
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
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
	"github.com/steveyegge/gastown/internal/jobledger"
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

	// ciEngineKeepStorageFlag is the builder prune flag that bounds the build
	// cache's size; the argument after it is a size such as 10240MB. The runner's
	// engine takes --reserved-space (gt-85weve: docker 29.9.3 rejects
	// --keep-storage); this constant is the only place to change the spelling.
	ciEngineKeepStorageFlag = "--reserved-space"

	// ciEngineMaxUsedSpaceFlag is the builder prune flag that caps the build
	// cache's used space: docker 29.9.3's help names it "the maximum amount of disk
	// space allowed to keep for cache". Unlike --reserved-space, which is a floor
	// the prune never goes below, it bounds the cache from above.
	ciEngineMaxUsedSpaceFlag = "--max-used-space"

	// ciEngineMegabyte is the byte count ciEngineKeepStorageSize counts a
	// megabyte in: 1 MiB, the unit the patrol's size settings are read in.
	ciEngineMegabyte = 1 << 20
)

// The outcomes an engine prune run records in the job ledger. A failed run is
// "failed"; a run that reached its verdict is "ok", except that a build cache step
// which reclaimed nothing while docker reported reclaimable cache before it records
// the empty outcome: the run did what its filter allowed, and the WARN names what is
// left (gt-np1hrk).
const (
	ciEngineOutcomeOK     = "ok"
	ciEngineOutcomeFailed = "failed"
)

// ciEngineListScript is the list program. It prints one JSON object: the data
// root's size in KiB and the three inspected lists. It changes nothing. An id CI
// deletes between the ls and the inspect makes docker exit non-zero after printing
// the objects it did inspect; the program keeps that array on stdout, hands
// docker's stderr back on its own stderr, and exits non-zero at the end, so the
// daemon decides whether the listing is usable (ciEngineListFromExec).
const ciEngineListScript = `set -euf
root=$1
containers=$(docker ps -aq --no-trunc)
images=$(docker image ls -aq --no-trunc)
volumes=$(docker volume ls -q)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failed=0
inspect_list() {
kind=$1
shift
if [ $# -eq 0 ]; then printf '[]'; return 0; fi
rc=0
docker "$kind" inspect "$@" >"$tmp/out" 2>"$tmp/err" || rc=$?
cat "$tmp/out"
if [ -s "$tmp/err" ]; then cat "$tmp/err" >&2; fi
if [ "$rc" -ne 0 ] || [ -s "$tmp/err" ]; then failed=1; fi
}
printf '{"data_root_kib":%s,"containers":' "$(du -sk "$root" | cut -f1)"
inspect_list container $containers
printf ',"images":'
inspect_list image $images
printf ',"volumes":'
inspect_list volume $volumes
printf '}\n'
exit $failed
`

// ciEngineInUseImagesScript is the read the run makes right before an image
// removal: the image id each container in the engine was created from, one per
// line. docker ps prints an image's name as the container was given it, so the ids
// come from inspecting the containers instead. It changes nothing.
const ciEngineInUseImagesScript = `set -euf
containers=$(docker ps -aq --no-trunc)
if [ -n "$containers" ]; then docker container inspect --format '{{.Image}}' $containers; fi
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

// ciEngineRemovalCounts names the ids a set of removals takes, per kind, for the
// run's log.
func ciEngineRemovalCounts(removals []ciEngineRemoval) string {
	counts := map[string]int{}
	for _, r := range removals {
		counts[r.kind] = len(r.ids)
	}
	return fmt.Sprintf("%d containers, %d images, %d volumes", counts["container"], counts["image"], counts["volume"])
}

// ciEngineShortImages names image ids for a log line, each as the digest's first 12
// characters, the prefix docker prints for a short id.
func ciEngineShortImages(ids []string) string {
	short := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimPrefix(id, "sha256:")
		if len(id) > 12 {
			id = id[:12]
		}
		short = append(short, id)
	}
	return strings.Join(short, ", ")
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
// unusedAge, keeping at most keepBytes, the least recently used records going
// first. docker's until filter takes a duration, not a time.
func ciEngineBuildCachePruneArgv(unusedAge time.Duration, keepBytes int64) []string {
	return []string{"docker", "builder", "prune", "-af", "--filter", "until=" + ciEngineUntil(unusedAge),
		ciEngineKeepStorageFlag, ciEngineKeepStorageSize(keepBytes)}
}

// ciEngineBuildCacheCapArgv is the build cache cap: the least recently used
// records go until the cache's used space is at most keepBytes. It has no until
// filter, so records younger than the age are eligible too.
func ciEngineBuildCacheCapArgv(keepBytes int64) []string {
	return []string{"docker", "builder", "prune", "-af", ciEngineMaxUsedSpaceFlag, ciEngineKeepStorageSize(keepBytes)}
}

// ciEngineKeepStorageSize spells keepBytes as the size docker's builder flags
// take: whole megabytes, 1 MiB each, when it divides evenly, otherwise the byte
// count with B. Neither spelling rounds, so the size never exceeds the limit.
func ciEngineKeepStorageSize(keepBytes int64) string {
	if keepBytes%ciEngineMegabyte == 0 {
		return strconv.FormatInt(keepBytes/ciEngineMegabyte, 10) + "MB"
	}
	return strconv.FormatInt(keepBytes, 10) + "B"
}

// ciEngineUntil spells an age as the duration docker's until filter parses: whole
// hours as "24h", otherwise whole minutes as "90m".
func ciEngineUntil(age time.Duration) string {
	if age%time.Hour == 0 {
		return strconv.FormatInt(int64(age/time.Hour), 10) + "h"
	}
	return strconv.FormatInt(int64(age/time.Minute), 10) + "m"
}

// ciEngineSizeReportFormat is the template the build cache's size is read with: one
// line per kind of storage, its size and what a prune could reclaim. It is the
// template the polecat image prune reads its sizes with.
const ciEngineSizeReportFormat = "{{.Type}} {{.Size}} {{.Reclaimable}}"

// errCIEngineTotal is the refusal for a builder prune whose output names no
// reclaimed size. The caller logs the output's last line itself, bounded.
var errCIEngineTotal = errors.New("docker builder prune printed no Total line with a size")

// errCIEngineReclaimable is the refusal for a size report whose Build Cache line
// names no reclaimable size.
var errCIEngineReclaimable = errors.New("docker system df printed no reclaimable size for the build cache")

// ciEngineBuildCacheTotal reads the bytes docker builder prune reports it reclaimed.
// The figure is on the Total line it prints: "Total: 68.8GB" from buildx, "Total
// reclaimed space: 68.8GB" from the classic builder. The last Total line is the one
// read; output with no Total line that carries a size is an error.
func ciEngineBuildCacheTotal(out []byte) (int64, error) {
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "Total") {
			continue
		}
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			return 0, errCIEngineTotal
		}
		n, err := dockerimg.ParseSize(strings.TrimSpace(line[colon+1:]))
		if err != nil {
			return 0, errCIEngineTotal
		}
		return n, nil
	}
	return 0, errCIEngineTotal
}

// ciEngineBuildCacheReport reads a build cache prune that ran without error. It
// returns the bytes the prune reclaimed and whether docker printed that figure in a
// size at all (known is false when the Total line could not be read), and the verdict
// line naming them. A total that cannot be read is named by docker's last line,
// bounded, and fails nothing.
func ciEngineBuildCacheReport(out []byte) (reclaimed int64, known bool, verdict string) {
	n, err := ciEngineBuildCacheTotal(out)
	if err != nil {
		return 0, false, fmt.Sprintf("build cache reclaimed unknown (docker's last line was %q)", ciEngineBounded(ciEngineLastLine(out)))
	}
	return n, true, "build cache reclaimed " + polecatimage.Human(n)
}

// ciEngineZeroReclaimWarn is the WARN for a run whose age prune and cap prune both
// reclaimed nothing although docker reported reclaimable cache before them, and the
// cache is still over the keep size. It names the age prune's filter and keep flag and
// the cap prune's flag, so the operator sees which passes ran and with what limits.
func ciEngineZeroReclaimWarn(reclaimable int64, unusedAge time.Duration, keepBytes int64) string {
	size := ciEngineKeepStorageSize(keepBytes)
	return fmt.Sprintf("disk_maintenance: ci engine prune: WARN: the build cache prune reclaimed %s and the cap prune reclaimed %s, though docker system df reported %s reclaimable before them; the age prune ran with --filter until=%s and %s %s, the cap prune with %s %s, so the cache stayed over the keep size",
		polecatimage.Human(0), polecatimage.Human(0), polecatimage.Human(reclaimable), ciEngineUntil(unusedAge), ciEngineKeepStorageFlag, size, ciEngineMaxUsedSpaceFlag, size)
}

// ciEngineCapReclaimed reads the cap prune's output the way ciEngineBuildCacheReport
// reads the age prune's: the bytes it reclaimed and whether docker printed them, and
// the verdict clause naming them after the age prune's. A total that cannot be read
// is named by docker's last line, bounded, and fails nothing.
func ciEngineCapReclaimed(out []byte) (n int64, known bool, text string) {
	n, err := ciEngineBuildCacheTotal(out)
	if err != nil {
		return 0, false, fmt.Sprintf("cap prune reclaimed unknown (docker's last line was %q)", ciEngineBounded(ciEngineLastLine(out)))
	}
	return n, true, "cap prune reclaimed " + polecatimage.Human(n)
}

// ciEngineLastLine is the last line of out that is not blank, or "".
func ciEngineLastLine(out []byte) string {
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
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

// ciEngineInUseImageIDs reads the in-use read's output: the image id of every
// container in the engine, one per line. A line that is not an image id means the
// read cannot be trusted, so it is an error rather than an empty set: an unknown
// in-use set removes no image.
func ciEngineInUseImageIDs(out []byte) (map[string]bool, error) {
	inUse := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !ciEngineImageIDRE.MatchString(line) {
			return nil, errors.New("the in-use read printed a line that is not an image id")
		}
		inUse[line] = true
	}
	return inUse, nil
}

// ciEngineDropInUseImages takes the planned images a container now references out
// of an image removal, returning the remaining removal and the ids it dropped. A
// removal left with no ids is not sent.
func ciEngineDropInUseImages(r ciEngineRemoval, inUse map[string]bool) (ciEngineRemoval, []string) {
	var ids, dropped []string
	for _, id := range r.ids {
		if inUse[id] {
			dropped = append(dropped, id)
			continue
		}
		ids = append(ids, id)
	}
	if len(dropped) == 0 {
		return r, nil
	}
	return ciEngineRemoval{kind: r.kind, argv: append([]string{"docker", "rmi", "-f"}, ids...), ids: ids}, dropped
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
	// ciEngineTaskNameRE is a Forgejo Actions job container's name: docker's leading
	// slash, FORGEJO-ACTIONS-TASK-, the task id, then the runner's suffix.
	ciEngineTaskNameRE = regexp.MustCompile(`^/FORGEJO-ACTIONS-TASK-([0-9]+)_`)
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
	// Name is the container's name as docker prints it, with its leading slash.
	Name  string `json:"Name"`
	State struct {
		Status string `json:"Status"`
		// StartedAt is when the container's process started, as docker prints it.
		StartedAt string `json:"StartedAt"`
		// FinishedAt is when the container last stopped. docker prints the zero
		// time for one that is not stopped, a container in "created" state that
		// never ran included.
		FinishedAt string `json:"FinishedAt"`
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

// ciEngineRemaining is the count of each kind a capped plan left for a later
// run: the candidates the cap kept out of this run's plan.
type ciEngineRemaining struct {
	containers, images, volumes int
}

// ciEngineStopKey is one planned container's place in the cap's order: the
// instant it stopped, which is what the cap cuts by, and whether that instant
// reads at all.
type ciEngineStopKey struct {
	container ciEngineContainer
	stoppedAt time.Time
	known     bool
}

// ciEngineVolumeKey is one planned volume's place in the cap's order: the
// instant it was created, which is what the cap cuts by, and whether that
// instant reads at all.
type ciEngineVolumeKey struct {
	volume  ciEngineVolume
	created time.Time
	known   bool
}

// capCIEnginePlan returns a copy of plan with its containers, images and volumes
// cut to max, and the count of each kind left for a later run. The plan is in id
// order today and its volumes in name order, so the copy's containers are sorted
// by their stop time (ciEngineStoppedAt) and its volumes by their creation time
// before the cut. Its images are cut as they are, most layers first: the planner
// already orders them so, which puts a child before its parent, and the cut keeps
// that order. A kind with no more candidates than max is returned as given, with
// nothing remaining.
//
// Every container and volume in the plan came through planCIEnginePrune, which
// refused a listing whose time does not read, so every stop and creation time
// here is known. One that does not read anyway is ordered last, so it is kept and
// counted as remaining, never removed ahead of one whose time is known. The input
// plan is not modified.
func capCIEnginePlan(plan ciEnginePlan, max int) (ciEnginePlan, ciEngineRemaining) {
	if max < 0 {
		max = 0
	}
	out := ciEnginePlan{
		containers: slices.Clone(plan.containers),
		images:     slices.Clone(plan.images),
		volumes:    slices.Clone(plan.volumes),
	}
	var remaining ciEngineRemaining
	if max < len(out.containers) {
		keyed := make([]ciEngineStopKey, len(out.containers))
		for i, c := range out.containers {
			keyed[i] = ciEngineStopKey{container: c}
			keyed[i].stoppedAt, keyed[i].known = ciEngineStopKeyOf(c)
		}
		slices.SortStableFunc(keyed, func(a, b ciEngineStopKey) int {
			switch {
			case a.known && b.known:
				if d := a.stoppedAt.Compare(b.stoppedAt); d != 0 {
					return d
				}
			case a.known:
				return -1
			case b.known:
				return 1
			}
			return strings.Compare(a.container.ID, b.container.ID)
		})
		for i := range max {
			out.containers[i] = keyed[i].container
		}
		remaining.containers = len(keyed) - max
		out.containers = out.containers[:max]
	}
	if max < len(out.images) {
		remaining.images = len(out.images) - max
		out.images = out.images[:max]
	}
	if max < len(out.volumes) {
		keyed := make([]ciEngineVolumeKey, len(out.volumes))
		for i, v := range out.volumes {
			keyed[i] = ciEngineVolumeKey{volume: v}
			keyed[i].created, keyed[i].known = ciEngineVolumeKeyOf(v)
		}
		slices.SortStableFunc(keyed, func(a, b ciEngineVolumeKey) int {
			switch {
			case a.known && b.known:
				if d := a.created.Compare(b.created); d != 0 {
					return d
				}
			case a.known:
				return -1
			case b.known:
				return 1
			}
			return strings.Compare(a.volume.Name, b.volume.Name)
		})
		for i := range max {
			out.volumes[i] = keyed[i].volume
		}
		remaining.volumes = len(keyed) - max
		out.volumes = out.volumes[:max]
	}
	return out, remaining
}

// ciEngineCappedPlan is capCIEnginePlan with the plan narrowed to what the
// containers it removes still free. The planner decides what is in use from the
// containers it plans to remove, so the image a planned container was created
// from and the volumes it mounts are candidates. A container the cap keeps stays
// in the engine, and its image and volumes stay with it: those candidates come
// out of the capped plan, or the run would free an image and a volume that a
// container still there needs. A plan the cap does not cut is returned as
// capCIEnginePlan returns it.
func ciEngineCappedPlan(plan ciEnginePlan, max int) (ciEnginePlan, ciEngineRemaining) {
	capped, remaining := capCIEnginePlan(plan, max)
	if len(capped.containers) == len(plan.containers) {
		return capped, remaining
	}
	removed := make(map[string]bool, len(capped.containers))
	for _, c := range capped.containers {
		removed[c.ID] = true
	}
	usedImage := map[string]bool{}
	usedVolume := map[string]bool{}
	for _, c := range plan.containers {
		if removed[c.ID] {
			continue
		}
		usedImage[c.Image] = true
		for _, m := range c.Mounts {
			if m.Type == "volume" {
				usedVolume[m.Name] = true
			}
		}
	}
	capped.images = slices.DeleteFunc(capped.images, func(img ciEngineImage) bool { return usedImage[img.ID] })
	capped.volumes = slices.DeleteFunc(capped.volumes, func(v ciEngineVolume) bool { return usedVolume[v.Name] })
	return capped, remaining
}

// ciEngineStopKeyOf reads a planned container's stop time, the key the container
// cap orders by. known is false when the plan's creation or finish time does not
// read; the cap then orders the container last, so it is kept. The plan the
// daemon builds never reaches that case: planCIEnginePrune refuses such a
// listing.
func ciEngineStopKeyOf(c ciEngineContainer) (time.Time, bool) {
	created, err := parseCIEngineTime(c.Created)
	if err != nil {
		return time.Time{}, false
	}
	stoppedAt, err := ciEngineStoppedAt(c, created)
	if err != nil {
		return time.Time{}, false
	}
	return stoppedAt, true
}

// ciEngineVolumeKeyOf reads a planned volume's creation time, the key the volume
// cap orders by. known is false when the time does not read; the cap then orders
// the volume last, so it is kept. The plan the daemon builds never reaches that
// case: planCIEnginePrune refuses such a listing.
func ciEngineVolumeKeyOf(v ciEngineVolume) (time.Time, bool) {
	created, err := parseCIEngineTime(v.CreatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return created, true
}

// planCIEnginePrune decides what the engine prune removes from a listing taken at
// now, under the keep rule in docs/guides/disk-maintenance.md. imageAge is how old
// an unused image must be, and unusedAge how long a stopped container has been
// stopped, and how old an unused anonymous volume and an unused build cache record
// must be. A listing it cannot read in full is refused: an id or name that is not
// in docker's shape, a time it cannot parse, or a missing data root figure. A
// refusal is a failed run, never an empty plan.
func planCIEnginePrune(l ciEngineListing, now time.Time, imageAge, unusedAge time.Duration, keepRepos []string) (ciEnginePlan, error) {
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
		if ciEngineStopped(c.State.Status) {
			stoppedAt, err := ciEngineStoppedAt(c, created)
			if err != nil {
				return plan, fmt.Errorf("container %s: %w", c.ID[:12], err)
			}
			if now.Sub(stoppedAt) > unusedAge {
				plan.containers = append(plan.containers, c)
				continue
			}
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
			if !baseKeep[id] && now.Sub(created[id]) > imageAge {
				removable[id] = true
			}
			continue
		}
		if now.Sub(created[id]) > imageAge {
			removable[id] = true
		}
	}

	// An unused gastown-* image past the newest five of its repository goes whatever its
	// age. An image tagged ci-base stays with the ci-base rule above, so its rollback and
	// its age still decide it.
	for id := range ciEngineBuiltExcess(images, created, usedImage, protected) {
		if !slices.Contains(ciEngineRepoTags(images[id].RepoTags), ciEngineBaseRepo) {
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
		if now.Sub(t) > unusedAge {
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

// ciEngineStoppedAt is the instant a stopped container stopped, which is what the
// unused age is measured from: a job that ran longer than the age and stopped a
// minute ago is not unused. docker's State.FinishedAt names that instant. A
// container in "created" state never ran, so docker gives it the zero time there
// and its creation is the instant it became idle. An unreadable finish time, or
// one before the creation, is refused rather than read as creation: it could age a
// container a job still holds.
func ciEngineStoppedAt(c ciEngineContainer, created time.Time) (time.Time, error) {
	finished, err := time.Parse(time.RFC3339Nano, c.State.FinishedAt)
	if err != nil {
		return time.Time{}, errors.New("a container's finish time is not RFC 3339")
	}
	if finished.IsZero() {
		return created, nil
	}
	if finished.Before(created) {
		return time.Time{}, errors.New("a container's finish time precedes its creation")
	}
	return finished, nil
}

// ciEngineJobContainer is a running Actions job container older than the age, with
// the task id its name carries; whether that task still runs is a later step's.
type ciEngineJobContainer struct {
	Container ciEngineContainer
	TaskID    int64
}

// ciEngineOrphanCandidates returns, in the listing's order, each running Actions
// job container whose process started more than age before now, with the task id
// its name carries. A started time docker's format cannot read, or a task id too
// large for an int64, is an error naming the container: a running job is never
// skipped as if it were not one.
func ciEngineOrphanCandidates(cs []ciEngineContainer, now time.Time, age time.Duration) ([]ciEngineJobContainer, error) {
	var out []ciEngineJobContainer
	for _, c := range cs {
		if c.State.Status != "running" {
			continue
		}
		m := ciEngineTaskNameRE.FindStringSubmatch(c.Name)
		if m == nil {
			continue
		}
		taskID, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("container %s: its task id does not fit an int64", c.Name)
		}
		started, err := time.Parse(time.RFC3339Nano, c.State.StartedAt)
		if err != nil {
			return nil, fmt.Errorf("container %s: its start time is not RFC 3339", c.Name)
		}
		if now.Sub(started) <= age {
			continue
		}
		out = append(out, ciEngineJobContainer{Container: c, TaskID: taskID})
	}
	return out, nil
}

// ciEngineSplitOrphans splits candidate job containers, already past the age, by
// whether the set holds their task as running: a task that no longer runs is what
// makes an orphan safe to remove, and a candidate whose task still runs is kept so
// the verdict can name why a container past the age stayed.
func ciEngineSplitOrphans(cands []ciEngineJobContainer, running map[int64]bool) (orphans, keptRunning []ciEngineContainer) {
	for _, cand := range cands {
		if running[cand.TaskID] {
			keptRunning = append(keptRunning, cand.Container)
			continue
		}
		orphans = append(orphans, cand.Container)
	}
	return orphans, keptRunning
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

// ciEnginePruneSettings reads the engine prune's six settings from the patrol's
// block, returning the default for each that is empty. A value the patrol cannot
// act on is an error naming its key; maxPerKind is the cap on candidates of each
// kind one run may remove.
func ciEnginePruneSettings(cfg *DaemonPatrolConfig) (every, imageAge, unusedAge time.Duration, keepBytes int64, repoList []string, maxPerKind int, err error) { //nolint:unparam // inert since the cap is applied; kept so gt done's revert guard does not read the line's removal as undoing 5174bc80
	var settings agentconfig.DiskMaintenanceConfig
	if c := diskMaintenanceSettings(cfg); c != nil {
		settings = *c
	}
	if every, err = settings.CIEnginePruneEvery(); err != nil {
		return 0, 0, 0, 0, nil, 0, err
	}
	if imageAge, err = settings.CIEnginePruneAgeDuration(); err != nil {
		return 0, 0, 0, 0, nil, 0, err
	}
	if unusedAge, err = settings.CIEngineUnusedAgeDuration(); err != nil {
		return 0, 0, 0, 0, nil, 0, err
	}
	if keepBytes, err = settings.CIEngineCacheKeepLimit(); err != nil {
		return 0, 0, 0, 0, nil, 0, err
	}
	if repoList, err = settings.CIEngineKeepRepoList(); err != nil {
		return 0, 0, 0, 0, nil, 0, err
	}
	if maxPerKind, err = settings.CIEnginePruneCap(); err != nil {
		return 0, 0, 0, 0, nil, 0, err
	}
	return every, imageAge, unusedAge, keepBytes, repoList, maxPerKind, nil
}

// runCIEnginePruneTick is the engine prune's own tick. It runs before the quiet
// guard and on its own interval, as the CI cache trim does. Its single-flight
// guard keeps one pass in flight, and the pass re-reads the due check when it
// starts.
func (d *Daemon) runCIEnginePruneTick() {
	pc := d.patrol()
	s := d.diskMaintenanceSeams()
	now := s.now()
	_, configured, err := ciCacheTrimContext(pc)
	if !configured {
		return
	}
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	every, _, _, _, _, _, err := ciEnginePruneSettings(pc)
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.diskMaintSchedule.trimDue(ciEnginePruneName, now, every) {
		return
	}
	mode, err := diskMaintenanceMode(pc)
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
	every, _, _, _, _, _, err := ciEnginePruneSettings(d.patrol())
	if err != nil {
		d.refuseDiskMaintenance(err)
		return
	}
	if !d.diskMaintSchedule.trimDue(ciEnginePruneName, s.now(), every) {
		return
	}
	verdict, reason := d.ciEnginePrune(ctx, mode)
	concluded := s.now()
	d.diskMaintConclude(ctx, ciEnginePruneName, "CI engine prune", verdict, concluded, every, reason)
}

// ciEngineReasonLine is the failure reason the attention item carries: the first
// line of the error or sentence the run logs.
func ciEngineReasonLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

// ciEnginePrune runs the engine prune once and reports whether it reached a
// verdict, and why when it did not: the first line of the error or the refusal
// sentence the run logs, which the attention item carries. Every run, whatever
// path it takes, is appended to the job ledger: the plan's counts, the bytes the
// build cache steps reclaimed (the cap prune's too, when it ran) and how the run
// ended, with the next due at one interval from its end. A run that cannot list the engine or plan from the
// listing removes nothing and says why. Right before the image command it re-reads
// what the containers use and drops the planned images one now references; a
// re-read that fails removes no image and fails the run after the other kinds have
// run. Once it removes, every kind and the build cache prune run whatever an
// earlier one did; a kind docker left in place, or that could not be checked, fails
// the run after the rest have run, and the next run plans again from what is left.
// The run removes at most maxPerKind containers, the ones stopped longest ago
// first, as many volumes, the oldest created first, and as many images, most layers
// first; a container it keeps also keeps its image and volumes out of the plan. It
// names the rest in its log, and a run that leaves some is still a verdict.
func (d *Daemon) ciEnginePrune(ctx context.Context, mode string) (verdict bool, reason string) {
	pc := d.patrol()
	s := d.diskMaintenanceSeams()
	started := s.now()
	every, imageAge, unusedAge, keepBytes, keepRepos, maxPerKind, err := ciEnginePruneSettings(pc)
	stats := map[string]int64{}
	outcome := ciEngineOutcomeFailed
	// The run is recorded on every return path below, the refusals included, so
	// the ledger says when the job last ran and when it is next due.
	defer func() {
		ended := s.now()
		run := jobledger.Run{
			Job:       ciEnginePruneName,
			StartedAt: started,
			EndedAt:   ended,
			Outcome:   outcome,
			Stats:     stats,
		}
		// A refused setting leaves the interval unknown, so no next due is
		// recorded rather than a guessed one.
		if every > 0 {
			next := ended.Add(every)
			run.NextDue = &next
		}
		d.recordCIEnginePruneRun(run)
	}()
	if err != nil {
		d.ciEnginePruneLogf("settings", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false, ciEngineReasonLine(err.Error())
	}
	kubeContext, _, err := ciCacheTrimContext(pc)
	if err != nil {
		d.ciEnginePruneLogf("context", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false, ciEngineReasonLine(err.Error())
	}
	kubeconfig := diskMaintKubeconfig(d.config.TownRoot, s.getenv)
	pod, err := d.ciCacheTrimPod(ctx, kubeconfig, kubeContext)
	if err != nil {
		// The lookup's error can carry kubectl's stderr, so only the sentinel's text
		// or the exit status is logged and carried.
		if errors.Is(err, errCICacheTrimNotRunning) {
			d.ciEnginePruneLogf("pod not running", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
			return false, ciEngineReasonLine(err.Error())
		}
		d.ciEnginePruneLogf("pod lookup", "disk_maintenance: ci engine prune: the runner's pod lookup failed (%s); nothing pruned", ciEngineExitStatus(err))
		return false, fmt.Sprintf("the runner's pod lookup failed (%s)", ciEngineExitStatus(err))
	}

	// A failed exec's log line names its exit status and the first line of its
	// stderr, bounded; ciEngineFailure never lets stdout or a second line through.
	stdout, stderr, err := d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext,
		"exec", "-n", ciCacheNamespace, pod, "-c", ciCacheContainer, "--",
		"sh", "-c", ciEngineListScript, "ci-engine-list", ciEngineRoot)
	listing, err := ciEngineListFromExec(stdout, stderr, err)
	if err != nil {
		d.ciEnginePruneLogf("listing", "disk_maintenance: ci engine prune: %v; nothing pruned", err)
		return false, ciEngineReasonLine(err.Error())
	}
	now := s.now()
	plan, err := planCIEnginePrune(listing, now, imageAge, unusedAge, keepRepos)
	if err != nil {
		d.ciEnginePruneLogf("plan refused", "disk_maintenance: ci engine prune: the listing could not be planned: %v; nothing pruned", err)
		return false, ciEngineReasonLine(fmt.Sprintf("the listing could not be planned: %v", err))
	}

	before := polecatimage.Human(*listing.DataRootKiB * 1024)
	// The ledger's stats keep the totals the plan found, as they did before the cap
	// moved ahead of the dry run's exit; the run's lines count what the cap leaves.
	stats["containers"] = int64(len(plan.containers))
	stats["images"] = int64(len(plan.images))
	stats["volumes"] = int64(len(plan.volumes))
	// The per-run cap keeps the containers stopped longest ago, the volumes
	// created longest ago and the plan's first images, most layers first; the
	// rest wait for a later run, and a run that leaves some is a verdict, not a
	// failure. A container the cap keeps stays in the engine, so the images and
	// volumes it still references come out of the plan with it. A dry run reports
	// this capped plan, so it names only what an apply would remove now.
	capped, remaining := ciEngineCappedPlan(plan, maxPerKind)
	plan = capped
	if remaining.containers > 0 || remaining.images > 0 || remaining.volumes > 0 {
		d.logger.Printf("disk_maintenance: ci engine prune: %d container(s), %d image(s), %d volume(s) remain for a later run, over ci_engine_prune_max_per_kind %d",
			remaining.containers, remaining.images, remaining.volumes, maxPerKind)
	}
	counts := ciEngineRemovalCounts(ciEngineRemovals(plan))
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
		// The build cache step did not run, so no build_cache_bytes is recorded.
		outcome = ciEngineOutcomeOK
		return true, ""
	}

	// Every kind and the build cache prune run whatever an earlier one did; each
	// failure is logged on its own line and the run fails once they have all run,
	// on the first failure's sentence.
	failed := false
	removals := ciEngineRemovals(plan)
	for i, r := range removals {
		if r.kind == "image" {
			// The listing is older than this exec, and docker rmi -f removes an image
			// a created container holds: re-read what the containers use and drop the
			// planned images one now references. A re-read that fails sends no image
			// command and fails the run, once logged.
			kept, dropped, readErr := d.ciEngineImagesSkipInUse(ctx, kubeconfig, kubeContext, pod, r)
			if readErr != nil {
				d.ciEnginePruneLogf("in-use read", "disk_maintenance: ci engine prune: %v; no image is removed this run", readErr)
				removals[i] = ciEngineRemoval{kind: r.kind}
				failed = true
				if reason == "" {
					reason = ciEngineReasonLine(readErr.Error())
				}
				continue
			}
			if len(dropped) > 0 {
				d.logger.Printf("disk_maintenance: ci engine prune: skipped %d planned image(s) a container now references: %s", len(dropped), ciEngineShortImages(dropped))
			}
			removals[i] = kept
			if len(kept.ids) == 0 {
				continue // every planned image is now in use: no image command is sent
			}
			r = kept
		}
		ok, why := d.ciEngineRemoveKind(ctx, kubeconfig, kubeContext, pod, r)
		if !ok {
			failed = true
			if reason == "" {
				reason = why
			}
		}
	}
	// The verdict line names what the image re-read left to remove, not what the
	// plan named before it.
	counts = ciEngineRemovalCounts(removals)
	// The build cache is sized once, before its prune, through the same exec: the
	// prune's total is read against what docker said was reclaimable. A failed size
	// read skips that check and nothing else.
	reclaimable, reclaimErr := d.ciEngineBuildCacheReclaimable(ctx, kubeconfig, kubeContext, pod)
	if reclaimErr != nil {
		d.ciEnginePruneLogf("reclaimable unreadable", "disk_maintenance: ci engine prune: the build cache's reclaimable size could not be read before its prune: %v; the zero-reclaim check is skipped", reclaimErr)
	}
	cache := "build cache prune failed"
	zeroReclaim := false
	var reclaimed int64
	reclaimedKnown := false
	if pruned, stderr, err := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, ciEngineBuildCachePruneArgv(unusedAge, keepBytes)...); err != nil {
		d.ciEnginePruneLogf("build cache prune failed", "disk_maintenance: ci engine prune: the build cache prune in the %s container failed (%s); the run is retried",
			ciCacheContainer, ciEngineFailure(err, stderr))
		failed = true
		if reason == "" {
			reason = fmt.Sprintf("the build cache prune in the %s container failed (%s)", ciCacheContainer, ciEngineFailure(err, stderr))
		}
	} else {
		var verdict string
		reclaimed, reclaimedKnown, verdict = ciEngineBuildCacheReport(pruned)
		cache = verdict
		if reclaimedKnown {
			stats["build_cache_bytes"] = reclaimed
		}
	}
	// The cap prune follows the age prune when the cache is still over the keep size:
	// the reclaimable figure docker gave before the age prune, less what the age prune
	// reclaimed. An unread figure, or an age prune that failed, leaves the excess
	// unknown, so no cap prune is sent.
	if over, usable := ciEngineCacheOverKeep(reclaimable, reclaimErr == nil, reclaimed, reclaimedKnown, keepBytes); usable && over > 0 {
		if capOut, capStderr, capErr := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, ciEngineBuildCacheCapArgv(keepBytes)...); capErr != nil {
			d.ciEnginePruneLogf("cap prune failed", "disk_maintenance: ci engine prune: the cap prune in the %s container failed (%s); the run is retried",
				ciCacheContainer, ciEngineFailure(capErr, capStderr))
			failed = true
			if reason == "" {
				reason = fmt.Sprintf("the cap prune in the %s container failed (%s)", ciCacheContainer, ciEngineFailure(capErr, capStderr))
			}
		} else {
			capReclaimed, capKnown, capText := ciEngineCapReclaimed(capOut)
			cache += ", " + capText
			if capKnown {
				stats["build_cache_bytes"] += capReclaimed
			}
			// The zero-reclaim WARN is raised only when the cap prune also reclaimed
			// nothing: the age prune alone reclaiming 0 is not enough while the cap
			// prune could still have removed cache over the keep size.
			if capKnown && capReclaimed == 0 && reclaimedKnown && reclaimed == 0 {
				zeroReclaim = true
				d.logger.Printf("%s", ciEngineZeroReclaimWarn(reclaimable, unusedAge, keepBytes))
			}
		}
	}
	afterKiB, err := d.ciEngineDataRootKiB(ctx, kubeconfig, kubeContext, pod)
	if err != nil {
		d.ciEnginePruneLogf("size unreadable", "disk_maintenance: ci engine prune: the data root's size after could not be read: %v; the run is retried", err)
		return false, ciEngineReasonLine(fmt.Sprintf("the data root's size after could not be read: %v", err))
	}
	if failed {
		d.logger.Printf("disk_maintenance: ci engine prune: a removal failed; the engine is %s, from %s; %s; the run is retried", polecatimage.Human(afterKiB*1024), before, cache)
		return false, reason
	}
	outcome = ciEngineOutcomeOK
	if zeroReclaim {
		// The empty outcome tells the job list this run succeeded but had nothing
		// it was allowed to remove from a cache docker called reclaimable.
		outcome = ""
	}
	d.logger.Printf("disk_maintenance: ci engine prune: removed %s, %s to %s; %s",
		counts, before, polecatimage.Human(afterKiB*1024), cache)
	return true, ""
}

// recordCIEnginePruneRun appends one run to the town's job ledger. A ledger that
// cannot be opened or appended is logged once and never changes the prune's own
// result: the ledger is a record of what the job did, not part of doing it.
func (d *Daemon) recordCIEnginePruneRun(run jobledger.Run) {
	ledger, err := d.diskMaintenanceSeams().jobLedger()
	if err != nil {
		d.ciEnginePruneLogf("job ledger", "disk_maintenance: ci engine prune: the job ledger could not be opened (%v); the run is not recorded", err)
		return
	}
	if err := ledger.Append(run); err != nil {
		d.ciEnginePruneLogf("job ledger", "disk_maintenance: ci engine prune: the run could not be recorded (%v)", err)
	}
}

// ciEngineImagesSkipInUse re-reads the containers' image ids right before an image
// removal and takes the planned images a container now references out of it, since
// docker rmi -f removes an image a created, not-yet-started container holds. A
// re-read that cannot run, or whose output is not image ids, returns an error and
// no removal: the caller fails the run and sends no image command. The ids dropped
// are returned for the log.
func (d *Daemon) ciEngineImagesSkipInUse(ctx context.Context, kubeconfig, kubeContext, pod string, r ciEngineRemoval) (ciEngineRemoval, []string, error) {
	out, stderr, err := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, "sh", "-c", ciEngineInUseImagesScript, "ci-engine-in-use")
	if err != nil {
		return ciEngineRemoval{}, nil, fmt.Errorf("the in-use read failed (%s)", ciEngineFailure(err, stderr))
	}
	inUse, err := ciEngineInUseImageIDs(out)
	if err != nil {
		return ciEngineRemoval{}, nil, err
	}
	kept, dropped := ciEngineDropInUseImages(r, inUse)
	return kept, dropped, nil
}

// ciEngineRemoveKind runs one kind's removal and reports whether every planned id
// is gone from the engine afterwards. The command's exit status is not the
// answer: docker rmi -f exits 1 over an id an earlier argument already freed. The
// answer is docker's own report, then one presence inspect over the ids it did
// not print. A failure is logged here, naming the kind, the exit status and the
// first line of docker's stderr, and is returned as the item's reason.
func (d *Daemon) ciEngineRemoveKind(ctx context.Context, kubeconfig, kubeContext, pod string, r ciEngineRemoval) (bool, string) {
	stdout, stderr, runErr := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, r.argv...)
	missing := ciEngineMissing(r, stdout)
	if len(missing) == 0 {
		return true, ""
	}
	// The inspect exits 1 whenever one of the ids is gone, so only a failure to run
	// it at all, not a status, stops the answer.
	seen, seenErr, inspectErr := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, r.presenceArgv(missing)...)
	if !ciEngineRan(inspectErr) {
		d.ciEnginePruneLogf("presence "+r.kind, "disk_maintenance: ci engine prune: the %s presence check could not run (%s); the run is retried", r.kind, ciEngineFailure(inspectErr, seenErr))
		return false, fmt.Sprintf("the %s presence check could not run (%s)", r.kind, ciEngineFailure(inspectErr, seenErr))
	}
	present := ciEnginePresent(r.kind, seen)
	left := 0
	for _, id := range missing {
		if present[id] {
			left++
		}
	}
	if left == 0 {
		return true, ""
	}
	d.ciEnginePruneLogf("not removed "+r.kind, "disk_maintenance: ci engine prune: docker left %d of %d planned %s(s) in the %s container (the removal ended: %s); the run is retried",
		left, len(r.ids), r.kind, ciCacheContainer, ciEngineFailure(runErr, stderr))
	return false, fmt.Sprintf("docker left %d of %d planned %s(s) in the %s container (the removal ended: %s)",
		left, len(r.ids), r.kind, ciCacheContainer, ciEngineFailure(runErr, stderr))
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
// line. The lines klog writes for kubectl are matched by ciEngineKlogHeader instead,
// and its report of the remote command's status by ciEngineKubectlExitReport.
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

// ciEngineKubectlExitReport matches the line kubectl writes to stderr when the remote
// command exits non-zero: kubectl's own report of the status, the decimal number ending
// the line. The number is the status the exec ended with, and kubectl exits with it, so
// only that number makes the line kubectl's; another program can print the sentence too.
var ciEngineKubectlExitReport = regexp.MustCompile(`^command terminated with exit code (\d+)$`)

// ciEngineFailure names how an engine exec ended for the log: its exit status, then,
// for a failed exec, the first line of its stderr. A nil error is exit status 0 with
// no stderr, since a command that succeeded has nothing to say about a failure.
func ciEngineFailure(err error, stderr []byte) string {
	if err == nil {
		return "exit status 0"
	}
	status := ciEngineExitStatus(err)
	if line := ciEngineStderrLine(err, stderr); line != "" {
		return status + ", stderr: " + line
	}
	return status
}

// ciEngineStderrLine is the first line of an engine program's stderr, bounded for the
// log. kubectl's own lines, decided with err as ciEngineKubectlLine decides them, and
// blank lines are skipped; control and format characters, bidi overrides and isolates
// among them, become spaces; the line is cut at ciEngineStderrMax characters.
func ciEngineStderrLine(err error, stderr []byte) string {
	for _, raw := range strings.Split(string(stderr), "\n") {
		if ciEngineKubectlLine(err, raw) {
			continue
		}
		if line := ciEngineBounded(raw); line != "" {
			return line
		}
	}
	return ""
}

// ciEngineBounded makes one line of an engine program's output safe for the log:
// control and format characters, bidi overrides and isolates among them, become
// spaces; the line is trimmed and cut at ciEngineStderrMax characters.
func ciEngineBounded(raw string) string {
	line := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, raw))
	if runes := []rune(line); len(runes) > ciEngineStderrMax {
		return string(runes[:ciEngineStderrMax])
	}
	return line
}

// ciEngineKubectlLine reports whether a stderr line is one kubectl printed itself. The
// exec's error decides the exit report: a line naming the status err reports is
// kubectl's, and one naming any other number, or reached with no status at all, is
// another program's own line.
func ciEngineKubectlLine(err error, line string) bool {
	for _, prefix := range ciEngineKubectlErrorPrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return ciEngineKlogHeader.MatchString(line) || ciEngineKubectlExitReportLine(err, line)
}

// ciEngineKubectlExitReportLine reports whether line is kubectl's report of an exec that
// ended with the status err carries.
func ciEngineKubectlExitReportLine(err error, line string) bool {
	m := ciEngineKubectlExitReport.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	code, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return false
	}
	var status interface{ ExitCode() int }
	return errors.As(err, &status) && status.ExitCode() == code
}

// ciEngineExec runs one program in the runner's engine container with argv as
// given: kubectl exec hands docker the words themselves, so no shell reads them.
func (d *Daemon) ciEngineExec(ctx context.Context, kubeconfig, kubeContext, pod string, argv ...string) ([]byte, []byte, error) {
	args := append([]string{"exec", "-n", ciCacheNamespace, pod, "-c", ciCacheContainer, "--"}, argv...)
	return d.ciCacheTrimKubectlRun(ctx, kubeconfig, kubeContext, args...)
}

// ciEngineDataRootKiB reads the data root's size in KiB through du -sk, whose
// first field is the figure. A CI job's volume can be removed while du walks the
// root, and du then prints the total for everything it did read and exits 1; such
// a run still gives its figure, which the caller would otherwise lose. Any other
// non-zero exit is a failure, and so is a vanished-file run with no figure. The
// error names the failure the way every engine exec does: the exit status and the
// first bounded stderr line, never stdout.
func (d *Daemon) ciEngineDataRootKiB(ctx context.Context, kubeconfig, kubeContext, pod string) (int64, error) {
	out, stderr, err := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, "du", "-sk", ciEngineRoot)
	failed := func() error { return fmt.Errorf("du of the data root failed (%s)", ciEngineFailure(err, stderr)) }
	if err != nil && !ciEngineDuVanished(err, stderr) {
		return 0, failed()
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		if err != nil {
			return 0, failed()
		}
		return 0, errors.New("du printed no figure")
	}
	kib, parseErr := strconv.ParseInt(fields[0], 10, 64)
	if parseErr != nil || kib < 0 {
		if err != nil {
			return 0, failed()
		}
		return 0, fmt.Errorf("du printed %q, not a KiB figure", fields[0])
	}
	return kib, nil
}

// ciEngineBuildCacheReclaimable reads the build cache's reclaimable size from one
// docker system df, made before the prune runs. On the template's Build Cache line
// the size is the first field after the kind and the reclaimable size the second,
// which docker may follow with a percentage. An error names the exit status and the
// bounded first stderr line, never stdout.
func (d *Daemon) ciEngineBuildCacheReclaimable(ctx context.Context, kubeconfig, kubeContext, pod string) (int64, error) {
	out, stderr, err := d.ciEngineExec(ctx, kubeconfig, kubeContext, pod, "docker", "system", "df", "--format", ciEngineSizeReportFormat)
	if err != nil {
		return 0, fmt.Errorf("docker system df failed (%s)", ciEngineFailure(err, stderr))
	}
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Build Cache ")
		if !ok {
			continue
		}
		if fields := strings.Fields(rest); len(fields) >= 2 {
			if n, parseErr := dockerimg.ParseSize(fields[1]); parseErr == nil {
				return n, nil
			}
		}
		return 0, errCIEngineReclaimable
	}
	return 0, errCIEngineReclaimable
}

// ciEngineDuVanished reports whether du's stderr is only the report of files that
// vanished while it walked the data root: at least one such line, and no line of
// any other kind. A report is du's own diagnosis, so it carries du's program prefix
// and ends in the errno text; another program's line ending the same way is not one,
// and its exit status stays a failure. kubectl's own lines are skipped, as
// ciEngineFailure skips them, and err decides which lines those are. An empty stderr is
// not this case, so a bare non-zero status with no diagnosis stays a failure.
func ciEngineDuVanished(err error, stderr []byte) bool {
	lines := 0
	for _, raw := range strings.Split(string(stderr), "\n") {
		if ciEngineKubectlLine(err, raw) {
			continue
		}
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !ciEngineDuVanishedLine(line) {
			return false
		}
		lines++
	}
	return lines > 0
}

// ciEngineDuVanishedLine reports whether one stderr line is du's report of a file that
// vanished while it walked: du's program prefix and the errno text.
func ciEngineDuVanishedLine(line string) bool {
	const (
		duPrefix = "du: "
		vanished = ": No such file or directory"
	)
	return strings.HasPrefix(line, duPrefix) && strings.HasSuffix(line, vanished)
}

// ciEngineGoneLine is docker's report that an object it was asked to inspect is
// gone: the line it prints for one id that CI removed between the list and the
// inspect. The forms are the older `Error: No such <kind>: <id>` and docker 29.9.3's
// `Error response from daemon: No such <container|image>: <id>` and
// `Error response from daemon: get <name>: no such volume`. The message names the id.
var ciEngineGoneLine = regexp.MustCompile(`(?i)^(?:Error: No such (?:container|image|volume): \S|Error response from daemon: No such (?:container|image): \S|Error response from daemon: get \S+: no such volume$)`)

// ciEngineStderrOnlyVanished reports whether a failed list program's stderr is only
// reports of objects or files that vanished between the list and the read: at least
// one such line, and no line of any other kind. A docker line must carry docker's
// missing-object wording (ciEngineGoneLine) and a du line must be du's vanished-file
// report (ciEngineDuVanishedLine); any other line is a fault, and a bare non-zero
// status with no report stays a failure. kubectl's own lines are skipped, as
// ciEngineFailure and ciEngineDuVanished skip them, since kubectl relays the
// program's stderr on the same stream and reports the status on its own line; err
// decides which lines are kubectl's. An empty stderr is not this case.
func ciEngineStderrOnlyVanished(err error, stderr []byte) bool {
	lines := 0
	for _, raw := range strings.Split(string(stderr), "\n") {
		if ciEngineKubectlLine(err, raw) {
			continue
		}
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !ciEngineGoneLine.MatchString(line) && !ciEngineDuVanishedLine(line) {
			return false
		}
		lines++
	}
	return lines > 0
}

// ciEngineListFromExec reads the list program's output. A run that exited zero
// must print a listing. A run that exited non-zero still gives its listing when
// every stderr line is docker's missing-object report or du's vanished-file report
// and the arrays it did print parse: docker inspected the objects it found, and only
// the ids that went are missing. Any other stderr line, or no listing, is the exec's
// failure, named the way every engine exec's is: its exit status and the first
// bounded stderr line.
func ciEngineListFromExec(stdout, stderr []byte, runErr error) (ciEngineListing, error) {
	listing, parseErr := parseCIEngineListing(stdout)
	if runErr == nil {
		if parseErr != nil {
			return ciEngineListing{}, parseErr
		}
		return listing, nil
	}
	if parseErr != nil || !ciEngineStderrOnlyVanished(runErr, stderr) {
		return ciEngineListing{}, fmt.Errorf("the listing exec into the %s container failed (%s)", ciCacheContainer, ciEngineFailure(runErr, stderr))
	}
	return listing, nil
}

// ciEnginePruneLogf logs one refusal or failure line of the engine prune, at most
// once an hour for the same kind of fault, as the cache trim's lines are held. The
// hold keys on kind, so the engine's lines do not share the cache trim's.
func (d *Daemon) ciEnginePruneLogf(kind, format string, args ...any) {
	if d.diskMaintSchedule.allowLog("ci-engine-prune "+kind, d.diskMaintenanceSeams().now(), cacheTrimRefusalLogInterval) {
		d.logger.Printf("%s", fmt.Sprintf(format, args...))
	}
}

// ciEngineCacheOverKeep is how far the build cache is over the keep size after the
// age prune: the reclaimable bytes docker reported before it ran, less the bytes it
// reclaimed, less the keep size, never below zero. usable is false when either figure
// is unread or negative, and then over is zero: a caller checks usable before it acts
// on over.
func ciEngineCacheOverKeep(reclaimable int64, reclaimableKnown bool, reclaimed int64, reclaimedKnown bool, keepBytes int64) (over int64, usable bool) {
	if !reclaimableKnown || !reclaimedKnown || reclaimable < 0 || reclaimed < 0 {
		return 0, false
	}
	over = reclaimable - reclaimed - keepBytes
	if over < 0 {
		return 0, true
	}
	return over, true
}
