package cachevol

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the package under the hermetic harness; the planners read nothing outside their inputs.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// base is the fixed clock every test reads from: nothing here calls time.Now.
var base = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func at(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }

// trimNow is the clock the plans are judged by, a day after base: every entry the file helper plants
// at an hour from base is past the build-cache minimum age by it.
var trimNow = at(24)

const sha = "0123456789abcdef0123456789abcdef01234567"

// TestPlanSeedListsTheLinuxStepsForEachPin: each pin gets the four seed commands in the Dockerfile's
// order, go mod download and go build required; no step names darwin or a GOOS other than linux.
func TestPlanSeedListsTheLinuxStepsForEachPin(t *testing.T) {
	t.Parallel()
	got, err := PlanSeed([]Pin{{Rig: "devops", Commit: sha}, {Rig: "beads", Commit: sha}})
	if err != nil {
		t.Fatalf("PlanSeed: %v", err)
	}
	if len(got) != 2 || got[0].Rig != "devops" || got[1].Rig != "beads" {
		t.Fatalf("pins in order = %+v", got)
	}
	want := [][]string{
		{"go", "mod", "download"},
		{"go", "build", "./..."},
		{"go", "vet", "./..."},
		{"go", "test", "-count=1", "-run", "^$", "./..."},
	}
	for _, rig := range got {
		var argvs [][]string
		for i, s := range rig.Steps {
			argvs = append(argvs, s.Argv)
			if s.Required != (i < 2) {
				t.Errorf("%s step %v Required = %v, want %v", rig.Rig, s.Argv, s.Required, i < 2)
			}
			if joined := strings.Join(s.Argv, " "); strings.Contains(joined, "darwin") {
				t.Errorf("%s has a darwin step: %q", rig.Rig, joined)
			}
		}
		if !reflect.DeepEqual(argvs, want) {
			t.Errorf("%s steps = %v, want %v", rig.Rig, argvs, want)
		}
	}
}

// TestPlanSeedRefusesAPinItCannotRead: a pin with no rig, a commit that is not a full sha, or a rig
// named twice stops the plan, so a seed never runs from a file it cannot read whole.
func TestPlanSeedRefusesAPinItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pins []Pin
	}{
		{"no rig", []Pin{{Rig: "", Commit: sha}}},
		{"a short commit", []Pin{{Rig: "devops", Commit: "abc123"}}},
		{"an upper-case commit", []Pin{{Rig: "devops", Commit: strings.ToUpper(sha)}}},
		{"a rig named twice", []Pin{{Rig: "devops", Commit: sha}, {Rig: "devops", Commit: sha}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := PlanSeed(tc.pins); err == nil {
				t.Errorf("PlanSeed(%+v) = %+v, want a refusal", tc.pins, got)
			}
		})
	}
}

// file is a listing entry with a size and times in hours after base.
func file(path string, c Cache, size int64, atimeH, mtimeH int) Entry {
	return Entry{Path: path, Cache: c, Size: size, Atime: at(atimeH), Mtime: at(mtimeH)}
}

const (
	actionOne     = "1111111111111111111111111111111111111111111111111111111111111111"
	actionTwo     = "3333333333333333333333333333333333333333333333333333333333333333"
	outputID      = "2222222222222222222222222222222222222222222222222222222222222222"
	actionPath    = "gocache/11/" + actionOne + "-a"
	actionTwoPath = "gocache/33/" + actionTwo + "-a"
	outputPath    = "gocache/22/" + outputID + "-d"
)

// action is a build-cache -a entry whose header names output, with a size and times in hours after
// base.
func action(path, output string, size int64, atimeH, mtimeH int) Entry {
	e := file(path, BuildCache, size, atimeH, mtimeH)
	e.Output = output
	return e
}

// actionHeader is the start of an -a file as Go writes it: version, action id, output id, size and
// time, each field after the version followed by a space (cmd/go/internal/cache putIndexEntry).
func actionHeader(actionID, outID string) []byte {
	return []byte("v1 " + actionID + " " + outID + " " + fmt.Sprintf("%20d", 10) + " " + fmt.Sprintf("%20d", 1<<30) + "\n")
}

func removedPaths(p TrimPlan) []string {
	var out []string
	for _, r := range p.Removals {
		out = append(out, r.Path)
	}
	return out
}

// planTrim plans listing under cap, failing the test when the planner refuses an input it accepts.
func planTrim(t *testing.T, listing []Entry, cap int64, sandboxesRunning bool, now time.Time) TrimPlan {
	t.Helper()
	plan, err := PlanTrim(listing, cap, sandboxesRunning, now)
	if err != nil {
		t.Fatalf("PlanTrim: %v", err)
	}
	return plan
}

// TestPlanTrimRemovesBuildCacheLeastRecentlyUsedFirst: build-cache files go oldest first until the
// claim is under the cap, and the module cache is not touched while the build cache can reach it.
func TestPlanTrimRemovesBuildCacheLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()
	listing := []Entry{
		file("gocache/new", BuildCache, 40, 9, 9),
		file("gocache/old", BuildCache, 40, 1, 1),
		file("gocache/mid", BuildCache, 40, 5, 5),
		file("gomodcache/dep", ModuleCache, 40, 0, 0),
	}
	plan := planTrim(t, listing, 120, false, trimNow)
	if got, want := removedPaths(plan), []string{"gocache/old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("removals = %v, want %v (the oldest build file, and only it, reaches the cap)", got, want)
	}
	if plan.Overflow {
		t.Error("a trimmable claim reported overflow")
	}
}

// TestPlanTrimTakesTheLaterOfAtimeAndMtime: an entry whose mtime is recent is recent, even with an old
// atime, because Go touches the mtime of a reused entry.
func TestPlanTrimTakesTheLaterOfAtimeAndMtime(t *testing.T) {
	t.Parallel()
	listing := []Entry{
		file("gocache/reused", BuildCache, 50, 1, 8),
		file("gocache/idle", BuildCache, 50, 5, 5),
	}
	plan := planTrim(t, listing, 50, false, trimNow)
	if got, want := removedPaths(plan), []string{"gocache/idle"}; !reflect.DeepEqual(got, want) {
		t.Errorf("removals = %v, want %v: the reused entry's mtime makes it the newer one", got, want)
	}
}

// TestPlanTrimTouchesTheModuleCacheOnlyWhenIdle pins each case of the module-cache rule under the
// overseer's default: a module-cache-only overflow with sandboxes running is reported, not trimmed.
func TestPlanTrimTouchesTheModuleCacheOnlyWhenIdle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		listing       []Entry
		capBytes      int64
		sandboxesRun  bool
		wantRemovals  []string
		wantOverflow  bool
		wantOverBytes int64
	}{
		{
			name:     "under the cap: nothing is removed",
			listing:  []Entry{file("gocache/a", BuildCache, 10, 1, 1), file("gomodcache/b", ModuleCache, 10, 1, 1)},
			capBytes: 100,
		},
		{
			name:         "trimmable from the build cache alone: the module cache is left",
			listing:      []Entry{file("gocache/a", BuildCache, 80, 1, 1), file("gomodcache/b", ModuleCache, 10, 1, 1)},
			capBytes:     50,
			wantRemovals: []string{"gocache/a"},
		},
		{
			name:          "module-cache-only overflow with sandboxes running: not touched, reported",
			listing:       []Entry{file("gomodcache/dep", ModuleCache, 80, 1, 1)},
			capBytes:      50,
			sandboxesRun:  true,
			wantOverflow:  true,
			wantOverBytes: 30,
		},
		{
			name:         "module-cache-only overflow with sandboxes idle: trimmed to the cap",
			listing:      []Entry{file("gomodcache/old", ModuleCache, 40, 1, 1), file("gomodcache/new", ModuleCache, 40, 9, 9)},
			capBytes:     50,
			wantRemovals: []string{"gomodcache/old"},
		},
		{
			name: "build cache cannot reach the cap, sandboxes running: module cache not touched",
			listing: []Entry{
				file("gocache/a", BuildCache, 20, 1, 1),
				file("gomodcache/dep", ModuleCache, 60, 1, 1),
			},
			capBytes:      50,
			sandboxesRun:  true,
			wantRemovals:  []string{"gocache/a"},
			wantOverflow:  true,
			wantOverBytes: 10,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := planTrim(t, tc.listing, tc.capBytes, tc.sandboxesRun, trimNow)
			if got := removedPaths(plan); !reflect.DeepEqual(got, tc.wantRemovals) && !(len(got) == 0 && len(tc.wantRemovals) == 0) {
				t.Errorf("removals = %v, want %v", got, tc.wantRemovals)
			}
			if plan.Overflow != tc.wantOverflow || plan.OverBytes != tc.wantOverBytes {
				t.Errorf("overflow = %v over %d, want %v over %d", plan.Overflow, plan.OverBytes, tc.wantOverflow, tc.wantOverBytes)
			}
		})
	}
}

// TestPlanTrimKeepsBuildFilesYoungerThanTheMinimumAge: a claim whose only over-cap build-cache files
// are younger than the minimum age loses nothing, and the trim reports how far over the cap it
// leaves the claim (gt-enplmw).
func TestPlanTrimKeepsBuildFilesYoungerThanTheMinimumAge(t *testing.T) {
	t.Parallel()
	// Both files were last used half an hour before the clock reads.
	listing := []Entry{
		file("gocache/young-a", BuildCache, 60, 24, 24),
		file("gocache/young-b", BuildCache, 60, 24, 24),
	}
	now := at(24).Add(30 * time.Minute)
	plan := planTrim(t, listing, 50, false, now)
	if got := removedPaths(plan); len(got) != 0 {
		t.Errorf("removals = %v, want none: every candidate is inside the minimum age", got)
	}
	if !plan.Overflow || plan.OverBytes != 70 {
		t.Errorf("overflow = %v over %d, want true over 70 (120 bytes against a 50-byte cap)", plan.Overflow, plan.OverBytes)
	}
}

// TestPlanTrimFloorIsTheGoTouchIntervalPlusMargin: the floor protects a file a gate used, which Go's
// hourly mtime touch can leave almost an hour older than the use. With the claim over its cap, a
// file used 74 minutes before the trim is kept and one used 76 minutes before is planned.
func TestPlanTrimFloorIsTheGoTouchIntervalPlusMargin(t *testing.T) {
	t.Parallel()
	now := at(24)
	tests := []struct {
		name     string
		used     time.Time
		wantGone bool
	}{
		{name: "74 minutes old is inside the floor", used: now.Add(-74 * time.Minute), wantGone: false},
		{name: "76 minutes old is past the floor", used: now.Add(-76 * time.Minute), wantGone: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			listing := []Entry{{Path: "gocache/tested", Cache: BuildCache, Size: 60, Atime: tc.used, Mtime: tc.used}}
			plan := planTrim(t, listing, 50, false, now)
			got := removedPaths(plan)
			if removed := len(got) == 1; removed != tc.wantGone {
				t.Errorf("removed = %v, want the file removed = %v (used %v before the trim)", got, tc.wantGone, now.Sub(tc.used))
			}
			wantOverBytes := int64(10)
			if tc.wantGone {
				wantOverBytes = 0
			}
			if plan.OverBytes != wantOverBytes {
				t.Errorf("overflow = %v over %d, want %d (60 bytes against a 50-byte cap, the file %s)", plan.Overflow, plan.OverBytes, wantOverBytes, map[bool]string{true: "removed", false: "kept"}[tc.wantGone])
			}
		})
	}
}

// TestBuildCacheMinAgeCoversTheGoTouchInterval: the floor is Go's touch interval plus the gate
// margin, and the margin keeps every file a gate that started within the last fifteen minutes uses.
func TestBuildCacheMinAgeCoversTheGoTouchInterval(t *testing.T) {
	t.Parallel()
	if buildCacheMinAge != goCacheTouchInterval+gateMargin {
		t.Errorf("buildCacheMinAge = %v, want goCacheTouchInterval %v plus gateMargin %v", buildCacheMinAge, goCacheTouchInterval, gateMargin)
	}
	if gateMargin < 15*time.Minute {
		t.Errorf("gateMargin = %v, want at least 15 minutes", gateMargin)
	}
}

// TestPlanTrimJudgesTheAgeByTheLaterOfAtimeAndMtime: the age is the later of atime and mtime, so a
// stale access time never makes a file Go touched look old, and a recent atime protects a file whose
// mtime is old (gt-enplmw).
func TestPlanTrimJudgesTheAgeByTheLaterOfAtimeAndMtime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		entry    Entry
		wantGone bool
	}{
		{
			// The mount left the atime a day stale; Go's touch on the mtime is the use that counts.
			name:     "a stale atime over a recent mtime is young",
			entry:    file("gocache/touched", BuildCache, 100, 0, 24),
			wantGone: false,
		},
		{
			// A recent atime protects a file whose mtime is old: the age is the later of the two.
			name:     "a recent atime over an old mtime is young",
			entry:    file("gocache/read", BuildCache, 100, 24, 0),
			wantGone: false,
		},
		{
			name:     "both times old is removable",
			entry:    file("gocache/idle", BuildCache, 100, 1, 1),
			wantGone: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := planTrim(t, []Entry{tc.entry}, 50, false, at(24))
			if got := len(removedPaths(plan)) == 1; got != tc.wantGone {
				t.Errorf("removed = %v, want %v (entry last used %v over at %v)", got, tc.wantGone, tc.entry.lastUsed(), at(24))
			}
		})
	}
}

// TestPlanTrimKeepsAFileWithNoTimes: an entry whose times are both unset has no age to judge, so it
// is kept rather than read as the oldest file in the claim.
func TestPlanTrimKeepsAFileWithNoTimes(t *testing.T) {
	t.Parallel()
	listing := []Entry{{Path: "gocache/unknown", Cache: BuildCache, Size: 100}}
	plan := planTrim(t, listing, 50, false, at(24))
	if got := removedPaths(plan); len(got) != 0 {
		t.Errorf("removals = %v, want none: a file with no times has no age to judge", got)
	}
	if !plan.Overflow || plan.OverBytes != 50 {
		t.Errorf("overflow = %v over %d, want true over 50", plan.Overflow, plan.OverBytes)
	}
}

// TestPlanTrimRefusesAZeroClock: the zero time is not a clock a trim may judge ages by. By it no
// file is old enough to remove, so the planner refuses it rather than returning a plan that reads as
// a claim with nothing old in it.
func TestPlanTrimRefusesAZeroClock(t *testing.T) {
	t.Parallel()
	listing := []Entry{file("gocache/old", BuildCache, 100, 1, 1)}
	if plan, err := PlanTrim(listing, 50, false, time.Time{}); err == nil {
		t.Errorf("PlanTrim with the zero time = %+v, want a refusal", plan)
	}
}

// TestPlanTrimDoesNotTrimTheModuleCacheWhenTheAgeFloorStopsTheBuildPass: a build pass the floor
// stopped leaves build-cache files in the claim, so the build cache has not said it cannot reach
// the cap and the module cache is not the next resort. It still goes when the build pass ran out of
// files, as it did before the floor (gt-enplmw).
func TestPlanTrimDoesNotTrimTheModuleCacheWhenTheAgeFloorStopsTheBuildPass(t *testing.T) {
	t.Parallel()
	aDay := at(24)
	tests := []struct {
		name         string
		listing      []Entry
		wantRemovals []string
		wantOverflow bool
	}{
		{
			name: "every build file is young: the module cache stays too",
			listing: []Entry{
				file("gocache/young", BuildCache, 60, 24, 24),
				file("gomodcache/dep", ModuleCache, 60, 1, 1),
			},
			wantOverflow: true,
		},
		{
			name: "an old build file goes, then a young one stops the pass: the module cache stays",
			listing: []Entry{
				file("gocache/old", BuildCache, 60, 1, 1),
				file("gocache/young", BuildCache, 60, 24, 24),
				file("gomodcache/dep", ModuleCache, 60, 1, 1),
			},
			wantRemovals: []string{"gocache/old"},
			wantOverflow: true,
		},
		{
			// No build file is held back by the floor, so the build cache has run out and the
			// module cache is the next resort, as it was before the floor existed.
			name: "the build pass runs out of files: the module cache goes, as before",
			listing: []Entry{
				file("gocache/old", BuildCache, 60, 1, 1),
				file("gomodcache/dep", ModuleCache, 60, 1, 1),
			},
			wantRemovals: []string{"gocache/old", "gomodcache/dep"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := planTrim(t, tc.listing, 50, false, aDay)
			if got := removedPaths(plan); !reflect.DeepEqual(got, tc.wantRemovals) && !(len(got) == 0 && len(tc.wantRemovals) == 0) {
				t.Errorf("removals = %v, want %v", got, tc.wantRemovals)
			}
			if plan.Overflow != tc.wantOverflow {
				t.Errorf("overflow = %v over %d, want %v", plan.Overflow, plan.OverBytes, tc.wantOverflow)
			}
		})
	}
}

// TestPlanTrimRemovesAnActionFileWithTheOutputItNames: a removal that takes the -a file takes the -d
// file its header names in the same plan, so no removal leaves an index naming a file that is gone
// (gt-udrvfw). Each case is a cap the -a file alone reaches: a planner that did not pair would stop
// after the -a and leave the -d behind.
func TestPlanTrimRemovesAnActionFileWithTheOutputItNames(t *testing.T) {
	t.Parallel()
	listing := []Entry{
		action(actionPath, outputPath, 90, 1, 1),
		file(outputPath, BuildCache, 10, 5, 5),
	}
	plan := planTrim(t, listing, 50, false, trimNow)
	if got, want := removedPaths(plan), []string{actionPath, outputPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("removals = %v, want %v: the -a file's removal takes the -d file it names", got, want)
	}
	if plan.Overflow {
		t.Errorf("overflow = %v over %d, want the pair's removal to bring the claim under the cap", plan.Overflow, plan.OverBytes)
	}
}

// TestPlanTrimRemovesAnOutputWithEveryActionFileThatNamesIt: a removal that takes a -d file takes
// every -a file whose header names it, in the same plan, since a build reads any of them (gt-udrvfw).
// The cap here is reached by the -d file alone, so a planner that did not pair would leave both -a
// files dangling.
func TestPlanTrimRemovesAnOutputWithEveryActionFileThatNamesIt(t *testing.T) {
	t.Parallel()
	listing := []Entry{
		file(outputPath, BuildCache, 90, 1, 1),
		action(actionPath, outputPath, 10, 2, 2),
		action(actionTwoPath, outputPath, 10, 3, 3),
	}
	plan := planTrim(t, listing, 50, false, trimNow)
	if got, want := removedPaths(plan), []string{actionPath, outputPath, actionTwoPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("removals = %v, want %v: the -d file's removal takes every -a file that names it", got, want)
	}
}

// TestPlanTrimCountsTheBytesOfAPairAndItsActionsOnce: a -d file that two -a files name leaves the
// claim once, so after the unit goes the claim is still 20 bytes over a 90-byte cap and Overflow says
// so; a planner that subtracted the -d file once per naming -a would report no overflow.
func TestPlanTrimCountsTheBytesOfAPairAndItsActionsOnce(t *testing.T) {
	t.Parallel()
	listing := []Entry{
		file(outputPath, BuildCache, 45, 1, 1),
		action(actionPath, outputPath, 45, 1, 1),
		file("gocache/young", BuildCache, 110, 24, 24),
	}
	plan := planTrim(t, listing, 90, false, trimNow)
	if got, want := removedPaths(plan), []string{actionPath, outputPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("removals = %v, want %v", got, want)
	}
	if !plan.Overflow || plan.OverBytes != 20 {
		t.Errorf("overflow = %v over %d, want true over 20 (200 bytes against a 90-byte cap, the 90-byte unit removed)",
			plan.Overflow, plan.OverBytes)
	}
}

// TestPlanTrimPlansALoneHalfAsBefore: an -a file whose -d file the listing does not hold, and a -d
// file no listed -a file names, are each a unit of one, planned by their own age and bytes as they
// were before the pairing existed.
func TestPlanTrimPlansALoneHalfAsBefore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		listing []Entry
		want    []string
	}{
		{
			name: "an -a file whose output is not in the listing",
			listing: []Entry{
				action(actionPath, "gocache/22/"+outputID+"-d", 90, 1, 1),
				file("gocache/other", BuildCache, 10, 2, 2),
			},
			want: []string{actionPath},
		},
		{
			name: "a -d file no listed -a file names",
			listing: []Entry{
				file(outputPath, BuildCache, 90, 1, 1),
				file("gocache/other", BuildCache, 10, 2, 2),
			},
			want: []string{outputPath},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := planTrim(t, tc.listing, 50, false, trimNow)
			if got := removedPaths(plan); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("removals = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlanTrimKeepsAUnitWhoseActionFileIsYoung: a unit goes whole or not at all, so a young -a file
// keeps its old -d file in the claim even while the claim is over its cap; removing the -d alone
// would leave the young index naming a file that is gone (gt-udrvfw).
func TestPlanTrimKeepsAUnitWhoseActionFileIsYoung(t *testing.T) {
	t.Parallel()
	listing := []Entry{
		action(actionPath, outputPath, 90, 24, 24),
		file(outputPath, BuildCache, 90, 1, 1),
	}
	plan := planTrim(t, listing, 50, false, trimNow)
	if got := removedPaths(plan); len(got) != 0 {
		t.Errorf("removals = %v, want none: the unit's youngest file is inside the minimum age", got)
	}
	if !plan.Overflow || plan.OverBytes != 130 {
		t.Errorf("overflow = %v over %d, want true over 130 (180 bytes against a 50-byte cap)", plan.Overflow, plan.OverBytes)
	}
}

// TestIsBuildActionFile: only Go's action-file name is read for a header, so a file named *-a
// anywhere else in the tree is left unpaired rather than parsed.
func TestIsBuildActionFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{actionPath, true},
		{outputPath, false},
		{"gocache/11/" + actionOne, false},
		{SeedBuildCacheDir + "/11/" + strings.Repeat("A", 64) + "-a", false},
		{SeedModuleCacheDir + "/11/" + actionOne + "-a", false},
		{SeedBuildCacheDir + "/1/" + actionOne + "-a", false},
		{SeedBuildCacheDir + "/11/" + actionOne[:63] + "-a", false},
		{SeedBuildCacheDir + "/11/" + actionOne + "-a/exe", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isBuildActionFile(tc.path); got != tc.want {
			t.Errorf("isBuildActionFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestBuildActionOutput reads the output path out of an -a header the way Go does: the second id in
// "v1 <action id> <output id> <size> <time>", at most as strict as Go's own check, so a header this
// leaves unpaired is one Go reads as a cache miss and never a live entry.
func TestBuildActionOutput(t *testing.T) {
	t.Parallel()
	upper := strings.ToUpper(outputID)
	tests := []struct {
		name   string
		path   string
		header []byte
		want   string
		ok     bool
	}{
		{"the header Go writes", actionPath, actionHeader(actionOne, outputID), outputPath, true},
		{"an upper-case id Go decodes", actionPath, actionHeader(strings.ToUpper(actionOne), upper), outputPath, true},
		{"another version", actionPath, []byte("v2 " + actionOne + " " + outputID + " "), "", false},
		{"no version", actionPath, actionHeader(actionOne, outputID)[3:], "", false},
		{"an action id that is not the file's", actionPath, actionHeader(actionTwo, outputID), "", false},
		{"a short header", actionPath, []byte("v1 " + actionOne + " "), "", false},
		{"an empty header", actionPath, nil, "", false},
		{"a non-hex id", actionPath, actionHeader(actionOne, strings.Repeat("z", 64)), "", false},
		{"a header cut before its size", actionPath, []byte("v1 " + actionOne + " " + outputID), "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := buildActionOutput(tc.path, tc.header)
			if ok != tc.ok || got != tc.want {
				t.Errorf("buildActionOutput = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// readerFor opens the bytes b, as a listing's open of a cache file would.
func readerFor(b []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

// TestBuildActionOutputFrom: the listing's read of an action file's header pairs the file with its
// output; a file that is gone, is not an action file, or holds a header Go would miss is paired with
// nothing; and a file that cannot be read is an error, since its -d file would be removed without it
// (gt-udrvfw).
func TestBuildActionOutputFrom(t *testing.T) {
	t.Parallel()
	header := actionHeader(actionOne, outputID)
	tests := []struct {
		name    string
		path    string
		open    func() (io.ReadCloser, error)
		want    string
		wantErr bool
	}{
		{"the header Go writes", actionPath, readerFor(header), outputPath, false},
		{"a file that is not an action file", outputPath, readerFor(header), "", false},
		{"a file this run removed", actionPath, func() (io.ReadCloser, error) { return nil, fs.ErrNotExist }, "", false},
		{"a file that cannot be opened", actionPath, func() (io.ReadCloser, error) { return nil, errors.New("open failed") }, "", true},
		{"a header that cannot be read", actionPath, func() (io.ReadCloser, error) { return io.NopCloser(failingReader{}), nil }, "", true},
		{"a header Go would read as a miss", actionPath, readerFor(nil), "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := BuildActionOutputFrom(tc.path, tc.open)
			if (err != nil) != tc.wantErr {
				t.Fatalf("BuildActionOutputFrom error = %v, want an error of %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("BuildActionOutputFrom = %q, want %q", got, tc.want)
			}
		})
	}
}

// failingReader is a reader whose every read fails, standing for a cache file the listing cannot
// read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

// TestReadBuildActionHeader: a short file is what a file Go is still writing gives and Go reads as a
// cache miss, so it is not an error; a read that fails is returned, since the listing then cannot say
// what the entry names.
func TestReadBuildActionHeader(t *testing.T) {
	t.Parallel()
	short := []byte("v1 ")
	got, err := readBuildActionHeader(strings.NewReader(string(short)))
	if err != nil {
		t.Fatalf("readBuildActionHeader on a short file: %v", err)
	}
	if string(got) != string(short) {
		t.Errorf("readBuildActionHeader = %q, want the %d bytes the file holds", got, len(short))
	}
	full := actionHeader(actionOne, outputID)
	got, err = readBuildActionHeader(bytes.NewReader(full))
	if err != nil {
		t.Fatalf("readBuildActionHeader on a whole header: %v", err)
	}
	if !bytes.Equal(got, full[:buildActionHeaderLen]) {
		t.Errorf("readBuildActionHeader = %q, want the header's first %d bytes", got, buildActionHeaderLen)
	}
	if _, err := readBuildActionHeader(failingReader{}); err == nil {
		t.Error("readBuildActionHeader read through a failing reader")
	}
}
