package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// rollbackFixture is a slingRollback over a temp town with fake work and
// town databases and in-memory work release and sandbox. Nothing it does
// starts a process or reads the cwd, the environment or a package global.
type rollbackFixture struct {
	r     slingRollback
	work  *beadsfake.Fake
	town  *beadsfake.Fake
	rel   *fakeWorkReleaser
	sb    *fakeSandbox
	seats int
}

// newRollbackFixture builds a fixture whose work database holds bead (nil:
// an open gt-abc).
func newRollbackFixture(t *testing.T, bead *beads.Issue, rel *fakeWorkReleaser) *rollbackFixture {
	t.Helper()
	if bead == nil {
		bead = &beads.Issue{ID: "gt-abc"}
	}
	if rel == nil {
		rel = &fakeWorkReleaser{beads: map[string][2]string{}}
	}
	f := &rollbackFixture{work: beadsfake.New(), town: beadsfake.New(beadsfake.WithPrefix("hq")), rel: rel, sb: &fakeSandbox{}}
	f.work.Seed(*bead)
	townRoot := t.TempDir()
	stores := fakeSlingStores(f.work)
	f.r = slingRollback{
		townRoot:         townRoot,
		stores:           stores,
		townBeads:        f.town,
		getBeadInfo:      func(id string) (*beadInfo, error) { return stores.beadInfo(townRoot, id) },
		collectMolecules: collectExistingMolecules,
		burnMolecules:    stores.burnMolecules,
		releaseSeat:      func() { f.seats++ },
		newReleaser:      func(string, string) polecatWorkReleaser { return f.rel },
		survivingWork:    func(string, string) (string, error) { return "", nil },
		openSandbox:      func(string, string) (spawnedPolecatSandbox, error) { return f.sb, nil },
	}
	return f
}

// desc is id's description in the work database.
func (f *rollbackFixture) desc(t *testing.T, id string) string {
	t.Helper()
	is, err := f.work.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	return is.Description
}

const rawReviewDesc = "attached_at: 2026-06-30T12:00:00Z\nno_merge: true\nreview_only: true\ndispatched_by: mayor/\n\nKeep this body."

// TestSlingRollbackClearsRawReviewOnlyMetadata: a failed raw sling leaves no
// no_merge/review_only marks behind, and keeps the rest of the description.
func TestSlingRollbackClearsRawReviewOnlyMetadata(t *testing.T) {
	t.Parallel()
	f := newRollbackFixture(t, &beads.Issue{ID: "gt-rawrollback", Description: rawReviewDesc}, nil)

	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "toast"}, "gt-rawrollback", "", "")

	desc := f.desc(t, "gt-rawrollback")
	assertNoRawReviewMetadata(t, desc)
	for _, keep := range []string{"dispatched_by: mayor/", "Keep this body."} {
		if !strings.Contains(desc, keep) {
			t.Errorf("rollback lost %q:\n%s", keep, desc)
		}
	}
}

// TestSlingRollbackBurnsAttachedMolecules: molecules attached by a partial
// formula instantiation are burned in the town the rollback runs in.
func TestSlingRollbackBurnsAttachedMolecules(t *testing.T) {
	t.Parallel()
	f := newRollbackFixture(t, nil, nil)
	f.r.getBeadInfo = func(id string) (*beadInfo, error) {
		return &beadInfo{Description: "attached_molecule: gt-wisp-stale", Dependencies: []beads.IssueDep{{ID: "gt-wisp-stale"}}}, nil
	}
	var burned []string
	var burnTown string
	f.r.burnMolecules = func(m []string, id, townRoot string) error {
		burned, burnTown = append(burned, m...), townRoot
		return nil
	}

	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast"}, "gt-abc", "", "")

	if strings.Join(burned, ",") != "gt-wisp-stale" || burnTown != f.r.townRoot {
		t.Fatalf("burned %v in %q, want gt-wisp-stale in %q", burned, burnTown, f.r.townRoot)
	}
}

// TestSlingRollbackKeepsMetadataWhenMoleculeBurnFails: a failed burn leaves
// the attached molecule and the raw marks, so the next sling still sees them.
func TestSlingRollbackKeepsMetadataWhenMoleculeBurnFails(t *testing.T) {
	t.Parallel()
	initial := "attached_molecule: gt-wisp-stale\n" + rawReviewDesc
	f := newRollbackFixture(t, &beads.Issue{ID: "gt-rawrollback", Description: initial}, nil)
	f.r.collectMolecules = func(*beadInfo) []string { return []string{"gt-wisp-stale"} }
	f.r.burnMolecules = func([]string, string, string) error { return errors.New("forced burn failure") }

	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "toast"}, "gt-rawrollback", "", "")

	desc := f.desc(t, "gt-rawrollback")
	if !strings.Contains(desc, "attached_molecule: gt-wisp-stale") {
		t.Fatalf("rollback hid the attached molecule after a failed burn:\n%s", desc)
	}
	assertHasRawReviewMetadata(t, desc)
}

// TestSlingRollbackClearsRawMetadataAfterMoleculeBurnSucceeds: after a burn
// the bead is read again, and the raw marks are cleared from what the burn
// left, without bringing the molecule back.
func TestSlingRollbackClearsRawMetadataAfterMoleculeBurnSucceeds(t *testing.T) {
	t.Parallel()
	f := newRollbackFixture(t, &beads.Issue{ID: "gt-rawrollback", Description: "attached_molecule: gt-wisp-stale\n" + rawReviewDesc}, nil)
	f.r.collectMolecules = func(*beadInfo) []string { return []string{"gt-wisp-stale"} }
	f.r.burnMolecules = func(m []string, _, _ string) error {
		desc := rawReviewDesc // the burn detached the molecule
		return f.work.Update("gt-rawrollback", beads.UpdateOptions{Description: &desc})
	}

	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "toast"}, "gt-rawrollback", "", "")

	desc := f.desc(t, "gt-rawrollback")
	assertNoRawReviewMetadata(t, desc)
	if strings.Contains(desc, "attached_molecule: gt-wisp-stale") || !strings.Contains(desc, "Keep this body.") {
		t.Fatalf("description after rollback:\n%s", desc)
	}
}

// convoy creates an open auto-convoy in the fixture's town database.
func (f *rollbackFixture) convoy(t *testing.T) string {
	t.Helper()
	c, err := f.town.Create(beads.CreateOptions{Title: "auto-convoy", Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func (f *rollbackFixture) status(t *testing.T, id string) (string, string) {
	t.Helper()
	is, err := f.town.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	return is.Status, is.CloseReason
}

// TestSlingRollbackClosesOnlyAGivenConvoy: the auto-convoy is closed in the
// town beads with the rollback reason, and no convoy means no close.
func TestSlingRollbackClosesOnlyAGivenConvoy(t *testing.T) {
	t.Parallel()
	f := newRollbackFixture(t, nil, nil)
	id := f.convoy(t)
	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", FreshSpawn: true}, "gt-abc", "", id)
	if status, reason := f.status(t, id); status != "closed" || reason != "Sling rollback - hook failed" {
		t.Errorf("convoy %s: status %q reason %q, want closed with the rollback reason", id, status, reason)
	}

	f = newRollbackFixture(t, nil, nil)
	id = f.convoy(t)
	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", FreshSpawn: true}, "gt-abc", "", "")
	if status, _ := f.status(t, id); status != "open" {
		t.Errorf("a rollback with no convoy closed %s (status %q)", id, status)
	}
}

// TestSlingRollbackWithoutASpawnReleasesTheSeatOnly: a failure before any
// polecat was spawned gives back the seat claim and touches no sandbox.
func TestSlingRollbackWithoutASpawnReleasesTheSeatOnly(t *testing.T) {
	t.Parallel()
	f := newRollbackFixture(t, nil, nil)
	f.r.rollback(nil, "", "", "")
	if f.seats != 1 || len(f.sb.removed)+len(f.sb.branches)+len(f.rel.resets) != 0 {
		t.Fatalf("seats %d, removed %v, branches %v, resets %v", f.seats, f.sb.removed, f.sb.branches, f.rel.resets)
	}
}

// TestSlingRollbackCleansUpTheSpawnedPolecat: with a spawn, rollback goes on
// to undo it (seat, sandbox, created branch).
func TestSlingRollbackCleansUpTheSpawnedPolecat(t *testing.T) {
	t.Parallel()
	f := newRollbackFixture(t, nil, nil)
	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "Toast", Branch: "p-toast-123", FreshSpawn: true, BranchCreated: true}, "", "", "")
	if f.seats != 1 || strings.Join(f.sb.removed, ",") != "Toast" || strings.Join(f.sb.branches, ",") != "p-toast-123" {
		t.Fatalf("seats %d, removed %v, branches %v", f.seats, f.sb.removed, f.sb.branches)
	}
}

// TestCloseConvoyReportsAFailedCloseWithoutPanicking: bd refusing the close
// is a warning in a best-effort rollback, and outside a town nothing runs.
func TestCloseConvoyReportsAFailedCloseWithoutPanicking(t *testing.T) {
	t.Parallel()
	f := newRollbackFixture(t, nil, nil)
	f.r.closeConvoy("hq-cv-gone", "Sling rollback - hook failed")

	outside := newRollbackFixture(t, nil, nil)
	id := outside.convoy(t)
	outside.r.townErr = errors.New("not in a Gas Town workspace")
	outside.r.closeConvoy(id, "reason")
	if status, _ := outside.status(t, id); status != "open" {
		t.Fatalf("closed a convoy with no town: status %q", status)
	}
}
