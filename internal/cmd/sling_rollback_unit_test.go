package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// mutableBead is one bead held by an in-process bd: show reads it, update
// writes --description, --status and --assignee, as the mutable shell stub
// these tests replaced did through files.
type mutableBead struct {
	mu                         sync.Mutex
	id, desc, status, assignee string
}

func (m *mutableBead) description() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.desc
}

func (m *mutableBead) setDescription(d string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desc = d
}

// mutableBD is an in-process bd over one bead. Every call is logged as
// "<cmd> <args...>"; sql answers nothing, and any other command succeeds
// silently.
func mutableBD(b *mutableBead) *inprocBD {
	return &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		switch cmd {
		case "show":
			b.mu.Lock()
			defer b.mu.Unlock()
			out, _ := json.Marshal([]map[string]any{{
				"id": b.id, "title": "Test issue", "status": b.status,
				"assignee": b.assignee, "description": b.desc, "dependencies": []any{},
			}})
			return bdOut(string(out))
		case "update":
			b.mu.Lock()
			defer b.mu.Unlock()
			for _, a := range args {
				switch {
				case strings.HasPrefix(a, "--description="):
					b.desc = strings.TrimPrefix(a, "--description=")
				case strings.HasPrefix(a, "--status="):
					b.status = strings.TrimPrefix(a, "--status=")
				case strings.HasPrefix(a, "--assignee="):
					b.assignee = strings.TrimPrefix(a, "--assignee=")
				}
			}
		}
		return bdOut("")
	}}
}

// rollbackFixture is a slingRollback over a temp town with an in-process bd
// and in-memory work release and sandbox. Nothing it does starts a process
// or reads the cwd, the environment or a package global.
type rollbackFixture struct {
	r     slingRollback
	bd    *inprocBD
	town  *beadsfake.Fake
	rel   *fakeWorkReleaser
	sb    *fakeSandbox
	seats int
}

func newRollbackFixture(t *testing.T, bd *inprocBD, rel *fakeWorkReleaser) *rollbackFixture {
	t.Helper()
	if bd == nil {
		bd = mutableBD(&mutableBead{id: "gt-abc", status: "open"})
	}
	if rel == nil {
		rel = &fakeWorkReleaser{beads: map[string][2]string{}}
	}
	f := &rollbackFixture{bd: bd, town: beadsfake.New(beadsfake.WithPrefix("hq")), rel: rel, sb: &fakeSandbox{}}
	townRoot := t.TempDir()
	run := bd.run
	f.r = slingRollback{
		townRoot:         townRoot,
		bd:               run,
		townBeads:        f.town,
		getBeadInfo:      func(id string) (*beadInfo, error) { return getBeadInfoVia(run, townRoot, id) },
		collectMolecules: collectExistingMolecules,
		burnMolecules: func(m []string, id, tr string) error {
			return burnExistingMoleculesVia(run, m, id, tr)
		},
		releaseSeat:   func() { f.seats++ },
		newReleaser:   func(string, string) polecatWorkReleaser { return f.rel },
		survivingWork: func(string, string) (string, error) { return "", nil },
		openSandbox:   func(string, string) (spawnedPolecatSandbox, error) { return f.sb, nil },
	}
	return f
}

const rawReviewDesc = "attached_at: 2026-06-30T12:00:00Z\nno_merge: true\nreview_only: true\ndispatched_by: mayor/\n\nKeep this body."

// TestSlingRollbackClearsRawReviewOnlyMetadata: a failed raw sling leaves no
// no_merge/review_only marks behind, and keeps the rest of the description.
func TestSlingRollbackClearsRawReviewOnlyMetadata(t *testing.T) {
	t.Parallel()
	bead := &mutableBead{id: "gt-rawrollback", status: "open", desc: rawReviewDesc}
	f := newRollbackFixture(t, mutableBD(bead), nil)

	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "toast"}, "gt-rawrollback", "", "")

	desc := bead.description()
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
	bead := &mutableBead{id: "gt-rawrollback", status: "open", desc: initial}
	f := newRollbackFixture(t, mutableBD(bead), nil)
	f.r.collectMolecules = func(*beadInfo) []string { return []string{"gt-wisp-stale"} }
	f.r.burnMolecules = func([]string, string, string) error { return errors.New("forced burn failure") }

	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "toast"}, "gt-rawrollback", "", "")

	desc := bead.description()
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
	bead := &mutableBead{id: "gt-rawrollback", status: "open", desc: "attached_molecule: gt-wisp-stale\n" + rawReviewDesc}
	f := newRollbackFixture(t, mutableBD(bead), nil)
	f.r.collectMolecules = func(*beadInfo) []string { return []string{"gt-wisp-stale"} }
	f.r.burnMolecules = func(m []string, _, _ string) error {
		bead.setDescription(rawReviewDesc) // the burn detached the molecule
		return nil
	}

	f.r.rollback(&SpawnedPolecatInfo{RigName: "gastown", PolecatName: "toast"}, "gt-rawrollback", "", "")

	desc := bead.description()
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
