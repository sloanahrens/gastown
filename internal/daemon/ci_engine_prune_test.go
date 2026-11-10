package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// The engine prune's fixtures are hand-built listings: each container, image and
// volume is the map docker inspect would print, with only the fields the planner
// reads. Ids are full-length and hex, as docker prints them.

// engHex64 is a container id, or the digest part of an image id.
func engHex64(c byte) string { return strings.Repeat(string(c), 64) }

// engImageID is an image id, as docker prints it.
func engImageID(c byte) string { return "sha256:" + engHex64(c) }

// engHex32 is an anonymous volume's name.
func engHex32(c byte) string { return strings.Repeat(string(c), 32) }

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
		engCtr('1', '2', "running", 1*time.Hour, engHex32('0')),
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
		engVol(engHex32('0'), 30*time.Hour),
		// Anonymous, unused and older than the age: removed.
		engVol(engHex32('1'), 30*time.Hour),
		// Anonymous with an -env companion: kept as a pair.
		engVol(engHex32('2'), 30*time.Hour),
		engVol(engHex32('2')+"-env", 30*time.Hour),
		// Anonymous and too new: kept.
		engVol(engHex32('3'), 1*time.Hour),
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
	if len(plan.volumes) != 1 || plan.volumes[0].Name != engHex32('1') {
		t.Errorf("planned volumes = %+v, want only the unused anonymous volume", plan.volumes)
	}

	// A child goes before its parent: the removal order is most layers first.
	for i := 1; i < len(plan.images); i++ {
		if len(plan.images[i].RootFS.Layers) > len(plan.images[i-1].RootFS.Layers) {
			t.Errorf("images planned %d before %d: a parent would go before its child", i-1, i)
		}
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
			l.Volumes = append(l.Volumes, ciEngineVolume{Name: engHex32('4')})
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
func newCIEngineFixture(t *testing.T, mode string, listing []byte, remove cliReply) *ciCacheFixture {
	t.Helper()
	f := newCICacheFixture(t, mode)
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
		case strings.Contains(verb, "ci-engine-remove"):
			return remove
		}
		return cliReply{}
	}
	return f
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
		[]map[string]any{engVol(engHex32('1'), 30*time.Hour)},
	), cliReply{})
	f.tick()
	f.drain()

	if n := len(engineExecs(f, "ci-engine-remove")); n != 0 {
		t.Fatalf("a dry run ran the remove program %d time(s)", n)
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

// TestCIEnginePruneApplyRemovesThePlannedIds: an apply passes exactly the planned
// ids and names as argv data, and its reply is the verdict; a reply that names
// another count is a failed run that is tried again soon.
func TestCIEnginePruneApplyRemovesThePlannedIds(t *testing.T) {
	t.Parallel()
	listing := engListing(t,
		[]map[string]any{engCtr('4', 'e', "exited", 100*time.Hour)},
		[]map[string]any{engImg('e', 100*time.Hour, []string{"sha256:layer-h"}, "gastown-daemon:ci-1")},
		[]map[string]any{engVol(engHex32('1'), 30*time.Hour)},
	)

	t.Run("the planned ids reach the remove program, and its reply is the verdict", func(t *testing.T) {
		t.Parallel()
		f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, listing, cliReply{stdout: "before_kib=41943040\nremoved=3\nafter_kib=10485760\n"})
		f.tick()
		f.drain()
		removes := engineExecs(f, "ci-engine-remove")
		if len(removes) != 1 {
			t.Fatalf("the remove program ran %d time(s), want once", len(removes))
		}
		argv := strings.Join(removes[0].args, "\x00")
		for _, want := range []string{engHex64('4'), engImageID('e'), engHex32('1')} {
			if !strings.Contains(argv, want) {
				t.Errorf("the remove exec's argv lacks %s", want)
			}
		}
		if f.d.diskMaintSchedule.lastRunAt(ciEnginePruneName).IsZero() {
			t.Errorf("a verdict left the engine prune unscheduled")
		}
	})

	// A remove that fails, or whose reply cannot be read or disagrees with the plan,
	// is a failed run: no verdict, a retry soon, and a line that says why.
	for name, tc := range map[string]struct {
		remove  cliReply
		wantLog string
	}{
		"a reply naming another count": {
			remove:  cliReply{stdout: "before_kib=1\nremoved=7\nafter_kib=1\n"},
			wantLog: "the plan named 3",
		},
		"a reply that cannot be read": {
			remove:  cliReply{stdout: "garbage\n"},
			wantLog: "could not be read",
		},
		"a remove exec that fails": {
			remove:  cliReply{code: 1, stderr: "volume is in use"},
			wantLog: "remove exec",
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
		})
	}
}

// TestParseCIEnginePruneReply: the remove program's reply is exactly its three
// figures, in order; anything else is an error, never zeros.
func TestParseCIEnginePruneReply(t *testing.T) {
	t.Parallel()
	good, err := parseCIEnginePruneReply([]byte("before_kib=41943040\nremoved=4\nafter_kib=10485760\n"))
	if err != nil {
		t.Fatalf("a well-formed reply does not parse: %v", err)
	}
	if good.beforeKiB != 41943040 || good.removed != 4 || good.afterKiB != 10485760 {
		t.Errorf("parsed %+v, want before 41943040, removed 4, after 10485760", good)
	}
	for name, out := range map[string]string{
		"empty":             "",
		"no removed line":   "before_kib=5\nafter_kib=3\n",
		"keys out of order": "removed=2\nbefore_kib=5\nafter_kib=3\n",
		"extra line":        "before_kib=5\nremoved=2\nafter_kib=3\nnoise\n",
		"not a number":      "before_kib=5\nremoved=two\nafter_kib=3\n",
		"negative":          "before_kib=5\nremoved=-1\nafter_kib=3\n",
	} {
		if _, err := parseCIEnginePruneReply([]byte(out)); err == nil {
			t.Errorf("%s reply parsed without an error", name)
		}
	}
}

// TestCIEnginePruneFailsWhenTheEngineIsNotRunning: a runner pod whose dind
// container is not running is a failed run: nothing is listed or removed, and the
// run is tried again soon rather than read as an engine with nothing to prune.
func TestCIEnginePruneFailsWhenTheEngineIsNotRunning(t *testing.T) {
	t.Parallel()
	f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engListing(t, nil, nil, nil), cliReply{})
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
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engListing(t, nil, nil, nil), cliReply{})
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
			f := newCIEngineFixture(t, agentconfig.DiskMaintenanceModeApply, engListing(t, nil, nil, nil), cliReply{})
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
