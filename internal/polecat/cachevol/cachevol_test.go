package cachevol

import (
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
