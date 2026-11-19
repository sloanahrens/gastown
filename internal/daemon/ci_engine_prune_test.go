package daemon

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

// The engine prune's fixtures are hand-built listings: each container, image and
// volume is the map docker inspect would print, with only the fields the planner
// reads. Ids are full-length and hex, as docker prints them.

// engHex64 is a container id, the digest part of an image id, or an anonymous
// volume's name.
func engHex64(c byte) string { return strings.Repeat(string(c), 64) }

// engImageID is an image id, as docker prints it.
func engImageID(c byte) string { return "sha256:" + engHex64(c) }

// engAgo is the RFC 3339 time d before the fixture's first instant.
func engAgo(d time.Duration) string {
	return ciCacheTestStart.Add(-d).Format(time.RFC3339Nano)
}

// engCtr is a container inspect with its status, image and volume mounts, stopped
// for as long as it has existed: a job that ran and stopped at once.
func engCtr(id byte, image byte, status string, created time.Duration, volumes ...string) map[string]any {
	return engCtrStopped(id, image, status, created, created, volumes...)
}

// engCtrStopped is engCtr with the instant it stopped named, so created-stopped is
// how long it ran. docker prints the zero finish time for a container that never
// ran, which is status "created"; every other status here is one that ran and
// stopped.
func engCtrStopped(id byte, image byte, status string, created, stopped time.Duration, volumes ...string) map[string]any {
	mounts := make([]map[string]any, 0, len(volumes))
	for _, v := range volumes {
		mounts = append(mounts, map[string]any{"Type": "volume", "Name": v})
	}
	finished := "0001-01-01T00:00:00Z"
	if status == "exited" || status == "dead" {
		finished = engAgo(stopped)
	}
	return map[string]any{
		"Id":      engHex64(id),
		"Image":   engImageID(image),
		"Created": engAgo(created),
		"State":   map[string]any{"Status": status, "FinishedAt": finished},
		"Mounts":  mounts,
	}
}

// engImg is an image inspect with its tags and rootfs layers.
func engImg(id byte, created time.Duration, layers []string, tags ...string) map[string]any {
	return map[string]any{
		"Id":       engImageID(id),
		"RepoTags": tags,
		"Created":  engAgo(created),
		"RootFS":   map[string]any{"Type": "layers", "Layers": layers},
	}
}

// engVol is a volume inspect.
func engVol(name string, created time.Duration) map[string]any {
	return map[string]any{"Name": name, "CreatedAt": engAgo(created)}
}

// engListing is the JSON the list program prints for the given objects, with a
// data root of 40 GiB.
func engListing(t *testing.T, containers, images, volumes []map[string]any) []byte {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"data_root_kib": int64(40 << 20),
		"containers":    containers,
		"images":        images,
		"volumes":       volumes,
	})
	if err != nil {
		t.Fatalf("encoding the listing: %v", err)
	}
	return out
}

// engDecode decodes a listing the way the daemon reads the list program's output.
func engDecode(t *testing.T, raw []byte) ciEngineListing {
	t.Helper()
	l, err := parseCIEngineListing(raw)
	if err != nil {
		t.Fatalf("the listing does not decode: %v", err)
	}
	return l
}

// engineBaseListing is the engine the keep-rule test plans over. Each comment
// names the rule the object exercises.
func engineBaseListing(t *testing.T) ciEngineListing {
	t.Helper()
	const (
		layerA = "sha256:layer-a"
		layerB = "sha256:layer-b"
		layerC = "sha256:layer-c"
	)
	containers := []map[string]any{
		// A running container uses the daemon image and the volume 0s: both stay.
		engCtr('1', '2', "running", 1*time.Hour, engHex64('0')),
		// A running container from 300 hours ago uses a ci-base image: it stays.
		engCtr('3', 'd', "running", 300*time.Hour),
		// A stopped container older than the unused age goes, and the image it used
		// stops being used with it.
		engCtr('4', 'e', "exited", 100*time.Hour),
		// A stopped container from an hour ago stays: it is too new to prune.
		engCtr('5', '8', "exited", 1*time.Hour),
	}
	images := []map[string]any{
		// ci-base:latest and ci-base:gate-tools share one id: both tags are the latest.
		engImg('a', 2*time.Hour, []string{layerA, layerB, layerC}, "ci-base:latest", "ci-base:gate-tools"),
		// The id before the latest: the rollback, kept.
		engImg('b', 30*time.Hour, []string{layerA, layerB, "sha256:layer-d"}, "ci-base:jq17"),
		// An older ci-base id no container uses: removed, with no age gate.
		engImg('c', 200*time.Hour, []string{layerA, "sha256:layer-e", "sha256:layer-f"}, "ci-base:pre-jq17"),
		// An older ci-base id a running container uses: kept.
		engImg('d', 300*time.Hour, []string{layerA, layerB, "sha256:layer-g"}, "ci-base:pre-gate-tools"),
		// A daemon CI build from 100 hours ago that nothing uses: removed.
		engImg('e', 100*time.Hour, []string{layerA, "sha256:layer-h"}, "gastown-daemon:ci-1"),
		// A daemon CI build a running container uses: kept.
		engImg('2', 100*time.Hour, []string{layerA, "sha256:layer-i"}, "gastown-daemon:ci-2"),
		// A postgres image from 100 hours ago that nothing uses: removed.
		engImg('9', 100*time.Hour, []string{"sha256:layer-j"}, "pgvector/pgvector:pg16"),
		// An image from an hour ago, used by a stopped container that stays: kept.
		engImg('8', 1*time.Hour, []string{"sha256:layer-k"}, "alpine:3.19"),
		// An untagged image from 400 hours ago whose layers are the first two of the
		// latest base: its parent. docker refuses to remove a parent with children, so
		// it stays.
		engImg('7', 400*time.Hour, []string{layerA, layerB}),
		// An untagged image from 400 hours ago that nothing else builds on: removed.
		engImg('6', 400*time.Hour, []string{"sha256:layer-l"}),
		// A kept repository that is not ci-base, old and unused: kept by name.
		engImg('5', 400*time.Hour, []string{"sha256:layer-m"}, "org/tools:1"),
		// A daemon CI build from an hour ago that nothing uses: kept, as too new.
		engImg('4', 1*time.Hour, []string{layerA, "sha256:layer-n"}, "gastown-daemon:ci-3"),
	}
	volumes := []map[string]any{
		// Anonymous and used by the running container: kept.
		engVol(engHex64('0'), 30*time.Hour),
		// Anonymous, unused and older than the age: removed.
		engVol(engHex64('1'), 30*time.Hour),
		// A 32-hex name is not the shape docker gives an anonymous volume: kept.
		engVol(strings.Repeat("2", 32), 30*time.Hour),
		// Anonymous and too new: kept.
		engVol(engHex64('3'), 1*time.Hour),
		// A named volume the gate uses: never a candidate.
		engVol("ci-gocache", 400*time.Hour),
	}
	return engDecode(t, engListing(t, containers, images, volumes))
}

// TestCIEnginePruneKeepsCIBaseAndUsedImages: the planner keeps the ci-base latest
// tag's id, every tag on it, and the one id before it; keeps images a container
// that stays uses, images in a kept repository, and a parent of a kept image; and
// removes the rest, with the stopped container's image freed by its removal.
func TestCIEnginePruneKeepsCIBaseAndUsedImages(t *testing.T) {
	t.Parallel()
	plan, err := planCIEnginePrune(engineBaseListing(t), ciCacheTestStart, 48*time.Hour, 24*time.Hour, []string{"ci-base", "org/tools"})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}

	gotImages := map[string]bool{}
	for _, img := range plan.images {
		gotImages[img.ID] = true
	}
	wantImages := []string{engImageID('c'), engImageID('e'), engImageID('9'), engImageID('6')}
	if len(gotImages) != len(wantImages) {
		t.Errorf("planned images = %v, want exactly %v", gotImages, wantImages)
	}
	for _, id := range wantImages {
		if !gotImages[id] {
			t.Errorf("image %s is not planned for removal", id)
		}
	}

	if len(plan.containers) != 1 || plan.containers[0].ID != engHex64('4') {
		t.Errorf("planned containers = %+v, want only the old stopped container", plan.containers)
	}
	if len(plan.volumes) != 1 || plan.volumes[0].Name != engHex64('1') {
		t.Errorf("planned volumes = %+v, want only the unused anonymous volume", plan.volumes)
	}

	// A child goes before its parent: the removal order is most layers first.
	for i := 1; i < len(plan.images); i++ {
		if len(plan.images[i].RootFS.Layers) > len(plan.images[i-1].RootFS.Layers) {
			t.Errorf("images planned %d before %d: a parent would go before its child", i-1, i)
		}
	}
}

// TestCIEnginePruneKeepsTheBaseTheRollbackAndInUseImagesAtOneHour: the shortest
// unused age the patrol accepts frees more stopped containers and volumes but keeps
// every image the base rule or a container that stays protects: ci-base:latest, the
// rollback id before it, and the image each running container uses.
func TestCIEnginePruneKeepsTheBaseTheRollbackAndInUseImagesAtOneHour(t *testing.T) {
	t.Parallel()
	plan, err := planCIEnginePrune(engineBaseListing(t), ciCacheTestStart, 48*time.Hour, time.Hour, []string{"ci-base", "org/tools"})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	planned := map[string]bool{}
	for _, img := range plan.images {
		planned[img.ID] = true
	}
	for _, keep := range []struct {
		id   string
		what string
	}{
		{engImageID('a'), "the ci-base:latest image"},
		{engImageID('b'), "the rollback image before latest"},
		{engImageID('2'), "the image the running container uses"},
		{engImageID('d'), "the ci-base image the running container uses"},
	} {
		if planned[keep.id] {
			t.Errorf("%s (%s) is planned for removal at a one-hour unused age", keep.what, keep.id)
		}
	}
}

// TestCIEnginePruneKeepsCurrentLatestAndItsImmediatePredecessor: the ci-base rollback
// is the id built just before the latest tag's id, not the newest other id. A base
// built after the latest, under another tag, waits out the age like any image;
// one past the age goes, as does an older one.
func TestCIEnginePruneKeepsCurrentLatestAndItsImmediatePredecessor(t *testing.T) {
	t.Parallel()
	images := []map[string]any{
		// The latest tag's id, built 50 hours ago: kept.
		engImg('a', 50*time.Hour, []string{"sha256:layer-a"}, "ci-base:latest"),
		// The id built just before the latest: the rollback, kept.
		engImg('b', 100*time.Hour, []string{"sha256:layer-b"}, "ci-base:jq17"),
		// The newest other id, built two hours ago under another tag: not kept by
		// the rollback rule, but younger than the age, so it waits like any image.
		engImg('c', 2*time.Hour, []string{"sha256:layer-c"}, "ci-base:pre-gate-tools"),
		// An older id: removed.
		engImg('d', 200*time.Hour, []string{"sha256:layer-d"}, "ci-base:pre-jq17"),
		// Another id not kept by the rule and past the age: removed.
		engImg('e', 150*time.Hour, []string{"sha256:layer-e"}, "ci-base:stale-build"),
	}
	plan, err := planCIEnginePrune(engDecode(t, engListing(t, nil, images, nil)), ciCacheTestStart, 48*time.Hour, 24*time.Hour, []string{"ci-base"})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	got := map[string]bool{}
	for _, img := range plan.images {
		got[img.ID] = true
	}
	for _, id := range []string{engImageID('a'), engImageID('b')} {
		if got[id] {
			t.Errorf("image %s is planned for removal, want the latest and its predecessor kept", id)
		}
	}
	if got[engImageID('c')] {
		t.Errorf("image %s is planned for removal, want a base younger than the age to wait it out", engImageID('c'))
	}
	for _, id := range []string{engImageID('d'), engImageID('e')} {
		if !got[id] {
			t.Errorf("image %s is not planned for removal", id)
		}
	}
	if len(plan.images) != 2 {
		t.Errorf("planned %d images, want exactly 2", len(plan.images))
	}
}

// TestCIEnginePruneKeepReposProtectASharedCIBaseImage: an image tagged with a kept
// repository other than ci-base stays, even when it is also tagged ci-base and is
// neither the latest nor its predecessor. ci-base in the list does not shield an
// image: an old base with no other kept tag goes.
func TestCIEnginePruneKeepReposProtectASharedCIBaseImage(t *testing.T) {
	t.Parallel()
	images := []map[string]any{
		engImg('a', 2*time.Hour, []string{"sha256:layer-a"}, "ci-base:latest"),
		// The rollback: kept whatever the keep list names.
		engImg('6', 50*time.Hour, []string{"sha256:layer-p"}, "ci-base:jq17"),
		// Old, ci-base-tagged, and tagged with org/tools: kept only by that name.
		engImg('b', 300*time.Hour, []string{"sha256:layer-b"}, "ci-base:pre-tools", "org/tools:1"),
		// Old, ci-base-tagged, with no other kept tag: removed.
		engImg('c', 400*time.Hour, []string{"sha256:layer-c"}, "ci-base:pre-jq17"),
	}
	listing := engDecode(t, engListing(t, nil, images, nil))

	for name, tc := range map[string]struct {
		keepRepos []string
		wantB     bool
	}{
		"org/tools is kept, so its shared image stays":    {keepRepos: []string{"ci-base", "org/tools"}, wantB: true},
		"ci-base alone does not shield a shared base tag": {keepRepos: []string{"ci-base"}, wantB: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan, err := planCIEnginePrune(listing, ciCacheTestStart, 48*time.Hour, 24*time.Hour, tc.keepRepos)
			if err != nil {
				t.Fatalf("planning: %v", err)
			}
			got := map[string]bool{}
			for _, img := range plan.images {
				got[img.ID] = true
			}
			if got[engImageID('b')] == tc.wantB {
				t.Errorf("shared image planned for removal = %v, want %v", got[engImageID('b')], !tc.wantB)
			}
			if !got[engImageID('c')] {
				t.Errorf("the old ci-base image with no kept tag is not planned for removal")
			}
			if got[engImageID('a')] || got[engImageID('6')] {
				t.Errorf("the latest or its predecessor is planned for removal")
			}
		})
	}
}

// TestCIEnginePruneRefusesAListingItCannotRead: a listing with a missing figure, an
// id or name in an unexpected shape, or a time it cannot read is refused, never
// planned as empty or with the item read as absent.
func TestCIEnginePruneRefusesAListingItCannotRead(t *testing.T) {
	t.Parallel()
	now := ciCacheTestStart
	ok := func(t *testing.T) ciEngineListing {
		t.Helper()
		return engDecode(t, engListing(t, nil, []map[string]any{engImg('a', 2*time.Hour, nil, "alpine:3")}, nil))
	}
	cases := map[string]func(l *ciEngineListing){
		"no data root figure": func(l *ciEngineListing) { l.DataRootKiB = nil },
		"container id not hex": func(l *ciEngineListing) {
			l.Containers = []ciEngineContainer{{ID: "not-a-container", Image: engImageID('a'), Created: engAgo(time.Hour)}}
		},
		"container image id not docker's": func(l *ciEngineListing) {
			l.Containers = []ciEngineContainer{{ID: engHex64('1'), Image: "alpine", Created: engAgo(time.Hour)}}
		},
		"container time unreadable": func(l *ciEngineListing) {
			l.Containers = []ciEngineContainer{{ID: engHex64('1'), Image: engImageID('a'), Created: "yesterday"}}
		},
		"stopped container finish time unreadable": func(l *ciEngineListing) {
			l.Containers = []ciEngineContainer{{ID: engHex64('1'), Image: engImageID('a'), Created: engAgo(time.Hour)}}
			l.Containers[0].State.Status = "exited"
			l.Containers[0].State.FinishedAt = "yesterday"
		},
		"stopped container finish time before its creation": func(l *ciEngineListing) {
			l.Containers = []ciEngineContainer{{ID: engHex64('1'), Image: engImageID('a'), Created: engAgo(time.Hour)}}
			l.Containers[0].State.Status = "exited"
			l.Containers[0].State.FinishedAt = engAgo(2 * time.Hour)
		},
		"image id not docker's": func(l *ciEngineListing) {
			l.Images = append(l.Images, ciEngineImage{ID: "sha256:short", Created: engAgo(time.Hour)})
		},
		"image time zero": func(l *ciEngineListing) {
			l.Images = append(l.Images, ciEngineImage{ID: engImageID('b'), Created: "0001-01-01T00:00:00Z"})
		},
		"volume name with a space": func(l *ciEngineListing) {
			l.Volumes = append(l.Volumes, ciEngineVolume{Name: "bad name", CreatedAt: engAgo(30 * time.Hour)})
		},
		"volume time missing": func(l *ciEngineListing) {
			l.Volumes = append(l.Volumes, ciEngineVolume{Name: engHex64('4')})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := ok(t)
			mutate(&l)
			if _, err := planCIEnginePrune(l, now, 48*time.Hour, 24*time.Hour, []string{"ci-base"}); err == nil {
				t.Errorf("a listing with %s was planned without an error", name)
			}
		})
	}
}

// newCIEngineFixture is the daemon a CI engine prune test runs: the CI cache
// trim's fixture, with the list program answered from listing and the remove
// program answered from remove.
// engineRemoveAnswers says how the fake engine answers the remove step's docker
// commands: fail makes every docker removal exit 1 with stderr and remove nothing,
// gone makes that failing removal exit 1 with nothing left (the id was freed by an
// earlier argument), short makes docker print one id fewer than it was given and
// leave that id in the engine, garbage makes it print lines naming none, and
// noInspect makes the presence inspect fail to run. The zero value is an engine
// that removes everything it is asked to and says so.
type engineRemoveAnswers struct {
	fail      bool
	stderr    string
	gone      bool
	short     bool
	garbage   bool
	noInspect bool
	// duOut replaces du's reply when duSet; duFail makes du exit 1; duReply, when
	// set, is du's answer in full, a total on stdout with a non-zero status and the
	// stderr that explains it.
	duOut   string
	duSet   bool
	duFail  bool
	duReply *cliReply
}

// engineDockerIDs returns the ids a docker removal exec was given: the argv after
// the verb words and flags.
func engineDockerIDs(args []string) []string {
	i := slices.Index(args, "docker")
	if i < 0 {
		return nil
	}
	var ids []string
	for _, a := range args[i+1:] {
		switch a {
		case "rm", "rmi", "-f", "volume":
			continue
		}
		ids = append(ids, a)
	}
	return ids
}

func newCIEngineFixture(t *testing.T, mode string, listing []byte, remove engineRemoveAnswers) *ciCacheFixture {
	t.Helper()
	f := newCICacheFixture(t, mode)
	// held is the set of ids the fake engine still has after a removal that did not
	// take them; the presence inspect echoes the requested ids it holds.
	var heldMu sync.Mutex
	held := map[string]bool{}
	// This fixture's engine prune is due at its first tick: the default interval,
	// and no recorded run.
	f.d.patrolConfig.Patrols.DiskMaintenance.CIEnginePruneInterval = ""
	delete(f.d.diskMaintSchedule.lastRun, ciEnginePruneName)
	f.cli.answer = func(c cliCall) cliReply {
		verb := cacheTrimVerb(c.args)
		switch {
		case strings.HasPrefix(verb, "get pods "):
			return f.pods
		case strings.Contains(verb, "ci-engine-list"):
			return cliReply{stdout: string(listing)}
		case strings.Contains(verb, "-- du -sk"):
			if remove.duReply != nil {
				return *remove.duReply
			}
			if remove.duFail {
				return cliReply{code: 1, stderr: "du: cannot read"}
			}
			if remove.duSet {
				return cliReply{stdout: remove.duOut}
			}
			return cliReply{stdout: "10485760\t" + ciEngineRoot + "\n"}
		case strings.Contains(verb, "-- docker builder prune"):
			return cliReply{stdout: "Total:\t0B\n"}
		case strings.Contains(verb, "-- docker system df"):
			return cliReply{stdout: "Build Cache 0B 0B\n"}
		case strings.Contains(verb, " inspect "):
			if remove.noInspect {
				return cliReply{startErr: errors.New("kubectl: could not run")}
			}
			i := slices.Index(c.args, "--format")
			var out strings.Builder
			gone := false
			heldMu.Lock()
			for _, id := range c.args[i+2:] {
				if held[id] {
					out.WriteString(id + "\n")
				} else {
					gone = true
				}
			}
			heldMu.Unlock()
			if gone {
				return cliReply{stdout: out.String(), code: 1, stderr: "Error: No such object"}
			}
			return cliReply{stdout: out.String()}
		case strings.Contains(verb, "-- docker "):
			ids := engineDockerIDs(c.args)
			if remove.fail {
				if !remove.gone {
					heldMu.Lock()
					for _, id := range ids {
						held[id] = true
					}
					heldMu.Unlock()
				}
				return cliReply{code: 1, stderr: remove.stderr}
			}
			if remove.garbage {
				return cliReply{stdout: "garbage\n"}
			}
			if remove.short && len(ids) > 0 {
				heldMu.Lock()
				held[ids[0]] = true
				heldMu.Unlock()
				ids = ids[1:]
			}
			var out strings.Builder
			for _, id := range ids {
				if strings.Contains(verb, "-- docker rmi") {
					out.WriteString("Untagged: some/repo:tag\nDeleted: " + id + "\n")
				} else {
					out.WriteString(id + "\n")
				}
			}
			return cliReply{stdout: out.String()}
		}
		return cliReply{}
	}
	return f
}

// engineDockerExecs returns the exec calls that ran docker with the given verb words.
func engineDockerExecs(f *ciCacheFixture, verb string) []cliCall {
	var out []cliCall
	for _, c := range f.execs() {
		if strings.Contains(cacheTrimVerb(c.args)+" ", "-- docker "+verb+" ") {
			out = append(out, c)
		}
	}
	return out
}

// engineExecs returns the exec calls whose program is named name.
func engineExecs(f *ciCacheFixture, name string) []cliCall {
	var out []cliCall
	for _, c := range f.execs() {
		if strings.Contains(cacheTrimVerb(c.args), name) {
			out = append(out, c)
		}
	}
	return out
}

// TestCIEnginePruneDryRunRemovesNothing: a dry run lists the engine once and runs
// no removal, and names what it would remove in its log.
func TestCIEnginePruneDryRunRemovesNothing(t *testing.T) {
	t.Parallel()
	f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeDryRun, engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 100*time.Hour)},
		[]map[string]any{engImg('e', 100*time.Hour, []string{"sha256:layer-h"}, "gastown-daemon:ci-1")},
		[]map[string]any{engVol(engHex64('1'), 30*time.Hour)},
	), engineRemoveAnswers{})
	f.tick()
	f.drain()

	if n := len(engineDockerExecs(f, "")); n != 0 {
		t.Fatalf("a dry run ran %d docker command(s)", n)
	}
	if n := len(engineExecs(f, "ci-engine-list")); n != 1 {
		t.Fatalf("a dry run ran the list program %d time(s), want once", n)
	}
	logs := f.logs.String()
	for _, want := range []string{"dry_run: would remove container", "dry_run: would remove image", "dry_run: would remove volume", "nothing removed"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the dry run's log lacks %q:\n%s", want, logs)
		}
	}
	if last := f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName); last.IsZero() {
		t.Errorf("a dry run that reached a verdict left the engine prune unscheduled")
	}
}

// TestCIEnginePruneApplyRemovesThePlannedIds: an apply runs one docker command per
// kind over exactly the planned ids, prunes the build cache at the cutoff, and takes
// its verdict from what docker reported removed; a removal docker did not report,
// or did not perform, is a failed run that is tried again soon.
func TestCIEnginePruneApplyRemovesThePlannedIds(t *testing.T) {
	t.Parallel()
	listing := engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 100*time.Hour)},
		[]map[string]any{engImg('e', 100*time.Hour, []string{"sha256:layer-h"}, "gastown-daemon:ci-1")},
		[]map[string]any{engVol(engHex64('1'), 30*time.Hour)},
	)

	t.Run("one docker command per kind over the planned ids, and docker's report is the verdict", func(t *testing.T) {
		t.Parallel()
		f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, listing, engineRemoveAnswers{})
		f.tick()
		f.drain()
		for verb, want := range map[string][]string{
			"rm":        {"docker", "rm", engHex64('4')},
			"rmi":       {"docker", "rmi", "-f", engImageID('e')},
			"volume rm": {"docker", "volume", "rm", engHex64('1')},
		} {
			execs := engineDockerExecs(f, verb)
			if len(execs) != 1 {
				t.Fatalf("docker %s ran %d time(s), want once", verb, len(execs))
			}
			i := slices.Index(execs[0].args, "docker")
			if got := execs[0].args[i:]; !slices.Equal(got, want) {
				t.Errorf("docker %s argv = %q, want %q", verb, got, want)
			}
		}
		prunes := engineDockerExecs(f, "builder prune")
		if len(prunes) != 1 || !slices.Contains(prunes[0].args, "until=6h") {
			t.Errorf("build cache prune = %q, want one run with until=6h", prunes)
		}
		// The list program is shell by design; no removal is.
		for _, c := range engineDockerExecs(f, "") {
			if slices.Contains(c.args, "sh") || slices.Contains(c.args, "-c") {
				t.Errorf("the remove step ran a shell: %q", c.args)
			}
		}
		if f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName).IsZero() {
			t.Errorf("a verdict left the engine prune unscheduled")
		}
		if !strings.Contains(f.logs.String(), "removed 1 containers, 1 images, 1 volumes") {
			t.Errorf("the verdict line is missing:\n%s", f.logs.String())
		}
	})

	// A removal docker leaves in place is a failed run: no verdict, a retry soon, and
	// a line that says which kind and how the removal ended. Every kind still ran.
	for name, tc := range map[string]struct {
		remove  engineRemoveAnswers
		wantLog string
	}{
		"docker prints one id fewer than planned and keeps it": {
			remove:  engineRemoveAnswers{short: true},
			wantLog: "docker left 1 of 1 planned image(s)",
		},
		"a removal exec fails and the object stays": {
			remove:  engineRemoveAnswers{fail: true, stderr: "volume is in use"},
			wantLog: "(the removal ended: exit status 1, stderr: volume is in use)",
		},
		"the presence check cannot run": {
			remove:  engineRemoveAnswers{garbage: true, noInspect: true},
			wantLog: "presence check could not run",
		},
	} {
		t.Run(name+" is a failed run, tried again soon", func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, listing, tc.remove)
			f.tick()
			f.drain()
			if last := f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName); !last.IsZero() {
				t.Errorf("a failed remove was recorded as a verdict at %v", last)
			}
			if retry := f.d.diskMaintSchedule.retryAtOf(ciEnginePruneName); retry.IsZero() {
				t.Errorf("a failed remove left no retry")
			}
			if !strings.Contains(f.logs.String(), tc.wantLog) {
				t.Errorf("the failed run does not name %q:\n%s", tc.wantLog, f.logs.String())
			}
			// Whatever failed, every kind and the build cache prune still ran, and the
			// size after was read.
			for _, verb := range []string{"rm", "rmi", "volume rm", "builder prune"} {
				if len(engineDockerExecs(f, verb)) != 1 {
					t.Errorf("docker %s ran %d time(s) after a failure, want once", verb, len(engineDockerExecs(f, verb)))
				}
			}
			if len(engineExecs(f, "du -sk")) == 0 {
				t.Errorf("the size after was not read after a failed removal")
			}
		})
	}

	// docker rmi -f exits 1 over an id an earlier argument already freed. The id is
	// gone, so the kind counts as removed and the run reaches a verdict.
	t.Run("a removal that exits 1 over objects already gone is a verdict", func(t *testing.T) {
		t.Parallel()
		f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, listing, engineRemoveAnswers{fail: true, gone: true})
		f.tick()
		f.drain()
		if f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName).IsZero() {
			t.Errorf("ids absent from the engine after the removal did not reach a verdict:\n%s", f.logs.String())
		}
		if !strings.Contains(f.logs.String(), "removed 1 containers, 1 images, 1 volumes") {
			t.Errorf("the verdict line is missing:\n%s", f.logs.String())
		}
	})

	t.Run("docker prints no id but the objects are gone is a verdict", func(t *testing.T) {
		t.Parallel()
		f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, listing, engineRemoveAnswers{garbage: true})
		f.tick()
		f.drain()
		if f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName).IsZero() {
			t.Errorf("objects absent from the engine did not reach a verdict:\n%s", f.logs.String())
		}
	})
}

// TestCIEnginePruneUsesTheConfiguredUnusedAge: the patrol's ci_engine_unused_age
// reaches the planner and the build cache prune. At 2h a 3h-old stopped container,
// its image and an unused volume go, and docker is told until=2h; at 8h a 7h-old
// container and volume stay, and docker is still told until=8h. An absent setting
// is 6h: a 7h-old one goes and docker is told until=6h.
func TestCIEnginePruneUsesTheConfiguredUnusedAge(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setting    string
		age        time.Duration
		wantRemove bool
		wantUntil  string
	}{
		"absent setting is six hours":       {age: 7 * time.Hour, wantRemove: true, wantUntil: "until=6h"},
		"two hours removes what is older":   {setting: "2h", age: 3 * time.Hour, wantRemove: true, wantUntil: "until=2h"},
		"eight hours keeps what is younger": {setting: "8h", age: 7 * time.Hour, wantRemove: false, wantUntil: "until=8h"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			listing := engListing(t,
				[]map[string]any{engCtr('4', 'e', "exited", tc.age)},
				[]map[string]any{engImg('e', 100*time.Hour, []string{"sha256:layer-h"}, "gastown-daemon:ci-1")},
				[]map[string]any{engVol(engHex64('1'), tc.age)},
			)
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, listing, engineRemoveAnswers{})
			f.d.patrolConfig.Patrols.DiskMaintenance.CIEngineUnusedAge = tc.setting
			f.tick()
			f.drain()

			for _, verb := range []string{"rm", "rmi", "volume rm"} {
				want := 0
				if tc.wantRemove {
					want = 1
				}
				if got := len(engineDockerExecs(f, verb)); got != want {
					t.Errorf("docker %s ran %d time(s), want %d:\n%s", verb, got, want, f.logs.String())
				}
			}
			prunes := engineDockerExecs(f, "builder prune")
			if len(prunes) != 1 || !slices.Contains(prunes[0].args, tc.wantUntil) {
				t.Errorf("build cache prune = %q, want one run with %s", prunes, tc.wantUntil)
			}
		})
	}
}

// TestCIEngineRemovedReadsDockersOutput: docker rm and docker volume rm print each
// removed id or name on a line; docker rmi prints Deleted: before each image id and
// Untagged: before each dropped tag, which is not a removal. Anything else names no
// removal, so a plan id docker did not print is missing.
func TestCIEngineRemovedReadsDockersOutput(t *testing.T) {
	t.Parallel()
	r := ciEngineRemoval{kind: "image", ids: []string{"sha256:aaa", "sha256:bbb"}}
	for name, tc := range map[string]struct {
		out     string
		missing []string
	}{
		"both deleted":          {"Untagged: ci-base:old\nDeleted: sha256:aaa\nDeleted: sha256:bbb\n", nil},
		"one deleted":           {"Deleted: sha256:aaa\n", []string{"sha256:bbb"}},
		"only untagged":         {"Untagged: ci-base:old\n", []string{"sha256:aaa", "sha256:bbb"}},
		"garbage":               {"garbage\n", []string{"sha256:aaa", "sha256:bbb"}},
		"empty":                 {"", []string{"sha256:aaa", "sha256:bbb"}},
		"plain lines (rm form)": {"sha256:aaa\nsha256:bbb\n", nil},
		// Captured from the runner's dind (docker 28, containerd image store): a tagged
		// image removed by its full id prints these two lines and no parent layers.
		"captured rmi -f of a tagged id": {"Untagged: forgejo:3000/bot-registry/gastown-daemon:8707a343dbf7\nDeleted: sha256:aaa\n", []string{"sha256:bbb"}},
	} {
		if got := ciEngineMissing(r, []byte(tc.out)); !slices.Equal(got, tc.missing) {
			t.Errorf("%s: missing = %q, want %q", name, got, tc.missing)
		}
	}
}

// TestCIEnginePruneFailsWhenTheEngineIsNotRunning: a runner pod whose dind
// container is not running is a failed run: nothing is listed or removed, and the
// run is tried again soon rather than read as an engine with nothing to prune.
func TestCIEnginePruneFailsWhenTheEngineIsNotRunning(t *testing.T) {
	t.Parallel()
	f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engListing(t, nil, nil, nil), engineRemoveAnswers{})
	f.pods = cliReply{stdout: ciCachePodsDindWaitingJSON}
	f.tick()
	f.drain()

	if n := len(engineExecs(f, "ci-engine-list")) + len(engineExecs(f, "ci-engine-remove")); n != 0 {
		t.Fatalf("a stopped engine ran %d exec(s), want none", n)
	}
	if last := f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName); !last.IsZero() {
		t.Errorf("a stopped engine was recorded as a verdict at %v", last)
	}
	if retry := f.d.diskMaintSchedule.retryAtOf(ciEnginePruneName); retry.IsZero() {
		t.Errorf("a stopped engine left no retry")
	}
	if !strings.Contains(f.logs.String(), "not running") {
		t.Errorf("the failed run did not say the engine is not running:\n%s", f.logs.String())
	}
}

// TestCIEnginePruneFailedListingRemovesNothing: a listing that does not decode,
// and a list exec that fails, are failed runs. Neither removes anything, and the
// run is tried again soon.
func TestCIEnginePruneFailedListingRemovesNothing(t *testing.T) {
	t.Parallel()
	for name, list := range map[string]cliReply{
		"listing does not decode": {stdout: "not json"},
		"list exec fails":         {code: 1, stderr: "Cannot connect to the Docker daemon"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engListing(t, nil, nil, nil), engineRemoveAnswers{})
			f.cli.answer = func(c cliCall) cliReply {
				verb := cacheTrimVerb(c.args)
				switch {
				case strings.HasPrefix(verb, "get pods "):
					return f.pods
				case strings.Contains(verb, "ci-engine-list"):
					return list
				}
				return cliReply{}
			}
			f.tick()
			f.drain()
			if n := len(engineExecs(f, "ci-engine-remove")); n != 0 {
				t.Fatalf("a failed listing ran the remove program %d time(s)", n)
			}
			if last := f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName); !last.IsZero() {
				t.Errorf("a failed listing was recorded as a verdict at %v", last)
			}
			if retry := f.d.diskMaintSchedule.retryAtOf(ciEnginePruneName); retry.IsZero() {
				t.Errorf("a failed listing left no retry")
			}
		})
	}
}

// TestCIEnginePruneScriptsHoldNoKeepLogic: the two shell programs hold no keep, age
// or repository rule. The keep set is decided in Go; the programs only list the
// engine, or remove the ids and names the planner named, and the build cache
// cutoff arrives as argv data.
func TestCIEnginePruneScriptsHoldNoKeepLogic(t *testing.T) {
	t.Parallel()
	for name, script := range map[string]string{"list": ciEngineListScript} {
		for _, rule := range []string{ciEngineBaseRepo, "keep", "24h", "48h", "mtime", "find "} {
			if strings.Contains(script, rule) {
				t.Errorf("the %s program holds %q: keep, age and repository logic is decided in Go", name, rule)
			}
		}
	}
}

// replaceEngineReply answers the calls whose verb contains match with reply, and every
// other call as the fixture does.
func replaceEngineReply(f *ciCacheFixture, match string, reply cliReply) {
	answer := f.cli.answer
	f.cli.answer = func(c cliCall) cliReply {
		if strings.Contains(cacheTrimVerb(c.args), match) {
			return reply
		}
		return answer(c)
	}
}

// engineLogLine returns the first logged line that contains marker, or "".
func engineLogLine(logs, marker string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}

// engineStderrField returns the text a failure line logs after "stderr: ", up to the
// line's last ")", which closes the parenthesis the stderr sits in. It is "" when the
// line logs no stderr.
func engineStderrField(line string) string {
	const open = "stderr: "
	i := strings.Index(line, open)
	j := strings.LastIndex(line, ")")
	if i < 0 || j < i+len(open) {
		return ""
	}
	return line[i+len(open) : j]
}

// engineFailureListing is the listing the failure tests prune: one container, one
// image and one anonymous volume, all old enough to go, so every removal runs.
func engineFailureListing(t *testing.T) []byte {
	t.Helper()
	return engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 100*time.Hour)},
		[]map[string]any{engImg('e', 100*time.Hour, []string{"sha256:layer-h"}, "gastown-daemon:ci-1")},
		[]map[string]any{engVol(engHex64('1'), 30*time.Hour)},
	)
}

// TestCIEnginePruneFailureLineCarriesTheFirstStderrLine: a failed step's log line names
// its exit status and the first line of the program's stderr, from every exec that
// logs a failure. The lines after the first are not logged.
func TestCIEnginePruneFailureLineCarriesTheFirstStderrLine(t *testing.T) {
	t.Parallel()
	const first = "Error response from daemon: conflict: unable to delete"
	const later = "a later stderr line"
	stderr := first + "\n" + later + "\n"
	const exited = "exit status 1"
	for name, setup := range map[string]struct {
		match  string
		reply  cliReply
		remove engineRemoveAnswers
		marker string
		status string
	}{
		"list exec":         {match: "ci-engine-list", reply: cliReply{code: 1, stderr: stderr}, marker: "the listing exec", status: exited},
		"build cache prune": {match: "-- docker builder prune", reply: cliReply{code: 1, stderr: stderr}, marker: "the build cache prune", status: exited},
		"size read":         {match: "-- du -sk", reply: cliReply{code: 1, stderr: stderr}, marker: "data root's size after", status: exited},
		"removal":           {remove: engineRemoveAnswers{fail: true, stderr: stderr}, marker: "planned image(s)", status: exited},
		"presence check":    {match: "-- docker image inspect", reply: cliReply{startErr: errors.New("exec stream broke"), stderr: stderr}, remove: engineRemoveAnswers{fail: true}, marker: "presence check could not run", status: "no exit status"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engineFailureListing(t), setup.remove)
			if setup.match != "" {
				replaceEngineReply(f, setup.match, setup.reply)
			}
			f.tick()
			f.drain()
			logs := f.logs.String()
			line := engineLogLine(logs, setup.marker)
			if line == "" {
				t.Fatalf("no failure line for %q in:\n%s", setup.marker, logs)
			}
			if got := engineStderrField(line); got != first {
				t.Errorf("the failure line's stderr = %q, want the first line %q:\n%s", got, first, line)
			}
			if strings.Contains(logs, later) {
				t.Errorf("a stderr line after the first reached the log:\n%s", logs)
			}
			if !strings.Contains(line, setup.status+", stderr: ") {
				t.Errorf("the failure line does not name %q before its stderr:\n%s", setup.status, line)
			}
		})
	}
}

// TestCIEnginePruneFailureLineStderrIsBounded: the stderr a failure line carries is
// at most 200 characters, control characters and newlines are spaces, and a cut never
// splits a character.
func TestCIEnginePruneFailureLineStderrIsBounded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stderr string
		want   string
	}{
		"a long line is cut at 200 characters": {
			stderr: strings.Repeat("x", 300) + "\nnext",
			want:   strings.Repeat("x", 200),
		},
		"control characters and tabs become spaces": {
			stderr: "one\x1b[2J\ttwo\rthree\nnext",
			want:   "one [2J two three",
		},
		"format and bidi characters become spaces": {
			stderr: "a\u202eb\u2066c\u200bd\ufeffe\nnext",
			want:   "a b c d e",
		},
		"blank lines before the first line are skipped": {
			stderr: "\n \nfirst real line\nlater",
			want:   "first real line",
		},
		"a cut counts characters, not bytes": {
			stderr: strings.Repeat("é", 250),
			want:   strings.Repeat("é", 200),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engineFailureListing(t), engineRemoveAnswers{})
			replaceEngineReply(f, "-- docker builder prune", cliReply{code: 1, stderr: tc.stderr})
			f.tick()
			f.drain()
			logs := f.logs.String()
			line := engineLogLine(logs, "the build cache prune")
			if line == "" {
				t.Fatalf("no build cache prune failure line in:\n%s", logs)
			}
			if got := engineStderrField(line); got != tc.want {
				t.Errorf("the failure line's stderr = %q (%d characters), want %q (%d)", got, len([]rune(got)), tc.want, len([]rune(tc.want)))
			}
			if strings.ContainsRune(line, '\x1b') || strings.Contains(line, "next") {
				t.Errorf("the failure line holds a control character or a later line:\n%s", line)
			}
		})
	}
}

// TestCIEnginePruneClusterExecErrorsAreNotLoggedAsEngineStderr: kubectl's own errors
// about the pod, the context or the connection, and its report of the remote
// command's status, are not the engine's stderr. The exit report names the status the
// exec ended with, so a line naming another number, or reached with no status, is a
// program's own line. A failed exec whose stderr is only kubectl's is logged with its
// exit status alone, and when kubectl's line comes before the program's, the program's
// line is the one logged.
func TestCIEnginePruneClusterExecErrorsAreNotLoggedAsEngineStderr(t *testing.T) {
	t.Parallel()
	const kubectlNotFound = `Error from server (NotFound): pods "runner-1" not found`
	const kubectlUpgrade = "error: unable to upgrade connection: container not found"
	const kubectlKlog = "E1103 10:00:00.000000    1234 memcache.go:265] couldn't get current server API group list"
	const kubectlKlogWarning = "W1103 10:00:00.000000    1234 warnings.go:70] deprecated API"
	const kubectlWarning = "Warning: deprecated API version"
	const kubectlExitReport = "command terminated with exit code 1"
	for name, tc := range map[string]struct {
		match   string
		reply   cliReply
		marker  string
		kubectl string
		want    string
	}{
		"list exec, kubectl's error only": {
			match: "ci-engine-list", reply: cliReply{code: 1, stderr: kubectlNotFound + "\n"},
			marker: "the listing exec", kubectl: kubectlNotFound, want: "",
		},
		"build cache prune, kubectl's error only": {
			match: "-- docker builder prune", reply: cliReply{code: 1, stderr: kubectlUpgrade + "\n"},
			marker: "the build cache prune", kubectl: kubectlUpgrade, want: "",
		},
		"kubectl's line before the program's": {
			match: "-- docker builder prune", reply: cliReply{code: 1, stderr: kubectlUpgrade + "\nprogram line\n"},
			marker: "the build cache prune", kubectl: kubectlUpgrade, want: "program line",
		},
		"klog error line before the program's": {
			match: "-- docker builder prune", reply: cliReply{code: 1, stderr: kubectlKlog + "\nprogram line\n"},
			marker: "the build cache prune", kubectl: kubectlKlog, want: "program line",
		},
		"klog warning line before the program's": {
			match: "-- docker builder prune", reply: cliReply{code: 1, stderr: kubectlKlogWarning + "\nprogram line\n"},
			marker: "the build cache prune", kubectl: kubectlKlogWarning, want: "program line",
		},
		"kubectl warning line before the program's": {
			match: "-- docker builder prune", reply: cliReply{code: 1, stderr: kubectlWarning + "\nprogram line\n"},
			marker: "the build cache prune", kubectl: kubectlWarning, want: "program line",
		},
		"the engine's own Error line is kept": {
			match: "-- docker builder prune", reply: cliReply{code: 1, stderr: "Error: No such object: x\n"},
			marker: "the build cache prune", want: "Error: No such object: x",
		},
		"the exit report kubectl adds, before the program's": {
			match: "-- docker builder prune", reply: cliReply{code: 1, stderr: kubectlExitReport + "\nprogram line\n"},
			marker: "the build cache prune", kubectl: kubectlExitReport, want: "program line",
		},
		"build cache prune, kubectl's exit report only": {
			match: "-- docker builder prune", reply: cliReply{code: 1, stderr: kubectlExitReport + "\n"},
			marker: "the build cache prune", kubectl: kubectlExitReport, want: "",
		},
		"another program's line in the exit report's shape": {
			match: "-- docker builder prune", reply: cliReply{code: 1, stderr: "command terminated with exit code 2\nprogram line\n"},
			marker: "the build cache prune", want: "command terminated with exit code 2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engineFailureListing(t), engineRemoveAnswers{})
			replaceEngineReply(f, tc.match, tc.reply)
			f.tick()
			f.drain()
			logs := f.logs.String()
			if tc.kubectl != "" && strings.Contains(logs, tc.kubectl) {
				t.Errorf("kubectl's own line reached the log:\n%s", logs)
			}
			line := engineLogLine(logs, tc.marker)
			if line == "" {
				t.Fatalf("no failure line for %q in:\n%s", tc.marker, logs)
			}
			if got := engineStderrField(line); got != tc.want {
				t.Errorf("the failure line's stderr = %q, want %q:\n%s", got, tc.want, line)
			}
			if !strings.Contains(line, "exit status 1") {
				t.Errorf("the failure line does not name the exit status:\n%s", line)
			}
		})
	}
}

// TestCIEnginePrunePrunesAnonymousVolumesNamedWith64HexDigits: an anonymous volume is
// one docker names with 64 hex digits, and there is no companion rule: an unmounted
// one older than the age goes even beside a "-env" volume. A 32-hex name, a named
// volume, a mounted volume and a young one stay.
func TestCIEnginePrunePrunesAnonymousVolumesNamedWith64HexDigits(t *testing.T) {
	t.Parallel()
	const (
		anon    = "cec4c545e1d3f5d23ce7cbecb0e47bb61644a8eab4178730068a4712f4099b81"
		mounted = "e98ecf6d8a8b17d753845a8268105dc3dc7d56a0a6643e10cd2b3198f3a97969"
	)
	listing := engListing(t,
		[]map[string]any{engCtr('1', 'e', "running", 1*time.Hour, mounted)},
		nil,
		[]map[string]any{
			engVol(anon, 30*time.Hour),
			engVol(anon+"-env", 30*time.Hour),
			engVol(anon[:32], 30*time.Hour),
			engVol(mounted, 30*time.Hour),
			engVol("ci-npm", 400*time.Hour),
		},
	)
	plan, err := planCIEnginePrune(engDecode(t, listing), ciCacheTestStart, 48*time.Hour, 24*time.Hour, []string{"ci-base"})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if len(plan.volumes) != 1 || plan.volumes[0].Name != anon {
		t.Errorf("planned volumes = %+v, want only the unmounted 64-hex volume", plan.volumes)
	}
}

// TestCIEngineRemovalsAreOneDockerCommandPerKind: the remove step is docker commands
// over argv ids, one per kind the plan names, with no shell, loop, counter or
// conditional anywhere between the plan and docker; a kind with no ids is not sent.
func TestCIEngineRemovalsAreOneDockerCommandPerKind(t *testing.T) {
	t.Parallel()
	plan := ciEnginePlan{
		containers: []ciEngineContainer{{ID: engHex64('1')}, {ID: engHex64('2')}},
		images:     []ciEngineImage{{ID: engImageID('a')}},
	}
	got := ciEngineRemovals(plan)
	want := []ciEngineRemoval{
		{kind: "container", argv: []string{"docker", "rm", engHex64('1'), engHex64('2')}, ids: []string{engHex64('1'), engHex64('2')}},
		{kind: "image", argv: []string{"docker", "rmi", "-f", engImageID('a')}, ids: []string{engImageID('a')}},
	}
	if len(got) != len(want) {
		t.Fatalf("removals = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].kind != want[i].kind || !slices.Equal(got[i].argv, want[i].argv) || !slices.Equal(got[i].ids, want[i].ids) {
			t.Errorf("removal %d = %+v, want %+v", i, got[i], want[i])
		}
		for _, a := range got[i].argv {
			if strings.ContainsAny(a, "|&;$`<>()\\\"' ") {
				t.Errorf("removal %d argv word %q would mean something to a shell", i, a)
			}
		}
	}
	if len(ciEngineRemovals(ciEnginePlan{})) != 0 {
		t.Errorf("an empty plan still has removals")
	}
}

// TestCIEngineBuildCachePruneArgvUsesADuration: docker's until filter takes a duration,
// so the build cache prune spells its unused age as one: 6h for the default age,
// never a timestamp.
func TestCIEngineBuildCachePruneArgvUsesADuration(t *testing.T) {
	t.Parallel()
	for age, want := range map[time.Duration]string{
		6 * time.Hour:    "6h",
		24 * time.Hour:   "24h",
		48 * time.Hour:   "48h",
		90 * time.Minute: "90m",
	} {
		got := ciEngineBuildCachePruneArgv(age)
		if wantArgv := []string{"docker", "builder", "prune", "-af", "--filter", "until=" + want}; !slices.Equal(got, wantArgv) {
			t.Errorf("build cache prune argv for %v = %q, want %q", age, got, wantArgv)
		}
	}
}

// TestCIEnginePrunePlansByTheUnusedAgeItIsGiven: a stopped container and an unused
// anonymous volume past the unused age are planned, and both below it are kept; the
// image age does not decide either.
func TestCIEnginePrunePlansByTheUnusedAgeItIsGiven(t *testing.T) {
	t.Parallel()
	listing := engDecode(t, engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 7*time.Hour)},
		[]map[string]any{engImg('e', 7*time.Hour, []string{"sha256:layer-h"}, "gastown-daemon:ci-1")},
		[]map[string]any{engVol(engHex64('1'), 7*time.Hour)},
	))
	for unusedAge, want := range map[time.Duration]bool{
		6 * time.Hour: true,
		8 * time.Hour: false,
	} {
		plan, err := planCIEnginePrune(listing, ciCacheTestStart, 48*time.Hour, unusedAge, []string{"ci-base"})
		if err != nil {
			t.Fatalf("planning at %v: %v", unusedAge, err)
		}
		if got := len(plan.containers) == 1; got != want {
			t.Errorf("stopped container 7h old at unused age %v: planned = %v, want %v", unusedAge, got, want)
		}
		if got := len(plan.volumes) == 1; got != want {
			t.Errorf("anonymous volume 7h old at unused age %v: planned = %v, want %v", unusedAge, got, want)
		}
	}
}

// TestCIEnginePruneMeasuresAStoppedContainersAgeFromItsStop: a stopped container's
// unused age runs from when it stopped, not from when it was created, so a job that
// ran longer than the age and stopped a moment ago stays. A container in "created"
// state never ran, so its creation is the instant it became idle.
func TestCIEnginePruneMeasuresAStoppedContainersAgeFromItsStop(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status  string
		created time.Duration
		stopped time.Duration
		want    bool
	}{
		"a long job that just stopped stays":          {status: "exited", created: 10 * time.Hour, stopped: time.Minute, want: false},
		"a long job stopped past the age goes":        {status: "exited", created: 10 * time.Hour, stopped: 7 * time.Hour, want: true},
		"a created container past the age goes":       {status: "created", created: 7 * time.Hour, want: true},
		"a created container inside the age stays":    {status: "created", created: 5 * time.Hour, want: false},
		"a dead container long stopped past goes":     {status: "dead", created: 30 * time.Hour, stopped: 9 * time.Hour, want: true},
		"an ancient container stopped just now stays": {status: "exited", created: 200 * time.Hour, stopped: 2 * time.Minute, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			listing := engDecode(t, engListing(t,
				[]map[string]any{engCtrStopped('4', 'e', tc.status, tc.created, tc.stopped)},
				[]map[string]any{engImg('e', 2*time.Hour, []string{"sha256:layer-h"}, "gastown-daemon:ci-1")},
				nil,
			))
			plan, err := planCIEnginePrune(listing, ciCacheTestStart, 48*time.Hour, 6*time.Hour, []string{"ci-base"})
			if err != nil {
				t.Fatalf("planning: %v", err)
			}
			if got := len(plan.containers) == 1; got != tc.want {
				t.Errorf("stopped container %s created %v ago and stopped %v ago is planned = %v, want %v",
					tc.status, tc.created, tc.stopped, got, tc.want)
			}
		})
	}
}

// engineBuildCacheListing is one stopped container, its image and an anonymous volume,
// each old enough to go: a run that removes one of each, so its verdict line and its
// build cache line are both there to read.
func engineBuildCacheListing(t *testing.T) []byte {
	t.Helper()
	return engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 100*time.Hour)},
		[]map[string]any{engImg('e', 100*time.Hour, []string{"sha256:layer-h"}, "gastown-daemon:ci-1")},
		[]map[string]any{engVol(engHex64('1'), 30*time.Hour)},
	)
}

// TestCIEnginePruneBuildCacheTotalReadsDockersTotalLine: the reclaimed figure is the
// size on the Total line docker builder prune prints, in either builder's wording;
// output that names no size on a Total line is refused.
func TestCIEnginePruneBuildCacheTotalReadsDockersTotalLine(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		out     string
		want    int64
		refused bool
	}{
		"buildx total":        {out: "Deleted build cache objects:\nabc\n\nTotal:\t68.8GB\n", want: 68_800_000_000},
		"classic total":       {out: "Deleted build cache objects:\nabc\nTotal reclaimed space: 1.2GB\n", want: 1_200_000_000},
		"nothing reclaimed":   {out: "Total:\t0B\n", want: 0},
		"no total line":       {out: "Deleted build cache objects:\nabc\n", refused: true},
		"total with no size":  {out: "Total:\tlots\n", refused: true},
		"total with no colon": {out: "Total 5GB\n", refused: true},
		"empty output":        {out: "", refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ciEngineBuildCacheTotal([]byte(tc.out))
			if tc.refused {
				if err == nil {
					t.Fatalf("the total of %q = %d, want a refusal", tc.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("the total of %q: %v", tc.out, err)
			}
			if got != tc.want {
				t.Errorf("the total of %q = %d, want %d", tc.out, got, tc.want)
			}
		})
	}
}

// TestCIEnginePruneBuildCacheLogsReclaimedBytes: the verdict line names the bytes the
// build cache prune reclaimed, read from the Total line docker printed.
func TestCIEnginePruneBuildCacheLogsReclaimedBytes(t *testing.T) {
	t.Parallel()
	f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engineBuildCacheListing(t), engineRemoveAnswers{})
	replaceEngineReply(f, "-- docker builder prune", cliReply{stdout: "Deleted build cache objects:\nabc\nTotal:\t2GB\n"})
	f.tick()
	f.drain()

	line := engineLogLine(f.logs.String(), "removed 1 containers, 1 images, 1 volumes")
	if want := "; build cache reclaimed " + polecatimage.Human(2_000_000_000); !strings.Contains(line, want) {
		t.Errorf("the verdict line does not name the reclaimed build cache %q:\n%s", want, f.logs.String())
	}
}

// TestCIEnginePruneBuildCacheWarnsWhenNothingIsReclaimed: a build cache prune that
// reclaims 0 bytes logs one WARN naming the reclaimable figure docker reported before
// the prune, the filter, and that cache younger than the filter is not removed, when
// that figure is above zero. Every other reading of the two logs no WARN, and the run
// still reaches its verdict.
func TestCIEnginePruneBuildCacheWarnsWhenNothingIsReclaimed(t *testing.T) {
	t.Parallel()
	const reclaimable = 68_800_000_000
	for name, tc := range map[string]struct {
		df, prune cliReply
		warn      bool
	}{
		"none of the reclaimable cache is reclaimed": {
			df: cliReply{stdout: "Build Cache 88GB 68.8GB (100%)\n"}, prune: cliReply{stdout: "Total:\t0B\n"}, warn: true,
		},
		"nothing was reclaimable before the prune": {
			df: cliReply{stdout: "Build Cache 0B 0B (0%)\n"}, prune: cliReply{stdout: "Total:\t0B\n"},
		},
		"the prune reclaims some of the reclaimable cache": {
			df: cliReply{stdout: "Build Cache 88GB 68.8GB (100%)\n"}, prune: cliReply{stdout: "Total:\t1GB\n"},
		},
		"the size report names no build cache": {
			df: cliReply{stdout: "Images 3 1GB 0B\n"}, prune: cliReply{stdout: "Total:\t0B\n"},
		},
		"the Build Cache line names no reclaimable size": {
			df: cliReply{stdout: "Build Cache 88GB\n"}, prune: cliReply{stdout: "Total:\t0B\n"},
		},
		"the size read before the prune failed": {
			df: cliReply{code: 1, stderr: "Cannot connect to the Docker daemon"}, prune: cliReply{stdout: "Total:\t0B\n"},
		},
		"the prune's total is unreadable": {
			df: cliReply{stdout: "Build Cache 88GB 68.8GB (100%)\n"}, prune: cliReply{stdout: "Total: lots\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engineBuildCacheListing(t), engineRemoveAnswers{})
			replaceEngineReply(f, "-- docker system df", tc.df)
			replaceEngineReply(f, "-- docker builder prune", tc.prune)
			f.tick()
			f.drain()

			logs := f.logs.String()
			var warns []string
			for _, line := range strings.Split(logs, "\n") {
				if strings.Contains(line, "WARN") {
					warns = append(warns, line)
				}
			}
			if f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName).IsZero() {
				t.Errorf("the run did not reach its verdict:\n%s", logs)
			}
			if !tc.warn {
				if len(warns) != 0 {
					t.Errorf("a run that is not a zero reclaim logged a WARN:\n%s", logs)
				}
				return
			}
			if len(warns) != 1 {
				t.Fatalf("logged %d WARN line(s), want one:\n%s", len(warns), logs)
			}
			for _, want := range []string{polecatimage.Human(reclaimable), "reclaimed " + polecatimage.Human(0), "until=6h", "cache younger than the filter is not removed"} {
				if !strings.Contains(warns[0], want) {
					t.Errorf("the WARN line lacks %q:\n%s", want, warns[0])
				}
			}
		})
	}
}

// TestCIEnginePruneBuildCacheSizeReadFailureSkipsOnlyTheWarn: a size read before the
// prune that fails is logged once and skips only the zero-reclaim check. The build
// cache prune, the removals and the verdict all still run.
func TestCIEnginePruneBuildCacheSizeReadFailureSkipsOnlyTheWarn(t *testing.T) {
	t.Parallel()
	f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engineBuildCacheListing(t), engineRemoveAnswers{})
	replaceEngineReply(f, "-- docker system df", cliReply{code: 1, stderr: "Cannot connect to the Docker daemon"})
	replaceEngineReply(f, "-- docker builder prune", cliReply{stdout: "Total:\t0B\n"})
	f.tick()
	f.drain()

	logs := f.logs.String()
	if n := len(engineDockerExecs(f, "builder prune")); n != 1 {
		t.Errorf("the build cache prune ran %d time(s), want once", n)
	}
	if n := len(engineDockerExecs(f, "rm")); n != 1 {
		t.Errorf("the container removal ran %d time(s), want once", n)
	}
	if line := engineLogLine(logs, "removed 1 containers, 1 images, 1 volumes"); !strings.HasSuffix(line, "; build cache reclaimed 0.0B") {
		t.Errorf("the verdict line does not name the build cache reclaimed:\n%s", logs)
	}
	if line := engineLogLine(logs, "zero-reclaim check is skipped"); !strings.Contains(line, "exit status 1") {
		t.Errorf("no line names the failed size read and the skipped check:\n%s", logs)
	}
	if strings.Contains(logs, "WARN") {
		t.Errorf("a failed size read logged a WARN:\n%s", logs)
	}
	if f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName).IsZero() {
		t.Errorf("a failed size read left the engine prune without a verdict")
	}
}

// TestCIEnginePruneBuildCacheUnparseableTotalDoesNotFailTheRun: a Total line that does
// not read as a size is logged as docker's last line, and the run still reaches its
// verdict with every removal done and no failure line.
func TestCIEnginePruneBuildCacheUnparseableTotalDoesNotFailTheRun(t *testing.T) {
	t.Parallel()
	f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engineBuildCacheListing(t), engineRemoveAnswers{})
	replaceEngineReply(f, "-- docker builder prune", cliReply{stdout: "Total: lots\n"})
	f.tick()
	f.drain()

	logs := f.logs.String()
	if want := `build cache reclaimed unknown (docker's last line was "Total: lots")`; !strings.Contains(logs, want) {
		t.Errorf("the verdict line does not log docker's last line %q:\n%s", want, logs)
	}
	if !strings.Contains(logs, "removed 1 containers, 1 images, 1 volumes") {
		t.Errorf("the verdict line is missing:\n%s", logs)
	}
	if strings.Contains(logs, "failed") || strings.Contains(logs, "retried") {
		t.Errorf("an unreadable total failed the run:\n%s", logs)
	}
	if f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName).IsZero() {
		t.Errorf("an unreadable total left the engine prune without a verdict")
	}
}

// TestCIEnginePrunePodLookupFailureLogsNoStderr: a pod lookup that fails, whether
// kubectl exits non-zero or its reply does not decode, is logged with a fixed reason
// and nothing of its output, and nothing is pruned.
func TestCIEnginePrunePodLookupFailureLogsNoStderr(t *testing.T) {
	t.Parallel()
	const podOutput = "secret-engine-detail-7f3a"
	for name, reply := range map[string]cliReply{
		"pod lookup exec fails":     {code: 1, stderr: podOutput},
		"pod reply does not decode": {stdout: podOutput + " not json"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engListing(t, nil, nil, nil), engineRemoveAnswers{})
			answer := f.cli.answer
			f.cli.answer = func(c cliCall) cliReply {
				if strings.HasPrefix(cacheTrimVerb(c.args), "get pods ") {
					return reply
				}
				return answer(c)
			}
			f.tick()
			f.drain()
			// The cache trim shares this lookup and logs its own line from it, so only
			// the engine prune's lines are read here.
			var logs []string
			for _, line := range strings.Split(f.logs.String(), "\n") {
				if strings.Contains(line, "ci engine prune") {
					logs = append(logs, line)
				}
			}
			engineLogs := strings.Join(logs, "\n")
			if strings.Contains(engineLogs, "secret-engine-detail") {
				t.Errorf("the pod lookup's output reached the engine prune's log:\n%s", engineLogs)
			}
			if !strings.Contains(engineLogs, "pod lookup failed") {
				t.Errorf("the failed pod lookup is not logged by the engine prune:\n%s", engineLogs)
			}
			if n := len(engineExecs(f, "ci-engine-remove")); n != 0 {
				t.Errorf("a failed pod lookup ran the remove program %d time(s)", n)
			}
		})
	}
}

// TestCIEnginePruneRefusesASettingItCannotActOn: an explicit empty keep list, and a
// kubernetes runner with no context, are refused at the tick: the engine is not
// listed, nothing is pruned, and the refusal names what is wrong.
func TestCIEnginePruneRefusesASettingItCannotActOn(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		breakSetting func(f *ciCacheFixture)
		wantLog      string
	}{
		"explicit empty keep list": {
			breakSetting: func(f *ciCacheFixture) { f.d.patrolConfig.Patrols.DiskMaintenance.CIEngineKeepRepos = []string{} },
			wantLog:      "ci_engine_keep_repos",
		},
		"kubernetes runner with no context": {
			breakSetting: func(f *ciCacheFixture) { f.d.patrolConfig.Patrols.TierSweep.Runner.Context = "" },
			wantLog:      "names no control cluster",
		},
		"an unused age under the floor": {
			breakSetting: func(f *ciCacheFixture) {
				f.d.patrolConfig.Patrols.DiskMaintenance.CIEngineUnusedAge = "30m"
			},
			wantLog: "ci_engine_unused_age",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engListing(t, nil, nil, nil), engineRemoveAnswers{})
			tc.breakSetting(f)
			f.tick()
			f.drain()
			if n := len(engineExecs(f, "ci-engine-list")) + len(engineExecs(f, "ci-engine-remove")); n != 0 {
				t.Fatalf("a refused setting ran %d engine exec(s), want none", n)
			}
			if !strings.Contains(f.logs.String(), tc.wantLog) {
				t.Errorf("the refusal does not name %q:\n%s", tc.wantLog, f.logs.String())
			}
		})
	}
}

// TestCIEnginePruneSettingsNameTheSettingTheyRefuse: a setting the patrol cannot act
// on makes the settings read fail with an error naming that setting, never a default.
func TestCIEnginePruneSettingsNameTheSettingTheyRefuse(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		set     func(c *DiskMaintenanceConfig)
		wantKey string
	}{
		"an interval that is not a duration": {
			set:     func(c *DiskMaintenanceConfig) { c.CIEnginePruneInterval = "soon" },
			wantKey: "ci_engine_prune_interval",
		},
		"an age of zero": {
			set:     func(c *DiskMaintenanceConfig) { c.CIEnginePruneAge = "0h" },
			wantKey: "ci_engine_prune_age",
		},
		"an unused age under the floor": {
			set:     func(c *DiskMaintenanceConfig) { c.CIEngineUnusedAge = "30m" },
			wantKey: "ci_engine_unused_age",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := diskMaintEnabled("", agentconfig.DiskMaintenanceModeApply)
			tc.set(cfg.Patrols.DiskMaintenance)
			_, _, _, _, err := ciEnginePruneSettings(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("ciEnginePruneSettings() error = %v, want one naming %s", err, tc.wantKey)
			}
		})
	}
}

// TestCIEngineSizeAfterIsReadFromDu: the size after a removal is du's first field.
// A du that fails, prints nothing or prints a non-number is a failed run that says
// so, and the removals still ran. A du that exits 1 is a failure whatever its
// stdout, unless every stderr line is du's own vanishing-file report: a report cut
// short, kubectl's exit report with no du line beside it, and a line in that shape
// naming a status other than the exec's (or reached with none), are failures.
func TestCIEngineSizeAfterIsReadFromDu(t *testing.T) {
	t.Parallel()
	listing := engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 100*time.Hour)}, nil, nil)
	const root = ciEngineRoot
	for name, tc := range map[string]struct {
		remove  engineRemoveAnswers
		wantLog string
	}{
		"du fails":            {engineRemoveAnswers{duFail: true}, "du of the data root failed (exit status 1, stderr: du: cannot read)"},
		"du prints nothing":   {engineRemoveAnswers{duSet: true, duOut: "\n"}, "du printed no figure"},
		"du prints a non-KiB": {engineRemoveAnswers{duSet: true, duOut: "lots\t/var/lib/docker\n"}, `du printed "lots", not a KiB figure`},
		"du prints negative":  {engineRemoveAnswers{duSet: true, duOut: "-4\t/var/lib/docker\n"}, `du printed "-4", not a KiB figure`},
		"du exits 1 with a total and other stderr": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stdout: "10485760\t" + root + "\n", stderr: "du: " + root + "/volumes/aaa: Permission denied\n"}},
			"du of the data root failed (exit status 1, stderr: du: " + root + "/volumes/aaa: Permission denied)",
		},
		"du exits 1 with a total and no stderr": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stdout: "10485760\t" + root + "\n"}},
			"du of the data root failed (exit status 1)",
		},
		"du exits 1 with vanished files and no figure": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stderr: "du: " + root + "/volumes/aaa: No such file or directory\n"}},
			"du of the data root failed (exit status 1, stderr: du: " + root + "/volumes/aaa: No such file or directory)",
		},
		"du exits 1 with vanished files and a non-KiB": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stdout: "lots\n", stderr: "du: " + root + "/volumes/aaa: No such file or directory\n"}},
			"du of the data root failed (exit status 1, stderr: du: " + root + "/volumes/aaa: No such file or directory)",
		},
		"du exits 1 with another program's vanish line": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stdout: "10485760\t" + root + "\n", stderr: "sh: 1: ci-entrypoint: No such file or directory\n"}},
			"du of the data root failed (exit status 1, stderr: sh: 1: ci-entrypoint: No such file or directory)",
		},
		"du exits 1 with a vanish line and another program's": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stdout: "10485760\t" + root + "\n", stderr: "du: cannot access '" + root + "/volumes/aaa': No such file or directory\ncp: cannot stat '" + root + "/volumes/bbb': No such file or directory\n"}},
			"du of the data root failed (exit status 1, stderr: du: cannot access '" + root + "/volumes/aaa': No such file or directory)",
		},
		"du exits 1 with a first line cut short": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stdout: "10485760\t" + root + "\n", stderr: "du: " + root + "/volumes/aaa: No such file or direc"}},
			"du of the data root failed (exit status 1, stderr: du: " + root + "/volumes/aaa: No such file or direc)",
		},
		"du exits 1 with only kubectl's exit report": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stdout: "10485760\t" + root + "\n", stderr: "command terminated with exit code 1\n"}},
			"du of the data root failed (exit status 1)",
		},
		"du exits 1 with a line near kubectl's exit report": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stdout: "10485760\t" + root + "\n", stderr: "du: " + root + "/volumes/aaa: No such file or directory\ncommand terminated with exit code one\n"}},
			"du of the data root failed (exit status 1, stderr: du: " + root + "/volumes/aaa: No such file or directory)",
		},
		"du exits 1 with another program's exit report": {
			engineRemoveAnswers{duReply: &cliReply{code: 1, stdout: "10485760\t" + root + "\n", stderr: "du: " + root + "/volumes/aaa: No such file or directory\ncommand terminated with exit code 2\n"}},
			"du of the data root failed (exit status 1, stderr: du: " + root + "/volumes/aaa: No such file or directory)",
		},
		"a start failure with a line in the exit report's shape": {
			engineRemoveAnswers{duReply: &cliReply{startErr: errors.New("exec: executable file not found in $PATH"), stderr: "command terminated with exit code 1\n"}},
			"du of the data root failed (no exit status, stderr: command terminated with exit code 1)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, listing, tc.remove)
			f.tick()
			f.drain()
			logs := f.logs.String()
			if !strings.Contains(logs, "the data root's size after could not be read") || !strings.Contains(logs, tc.wantLog) {
				t.Errorf("the log does not name an unreadable size (%q):\n%s", tc.wantLog, logs)
			}
			if !f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName).IsZero() {
				t.Errorf("an unreadable size was recorded as a verdict")
			}
			if len(engineDockerExecs(f, "rm")) != 1 {
				t.Errorf("the container removal did not run before du")
			}
		})
	}
}

// TestCIEngineSizeAfterAcceptsAVanishedFile: a CI job's volume can be removed while
// du walks the data root, so du prints its total to stdout and exits 1 over the
// vanished paths, and kubectl adds its own report of that status to the same stderr.
// That figure is the size after, and the run reaches a verdict; kubectl's lines are
// not du's report, so none of them vetoes it.
func TestCIEngineSizeAfterAcceptsAVanishedFile(t *testing.T) {
	t.Parallel()
	const root = ciEngineRoot
	listing := engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 100*time.Hour)}, nil, nil)
	for name, stderr := range map[string]string{
		"two vanished volumes": "du: " + root + "/volumes/aaa: No such file or directory\n" +
			"du: " + root + "/volumes/bbb: No such file or directory\n",
		"du's own report wording": "du: cannot access '" + root + "/volumes/aaa': No such file or directory\n" +
			"du: cannot read directory '" + root + "/volumes/bbb': No such file or directory\n",
		"a kubectl warning alongside": "Warning: a transient kubectl line\n" +
			"du: " + root + "/volumes/aaa: No such file or directory\n",
		"many vanished volumes": strings.Repeat("du: "+root+"/volumes/aaa: No such file or directory\n", 40),
		"the exit report kubectl adds": "du: " + root + "/volumes/aaa: No such file or directory\n" +
			"du: " + root + "/volumes/bbb: No such file or directory\n" +
			"command terminated with exit code 1\n",
		"many vanished volumes and the exit report": strings.Repeat("du: "+root+"/volumes/aaa: No such file or directory\n", 40) +
			"command terminated with exit code 1\n",
		"kubectl's exit report ahead of du's": "command terminated with exit code 1\n" +
			"du: " + root + "/volumes/aaa: No such file or directory\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, listing, engineRemoveAnswers{
				duReply: &cliReply{code: 1, stdout: "8388608\t" + root + "\n", stderr: stderr},
			})
			f.tick()
			f.drain()
			logs := f.logs.String()
			if strings.Contains(logs, "the data root's size after could not be read") {
				t.Errorf("a vanished file made the size unreadable:\n%s", logs)
			}
			if !strings.Contains(logs, "40.0GB to 8.0GB") {
				t.Errorf("the verdict does not report du's total as the size after:\n%s", logs)
			}
			if f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName).IsZero() {
				t.Errorf("the vanished-file run reached no verdict:\n%s", logs)
			}
		})
	}
}

// TestCIEnginePresentReadsInspectOutput: the presence inspect prints the id (the
// name, for a volume) of each object found, one per line. A line not in that
// kind's shape names nothing. The image rows are captured from the runner's dind
// (docker 28, containerd image store): the present id alone on stdout, with the
// gone id's error on stderr, which the daemon never reads. No stopped container
// or anonymous volume existed to capture, so those rows use the documented shape,
// one id or name per line.
func TestCIEnginePresentReadsInspectOutput(t *testing.T) {
	t.Parallel()
	const present = "sha256:6f1314f5fe28a6de6f7ab8e1e562b450339e1b1720882b2dd4387f651bb2b35d"
	for name, tc := range map[string]struct {
		kind string
		out  string
		want []string
	}{
		"image, one present of two (captured)": {"image", present + "\n", []string{present}},
		"image, none present":                  {"image", "", nil},
		"image, a short id names nothing":      {"image", "sha256:6f13\n" + present + "\n", []string{present}},
		"image, error text names nothing":      {"image", "Error response from daemon: No such image: " + present + "\n", nil},
		"container, two present (documented)":  {"container", engHex64('1') + "\n" + engHex64('2') + "\n", []string{engHex64('1'), engHex64('2')}},
		"container, a name names nothing":      {"container", "boring_name\n", nil},
		"volume, a name per line (documented)": {"volume", "ci-gocache\n" + engHex64('1') + "\n", []string{"ci-gocache", engHex64('1')}},
		"volume, a line with a space":          {"volume", "bad name\n", nil},
		"unknown kind":                         {"network", present + "\n", nil},
	} {
		got := ciEnginePresent(tc.kind, []byte(tc.out))
		if len(got) != len(tc.want) {
			t.Errorf("%s: present = %v, want %v", name, got, tc.want)
			continue
		}
		for _, id := range tc.want {
			if !got[id] {
				t.Errorf("%s: %s is not reported present (%v)", name, id, got)
			}
		}
	}
}

// TestCIEnginePresenceArgv: the presence check is the kind's inspect over the
// missing ids, formatted to the id (the name for a volume), with no shell.
func TestCIEnginePresenceArgv(t *testing.T) {
	t.Parallel()
	for kind, want := range map[string][]string{
		"container": {"docker", "container", "inspect", "--format", "{{.Id}}", "a", "b"},
		"image":     {"docker", "image", "inspect", "--format", "{{.Id}}", "a", "b"},
		"volume":    {"docker", "volume", "inspect", "--format", "{{.Name}}", "a", "b"},
	} {
		got := ciEngineRemoval{kind: kind}.presenceArgv([]string{"a", "b"})
		if !slices.Equal(got, want) {
			t.Errorf("%s presence argv = %q, want %q", kind, got, want)
		}
	}
}
