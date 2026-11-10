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

// engCtr is a container inspect with its status, image and volume mounts.
func engCtr(id byte, image byte, status string, created time.Duration, volumes ...string) map[string]any {
	mounts := make([]map[string]any, 0, len(volumes))
	for _, v := range volumes {
		mounts = append(mounts, map[string]any{"Type": "volume", "Name": v})
	}
	return map[string]any{
		"Id":      engHex64(id),
		"Image":   engImageID(image),
		"Created": engAgo(created),
		"State":   map[string]any{"Status": status},
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
	plan, err := planCIEnginePrune(engineBaseListing(t), ciCacheTestStart, 48*time.Hour, []string{"ci-base", "org/tools"})
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
	plan, err := planCIEnginePrune(engDecode(t, engListing(t, nil, images, nil)), ciCacheTestStart, 48*time.Hour, []string{"ci-base"})
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
			plan, err := planCIEnginePrune(listing, ciCacheTestStart, 48*time.Hour, tc.keepRepos)
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
			if _, err := planCIEnginePrune(l, now, 48*time.Hour, []string{"ci-base"}); err == nil {
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
	// duOut replaces du's reply when duSet; duFail makes du exit 1.
	duOut  string
	duSet  bool
	duFail bool
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
			if remove.duFail {
				return cliReply{code: 1, stderr: "du: cannot read"}
			}
			if remove.duSet {
				return cliReply{stdout: remove.duOut}
			}
			return cliReply{stdout: "10485760\t" + ciEngineRoot + "\n"}
		case strings.Contains(verb, "-- docker builder prune"):
			return cliReply{stdout: "Total:\t0B\n"}
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
		cutoff := ciCacheTestStart.Add(-ciEngineUnusedAge).Format(time.RFC3339)
		prunes := engineDockerExecs(f, "builder prune")
		if len(prunes) != 1 || !slices.Contains(prunes[0].args, "until="+cutoff) {
			t.Errorf("build cache prune = %q, want one run with until=%s", prunes, cutoff)
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
			wantLog: "(the removal ended: exit status 1)",
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

// TestCIEnginePruneExecErrorLogsNoEngineStderr: a failed list or remove exec is
// logged with its exit status only. The engine's stderr can carry what the engine
// holds, so it reaches neither the log nor an attention item.
func TestCIEnginePruneExecErrorLogsNoEngineStderr(t *testing.T) {
	t.Parallel()
	const engineStderr = "Error response from daemon: secret-engine-detail-7f3a"
	listing := engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 100*time.Hour)},
		[]map[string]any{engImg('e', 100*time.Hour, []string{"sha256:layer-h"}, "gastown-daemon:ci-1")},
		[]map[string]any{engVol(engHex64('1'), 30*time.Hour)},
	)
	for name, tc := range map[string]struct {
		list   cliReply
		remove engineRemoveAnswers
	}{
		"list exec fails":   {list: cliReply{code: 1, stderr: engineStderr}},
		"remove exec fails": {remove: engineRemoveAnswers{fail: true, stderr: engineStderr}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, listing, tc.remove)
			if tc.list != (cliReply{}) {
				answer := f.cli.answer
				f.cli.answer = func(c cliCall) cliReply {
					if strings.Contains(cacheTrimVerb(c.args), "ci-engine-list") {
						return tc.list
					}
					return answer(c)
				}
			}
			f.tick()
			f.drain()
			logs := f.logs.String()
			if strings.Contains(logs, "secret-engine-detail") {
				t.Errorf("the engine's stderr reached the log:\n%s", logs)
			}
			if !strings.Contains(logs, "exit status 1") {
				t.Errorf("the failed exec is not logged with its exit status:\n%s", logs)
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
	plan, err := planCIEnginePrune(engDecode(t, listing), ciCacheTestStart, 48*time.Hour, []string{"ci-base"})
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
	if argv := ciEngineBuildCachePruneArgv("2026-11-09T00:00:00Z"); !slices.Equal(argv, []string{"docker", "builder", "prune", "-af", "--filter", "until=2026-11-09T00:00:00Z"}) {
		t.Errorf("build cache prune argv = %q", argv)
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
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := diskMaintEnabled("", agentconfig.DiskMaintenanceModeApply)
			tc.set(cfg.Patrols.DiskMaintenance)
			_, _, _, err := ciEnginePruneSettings(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("ciEnginePruneSettings() error = %v, want one naming %s", err, tc.wantKey)
			}
		})
	}
}

// TestCIEngineSizeAfterIsReadFromDu: the size after a removal is du's first field.
// A du that fails, prints nothing or prints a non-number is a failed run that says
// so, and the removals still ran.
func TestCIEngineSizeAfterIsReadFromDu(t *testing.T) {
	t.Parallel()
	listing := engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 100*time.Hour)}, nil, nil)
	for name, tc := range map[string]struct {
		remove  engineRemoveAnswers
		wantLog string
	}{
		"du fails":            {engineRemoveAnswers{duFail: true}, "du of the data root failed (exit status 1)"},
		"du prints nothing":   {engineRemoveAnswers{duSet: true, duOut: "\n"}, "du printed no figure"},
		"du prints a non-KiB": {engineRemoveAnswers{duSet: true, duOut: "lots\t/var/lib/docker\n"}, `du printed "lots", not a KiB figure`},
		"du prints negative":  {engineRemoveAnswers{duSet: true, duOut: "-4\t/var/lib/docker\n"}, `du printed "-4", not a KiB figure`},
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
