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
	plan := PlanTrim(listing, 120, false)
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
	plan := PlanTrim(listing, 50, false)
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
			plan := PlanTrim(tc.listing, tc.capBytes, tc.sandboxesRun)
			if got := removedPaths(plan); !reflect.DeepEqual(got, tc.wantRemovals) && !(len(got) == 0 && len(tc.wantRemovals) == 0) {
				t.Errorf("removals = %v, want %v", got, tc.wantRemovals)
			}
			if plan.Overflow != tc.wantOverflow || plan.OverBytes != tc.wantOverBytes {
				t.Errorf("overflow = %v over %d, want %v over %d", plan.Overflow, plan.OverBytes, tc.wantOverflow, tc.wantOverBytes)
			}
		})
	}
}
