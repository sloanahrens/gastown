package daemon

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

// sweepRunsPruneFixture builds an entry whose ModTime is now minus age, so a
// row reads as the ages the planner weighs.
func sweepRunsPruneFixture(now time.Time, name string, age time.Duration, bytes int64) sweepRunEntry {
	return sweepRunEntry{Name: name, ModTime: now.Add(-age), Bytes: bytes}
}

// TestSweepRunsPrunePlan is the planner's table: names removed, names skipped,
// and the byte total, for listings that mix the age rule, the keepNewest rule
// and names the sweep did not write.
func TestSweepRunsPrunePlan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	const minAge = 72 * time.Hour

	tests := []struct {
		name string
		// keep is keepNewest; the row says it so the cut is visible.
		keep        int
		entries     []sweepRunEntry
		wantRemove  []string
		wantSkipped []string
	}{
		{
			// The young entry takes the newest slot it earns, and the two old
			// entries under the cut are kept by it alone: without the
			// keepNewest rule they would be listed.
			name: "newest kept, young kept, the rest removed",
			keep: 3,
			entries: []sweepRunEntry{
				sweepRunsPruneFixture(now, "gastown-11111111-shell", 10*time.Hour, 5),
				sweepRunsPruneFixture(now, "gastown-22222222-shell", 100*time.Hour, 10),
				sweepRunsPruneFixture(now, "gastown-33333333-integration-race", 200*time.Hour, 20),
				sweepRunsPruneFixture(now, "gastown-44444444-shell", 300*time.Hour, 30),
				sweepRunsPruneFixture(now, "gastown-55555555-shell", 400*time.Hour, 40),
			},
			wantRemove: []string{"gastown-44444444-shell", "gastown-55555555-shell"},
		},
		{
			// Age alone keeps the young entry: keepNewest 0 keeps nothing by
			// count, so the young one survives on its age and the old one is
			// listed.
			name: "young kept even when it is not among the newest",
			keep: 0,
			entries: []sweepRunEntry{
				sweepRunsPruneFixture(now, "gastown-11111111-shell", time.Hour, 5),
				sweepRunsPruneFixture(now, "gastown-22222222-shell", 400*time.Hour, 40),
			},
			wantRemove: []string{"gastown-22222222-shell"},
		},
		{
			name: "names the sweep did not write are skipped, never listed",
			keep: 0,
			entries: []sweepRunEntry{
				sweepRunsPruneFixture(now, "gastown-abcdef12-shell", 400*time.Hour, 7),
				sweepRunsPruneFixture(now, "not-a-run", 400*time.Hour, 1),
				sweepRunsPruneFixture(now, "gastown-deadbeef", 400*time.Hour, 1),
				sweepRunsPruneFixture(now, "gastown-deadbeef-shellx", 400*time.Hour, 1),
				sweepRunsPruneFixture(now, "gastown-xyz-shell", 400*time.Hour, 1),
				sweepRunsPruneFixture(now, "gastown-deadbeef-shell-shell", 400*time.Hour, 1),
				sweepRunsPruneFixture(now, "deadbeef-shell", 400*time.Hour, 1),
				sweepRunsPruneFixture(now, "gastown-DEADBEEF-shell", 400*time.Hour, 1),
				sweepRunsPruneFixture(now, "gastown-0123456789-shell", 400*time.Hour, 1),
				sweepRunsPruneFixture(now, "gastown--deadbeef-shell", 400*time.Hour, 1),
			},
			wantRemove: []string{"gastown-abcdef12-shell"},
			wantSkipped: []string{
				"not-a-run",
				"gastown-deadbeef",
				"gastown-deadbeef-shellx",
				"gastown-xyz-shell",
				"gastown-deadbeef-shell-shell",
				"deadbeef-shell",
				"gastown-DEADBEEF-shell",
				"gastown-0123456789-shell",
				"gastown--deadbeef-shell",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := planSweepRunsPrune(tc.entries, now, tc.keep, minAge)

			var gotRemove []string
			for _, removal := range plan.Remove {
				gotRemove = append(gotRemove, removal.Name)
				if removal.Reason == "" {
					t.Errorf("%s is listed with no reason", removal.Name)
				}
			}
			if !slices.Equal(gotRemove, tc.wantRemove) {
				t.Errorf("Remove = %v, want %v", gotRemove, tc.wantRemove)
			}
			if !slices.Equal(plan.Skipped, tc.wantSkipped) {
				t.Errorf("Skipped = %v, want %v", plan.Skipped, tc.wantSkipped)
			}

			// Bytes is exactly the sum of the entries the plan lists, so a
			// skipped or kept directory never counts toward what a prune frees.
			var wantBytes int64
			for _, removal := range plan.Remove {
				index := slices.IndexFunc(tc.entries, func(e sweepRunEntry) bool { return e.Name == removal.Name })
				if index < 0 {
					t.Fatalf("Remove names %q, which was not an entry", removal.Name)
				}
				if removal.Bytes != tc.entries[index].Bytes {
					t.Errorf("%s: Bytes = %d, want the entry's %d", removal.Name, removal.Bytes, tc.entries[index].Bytes)
				}
				wantBytes += tc.entries[index].Bytes
			}
			if plan.Bytes != wantBytes {
				t.Errorf("Bytes = %d, want the sum of the listed entries %d", plan.Bytes, wantBytes)
			}
		})
	}
}

// TestSweepRunsPrunePlanKeepsZeroModTime holds an entry whose ModTime nobody
// recorded as never removed, and shows it takes no keepNewest place: with the
// zero entry dropped from the count both old entries would be over a cut of
// one and listed.
func TestSweepRunsPrunePlanKeepsZeroModTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	entries := []sweepRunEntry{
		{Name: "gastown-11111111-shell", Bytes: 5},
		sweepRunsPruneFixture(now, "gastown-22222222-shell", 100*time.Hour, 10),
		sweepRunsPruneFixture(now, "gastown-33333333-shell", 200*time.Hour, 20),
	}

	plan := planSweepRunsPrune(entries, now, 2, 72*time.Hour)

	if len(plan.Remove) != 0 {
		t.Errorf("Remove = %v, want nothing: the zero ModTime is kept and the two old entries fill the newest two", removalsOf(plan))
	}
	if len(plan.Skipped) != 0 {
		t.Errorf("Skipped = %v, want nothing", plan.Skipped)
	}
}

// TestSweepRunsPrunePlanMinAgeBoundary holds an entry exactly minAge old
// outside the age rule: "younger than minAge" excludes it, so it is listed.
// The entry one second inside the bound is kept by age alone, because the
// younger entry takes the only keepNewest place.
func TestSweepRunsPrunePlanMinAgeBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	const minAge = 72 * time.Hour
	entries := []sweepRunEntry{
		sweepRunsPruneFixture(now, "gastown-11111111-shell", minAge, 10),
		sweepRunsPruneFixture(now, "gastown-22222222-shell", minAge-time.Second, 20),
		sweepRunsPruneFixture(now, "gastown-33333333-shell", time.Hour, 30),
	}

	plan := planSweepRunsPrune(entries, now, 1, minAge)

	if got, want := removalsOf(plan), []string{"gastown-11111111-shell"}; !slices.Equal(got, want) {
		t.Errorf("Remove = %v, want %v", got, want)
	}
}

// TestSweepRunsPrunePlanKeepsATieAtTheCut holds every entry tied at the
// keepNewest-th newest time: keeping a directory too many is the safe
// direction, so a cut that cannot separate them keeps them all.
func TestSweepRunsPrunePlanKeepsATieAtTheCut(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	tied := now.Add(-24 * time.Hour)
	entries := []sweepRunEntry{
		{Name: "gastown-11111111-shell", ModTime: tied, Bytes: 10},
		{Name: "gastown-22222222-shell", ModTime: tied, Bytes: 20},
		{Name: "gastown-33333333-shell", ModTime: tied, Bytes: 30},
	}

	plan := planSweepRunsPrune(entries, now, 2, time.Hour)

	if len(plan.Remove) != 0 {
		t.Errorf("Remove = %v, want nothing: all three tie at the cut", removalsOf(plan))
	}
}

// TestSweepRunsPrunePlanMatchesProducerNames ties the matcher to the name the
// sweep actually writes: tierSweepRunnerRunID builds the directory name, so a
// change to how it joins the rig, the commit and the tiers turns this red
// before the planner silently skips every real directory.
func TestSweepRunsPrunePlanMatchesProducerNames(t *testing.T) {
	t.Parallel()
	const sha = "4f8767457c937ffa02295da08122494e9364eeb9"
	// Every stage the sweep runs, named by the producer itself, so a stage
	// that gains or renames a tier turns this red here rather than silently
	// making the planner skip that stage's real directories.
	var names []string
	for _, stage := range tierSweepStages("gastown") {
		names = append(names, tierSweepRunnerRunID(stage.rig, sha, stage.tiers))
	}
	names = append(names, tierSweepRunnerRunID("a-rig-with-hyphens", sha, []string{tiersweep.TierShell}))

	for _, name := range names {
		if !isSweepRunName(name) {
			t.Errorf("isSweepRunName(%q) = false, but tierSweepRunnerRunID wrote it", name)
		}
	}

	// The matcher must not accept a tier token the sweep never runs.
	foreign := fmt.Sprintf("gastown-%s-weekly", sha[:8])
	if isSweepRunName(foreign) {
		t.Errorf("isSweepRunName(%q) = true, but the sweep runs no weekly tier", foreign)
	}
}

// removalsOf lists the names a plan removes, so a failure message states what
// the planner chose.
func removalsOf(plan sweepRunsPlan) []string {
	names := make([]string, 0, len(plan.Remove))
	for _, removal := range plan.Remove {
		names = append(names, removal.Name)
	}
	return names
}
