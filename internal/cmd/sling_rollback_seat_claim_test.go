package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// seedSeatAgentBead writes the agent bead of the seat rigName/polecatName,
// with hookBead in its hook_bead field, into the fixture's work database.
func (f *rollbackFixture) seedSeatAgentBead(t *testing.T, rigName, polecatName, hookBead, state string) {
	t.Helper()
	prefix := beads.GetPrefixForRig(f.r.townRoot, rigName)
	f.work.Seed(beads.Issue{
		ID:   beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName),
		Type: "agent",
		Description: "role_type: polecat\nrig: " + rigName +
			"\nagent_state: " + state + "\nhook_bead: " + hookBead + "\n",
	})
}

// TestSlingRollbackLeavesASeatAnotherSlingHolds: the agent bead a spawn writes
// names the bead that spawn was for. When it names a different bead, another
// sling has claimed the seat since, so the deferred sling's rollback must not
// release the claim, reset the slot or remove the sandbox and branch under it
// (gt-7izlwj).
func TestSlingRollbackLeavesASeatAnotherSlingHolds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		hookBead string // the bead the spawn record says this sling spawned for
		beadID   string // the rollback's own bead, used when hookBead is unknown
	}{
		{name: "deferred bead known", hookBead: "gt-ug7zed", beadID: "gt-ug7zed"},
		{name: "deferred bead unknown", hookBead: "", beadID: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel := &fakeWorkReleaser{beads: map[string][2]string{}}
			f := newRollbackFixture(t, nil, rel)
			f.seedSeatAgentBead(t, "gastown", "Toast", "gt-wlrgk8", "spawning")
			var lines []string
			f.r.log = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

			f.r.cleanupSpawned(&SpawnedPolecatInfo{
				RigName: "gastown", PolecatName: "Toast", HookBead: tc.hookBead,
				Branch: "polecat/Toast/gt-ug7zed", FreshSpawn: true, BranchCreated: true,
			}, "gastown", tc.beadID, "")

			if f.seats != 0 {
				t.Errorf("released the seat claim of a seat another sling holds: %d", f.seats)
			}
			if len(f.sb.removed) != 0 || len(f.sb.branches) != 0 {
				t.Errorf("removed the sandbox or branch of another sling's seat: sandbox %v, branches %v", f.sb.removed, f.sb.branches)
			}
			if len(rel.resets) != 0 {
				t.Errorf("reset the slot of another sling's seat: %v", rel.resets)
			}
			if slices.Contains(rel.released, "gt-ug7zed") {
				t.Errorf("released a bead from a seat another sling holds: %v", rel.released)
			}
			if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "gt-wlrgk8") }) {
				t.Errorf("no line names the bead the seat now holds (gt-wlrgk8): %q", lines)
			}
		})
	}
}

// TestSlingRollbackUndoesTheSeatThisSlingHolds: the agent bead naming the bead
// this sling spawned for is the seat this sling still holds, so the rollback
// undoes it as it always has: the claim is released, a fresh spawn's sandbox
// and created branch are removed, and a reused seat's slot is reset.
func TestSlingRollbackUndoesTheSeatThisSlingHolds(t *testing.T) {
	t.Parallel()
	const agent = "gastown/polecats/Toast"
	cases := []struct {
		name         string
		hookBead     string // the spawn record's bead; "" reads the rollback's own
		fresh        bool
		noAgentBead  bool // the seat has no agent bead at all
		wantRemoved  bool
		wantBranchRm bool
		wantReset    bool
	}{
		{name: "fresh spawn: claim and created branch removed", hookBead: "", fresh: true, wantRemoved: true, wantBranchRm: true},
		{name: "reused seat: claim and slot reset", hookBead: "gt-ug7zed", fresh: false, wantReset: true},
		// A seat with no agent bead names no other claimant, so the rollback
		// keeps none of the reserved steps (gt-7izlwj).
		{name: "no agent bead: claim and created branch removed", fresh: true, noAgentBead: true, wantRemoved: true, wantBranchRm: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel := &fakeWorkReleaser{beads: map[string][2]string{"gt-ug7zed": {"hooked", agent}}}
			f := newRollbackFixture(t, nil, rel)
			if !tc.noAgentBead {
				f.seedSeatAgentBead(t, "gastown", "Toast", "gt-ug7zed", "spawning")
			}

			f.r.cleanupSpawned(&SpawnedPolecatInfo{
				RigName: "gastown", PolecatName: "Toast", HookBead: tc.hookBead,
				Branch: "polecat/Toast/gt-ug7zed", FreshSpawn: tc.fresh, BranchCreated: true,
			}, "gastown", "gt-ug7zed", "")

			if f.seats != 1 {
				t.Errorf("seat claims released = %d, want 1", f.seats)
			}
			if got := len(f.sb.removed) == 1; got != tc.wantRemoved {
				t.Errorf("sandbox removed = %v, want %v", f.sb.removed, tc.wantRemoved)
			}
			if got := len(f.sb.branches) == 1; got != tc.wantBranchRm {
				t.Errorf("branch delete = %v, want %v", f.sb.branches, tc.wantBranchRm)
			}
			if got := len(rel.resets) == 1; got != tc.wantReset {
				t.Errorf("slot reset = %v, want %v", rel.resets, tc.wantReset)
			}
			if !slices.Contains(rel.released, "gt-ug7zed") {
				t.Errorf("work bead released = %v, want gt-ug7zed", rel.released)
			}
		})
	}
}

// TestSlingRollbackKeepsTheSlotAndBranchWhenTheHolderIsUnknown: an agent bead
// that cannot be read, or a rollback with no town root to read it in, does not
// say who holds the seat, so the rollback leaves the slot and the branch and
// warns (gt-7izlwj). The seat claim still goes back, because no session will
// ever exist for this spawn (gt-t8q5).
func TestSlingRollbackKeepsTheSlotAndBranchWhenTheHolderIsUnknown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(f *rollbackFixture)
	}{
		{name: "the agent bead cannot be read", setup: func(f *rollbackFixture) {
			f.work.FailWith("show", errors.New("dolt unreachable"))
		}},
		{name: "the town root cannot be found", setup: func(f *rollbackFixture) {
			f.r.townErr = errors.New("not in a town")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel := &fakeWorkReleaser{beads: map[string][2]string{}}
			f := newRollbackFixture(t, nil, rel)
			f.seedSeatAgentBead(t, "gastown", "Toast", "gt-ug7zed", "spawning")
			tc.setup(f)
			var lines []string
			f.r.log = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

			f.r.cleanupSpawned(&SpawnedPolecatInfo{
				RigName: "gastown", PolecatName: "Toast", HookBead: "gt-ug7zed",
				Branch: "polecat/Toast/gt-ug7zed", FreshSpawn: true, BranchCreated: true,
			}, "gastown", "", "")

			if f.seats != 1 {
				t.Errorf("seat claims released = %d, want 1: no session will ever exist for this spawn (gt-t8q5)", f.seats)
			}
			if len(f.sb.removed) != 0 || len(f.sb.branches) != 0 {
				t.Errorf("removed the sandbox or branch without reading who holds the seat: sandbox %v, branches %v", f.sb.removed, f.sb.branches)
			}
			if len(rel.resets) != 0 {
				t.Errorf("reset the slot without reading who holds the seat: %v", rel.resets)
			}
			if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "agent bead") }) {
				t.Errorf("the unknown holder was not warned about: %q", lines)
			}
		})
	}
}
