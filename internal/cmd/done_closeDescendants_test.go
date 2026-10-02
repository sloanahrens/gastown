package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/done"
)

// TestDoneCloseDescendantsWithChildren verifies that when gt done is called
// with a molecule that has children, closeDescendants closes the children
// before the root molecule (edge case #1).
func TestDoneCloseDescendantsWithChildren(t *testing.T) {
	t.Parallel()
	db := doneMoleculeDB(true)
	seedChildren(t, db, "gt-wisp-xyz",
		beads.Issue{ID: "gt-step-1", Title: "Step 1", Status: "open", Ephemeral: true},
		beads.Issue{ID: "gt-step-2", Title: "Step 2", Status: "open", Ephemeral: true})
	rec := runDoneStateUpdate(t, db)

	got := rec.closed()
	if len(got) != 4 {
		t.Fatalf("closes = %v, want both steps, then the wisp, then the base bead", got)
	}
	steps := []string{got[0], got[1]}
	sort.Strings(steps)
	if !reflect.DeepEqual(steps, []string{"gt-step-1", "gt-step-2"}) || got[2] != "gt-wisp-xyz" || got[3] != "gt-base-123" {
		t.Errorf("closes = %v, want steps, then gt-wisp-xyz, then gt-base-123", got)
	}
}

// TestDoneCloseDescendantsNoChildren verifies that gt done works correctly
// when the molecule has no children - it should just close the molecule and
// hooked bead without errors (edge case #2).
func TestDoneCloseDescendantsNoChildren(t *testing.T) {
	t.Parallel()
	rec := runDoneStateUpdate(t, doneMoleculeDB(true))
	if got, want := rec.closed(), []string{"gt-wisp-xyz", "gt-base-123"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closes = %v, want %v", got, want)
	}
}

// TestDoneCloseDescendantsSomeAlreadyClosed verifies that closeDescendants
// skips children that are already closed (edge case #3).
func TestDoneCloseDescendantsSomeAlreadyClosed(t *testing.T) {
	t.Parallel()
	db := doneMoleculeDB(true)
	seedChildren(t, db, "gt-wisp-xyz",
		beads.Issue{ID: "gt-step-open", Title: "Step Open", Status: "open", Ephemeral: true},
		beads.Issue{ID: "gt-step-closed", Title: "Step Closed", Status: "closed", Ephemeral: true})
	rec := runDoneStateUpdate(t, db)
	if got, want := rec.closed(), []string{"gt-step-open", "gt-wisp-xyz", "gt-base-123"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closes = %v, want %v (the closed step is not closed again)", got, want)
	}
}

// TestDoneCloseDescendantsDeeplyNested verifies that closeDescendants
// correctly handles deeply nested children (grandchildren) recursively
// (edge case #4).
func TestDoneCloseDescendantsDeeplyNested(t *testing.T) {
	t.Parallel()
	db := doneMoleculeDB(true)
	seedChildren(t, db, "gt-wisp-xyz", beads.Issue{ID: "gt-child", Title: "Child", Status: "open", Ephemeral: true})
	seedChildren(t, db, "gt-child", beads.Issue{ID: "gt-grandchild", Title: "Grandchild", Status: "open", Ephemeral: true})
	rec := runDoneStateUpdate(t, db)
	if got, want := rec.closed(), []string{"gt-grandchild", "gt-child", "gt-wisp-xyz", "gt-base-123"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closes = %v, want %v", got, want)
	}
}

// TestDoneCloseDescendantsNoMoleculeAttached verifies that gt done handles
// the case where there is no molecule attached gracefully (edge case #5).
func TestDoneCloseDescendantsNoMoleculeAttached(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(doneAgentBead(),
		beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked), Description: "no molecule attached"})
	rec := runDoneStateUpdate(t, db)
	if got, want := rec.closed(), []string{"gt-base-123"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closes = %v, want %v", got, want)
	}
}

// TestCloseDescendantsHandlesListError verifies that a failure listing the
// molecule's steps is logged and gt done still closes the molecule and the
// hooked bead.
func TestCloseDescendantsHandlesListError(t *testing.T) {
	t.Parallel()
	rec := runDoneStateUpdateWith(t, doneMoleculeDB(true), errors.New("database locked"))
	if got, want := rec.closed(), []string{"gt-wisp-xyz", "gt-base-123"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closes = %v, want %v", got, want)
	}
}

// TestCloseDescendantsMoleculeNotFound: an attached molecule that no longer
// exists was already burned by another path; gt done closes the hooked bead.
func TestCloseDescendantsMoleculeNotFound(t *testing.T) {
	t.Parallel()
	db := doneMoleculeDB(false)
	rec := runDoneStateUpdate(t, db)
	if got, want := rec.closed(), []string{"gt-base-123"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closes = %v, want %v", got, want)
	}
	if is, err := db.Show("gt-base-123"); err != nil || is.Status != "closed" {
		t.Errorf("base bead = %+v, %v; want closed", is, err)
	}
}

// TestDoneStateUpdateMarksAgentDone: after a completed exit the agent bead's
// hook_bead is cleared, its agent_state is done, and the closed wisps were
// purged.
func TestDoneStateUpdateMarksAgentDone(t *testing.T) {
	t.Parallel()
	db := doneMoleculeDB(true)
	rec := runDoneStateUpdate(t, db)
	_, fields, err := beads.GetAgentBead(db, "gt-gastown-polecat-nux")
	if err != nil || fields == nil {
		t.Fatalf("agent bead: %+v, %v", fields, err)
	}
	if fields.AgentState != string(beads.AgentStateDone) || fields.HookBead != "" {
		t.Errorf("agent fields = state %q hook %q, want done and no hook", fields.AgentState, fields.HookBead)
	}
	if rec.purges != 1 {
		t.Errorf("purges = %d, want 1", rec.purges)
	}
}

// doneAgentBead is polecat gastown/nux's agent bead, working on
// gt-base-123.
func doneAgentBead() beads.Issue {
	return beads.Issue{ID: "gt-gastown-polecat-nux", Title: "Polecat nux", Status: "open", Labels: []string{"gt:agent"},
		Description: beads.FormatAgentDescription("Polecat nux", &beads.AgentFields{RoleType: "polecat", Rig: "gastown", AgentState: "working", HookBead: "gt-base-123"})}
}

// doneMoleculeDB holds the agent bead and the hooked base bead, whose
// molecule gt-wisp-xyz is attached; withWisp seeds that molecule's root.
func doneMoleculeDB(withWisp bool) *beadsfake.Fake {
	db := beadsfake.New()
	db.Seed(doneAgentBead(),
		beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked), Description: "attached_molecule: gt-wisp-xyz"})
	if withWisp {
		db.Seed(beads.Issue{ID: "gt-wisp-xyz", Title: "mol-polecat-work", Status: "open", Ephemeral: true})
	}
	return db
}

// seedChildren seeds kids as parent's children.
func seedChildren(t *testing.T, db *beadsfake.Fake, parent string, kids ...beads.Issue) {
	t.Helper()
	for _, kid := range kids {
		db.Seed(kid)
		if err := db.AddTypedDependency(kid.ID, parent, "parent-child"); err != nil {
			t.Fatalf("parent %s of %s: %v", parent, kid.ID, err)
		}
	}
}

// doneRecorder is the store updateAgentStateOnDone sees: db, with every
// closed ID recorded in order, Children failing with childrenErr when set,
// and each purge counted.
type doneRecorder struct {
	beads.Client
	childrenErr error

	mu     *sync.Mutex
	closes *[]string
	purges int
}

func (r doneRecorder) record(ids []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.closes = append(*r.closes, ids...)
}

func (r doneRecorder) Children(id string) ([]*beads.Issue, error) {
	if r.childrenErr != nil {
		return nil, r.childrenErr
	}
	return r.Client.Children(id)
}

func (r doneRecorder) Close(ids ...string) error {
	err := r.Client.Close(ids...)
	r.record(beads.ClosedIDs(ids, err))
	return err
}

func (r doneRecorder) CloseWithReason(reason string, ids ...string) error {
	err := r.Client.CloseWithReason(reason, ids...)
	r.record(beads.ClosedIDs(ids, err))
	return err
}

func (r doneRecorder) ForceCloseWithReason(reason string, ids ...string) error {
	err := r.Client.ForceCloseWithReason(reason, ids...)
	r.record(beads.ClosedIDs(ids, err))
	return err
}

func (r *doneRecorder) closed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), *r.closes...)
}

// runDoneStateUpdate runs updateAgentStateOnDone as polecat gastown/nux,
// from the rig directory of a fresh town whose routes send gt- to the
// gastown rig, with db as every store. No git repository holds the town, so
// there is no HEAD for review evidence.
func runDoneStateUpdate(t *testing.T, db beads.Client) *doneRecorder {
	t.Helper()
	return runDoneStateUpdateWith(t, db, nil)
}

// runDoneStateUpdateWith is runDoneStateUpdate whose Children fails with
// childrenErr when set.
func runDoneStateUpdateWith(t *testing.T, db beads.Client, childrenErr error) *doneRecorder {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	rec := &doneRecorder{Client: db, childrenErr: childrenErr, mu: &sync.Mutex{}, closes: &[]string{}}
	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return rec },
		source: func(string, string) beads.Client { return rec },
		purge:  func(string, string) { rec.purges++ },
		reviewHead: func() (string, error) {
			return "", errors.New("resolving current HEAD: not a git repository")
		},
	}
	_ = updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, ExitCompleted, "gt-base-123")
	return rec
}

// TestDoneStateEnvZeroValueIsTheRealProcess: updateAgentStateOnDone passes
// the zero doneStateEnv, which must read the real environment and the real
// HEAD, and open bd stores.
func TestDoneStateEnvZeroValueIsTheRealProcess(t *testing.T) {
	t.Parallel()
	var e doneStateEnv
	if got, want := reflect.ValueOf(e.lookup()).Pointer(), reflect.ValueOf(os.Getenv).Pointer(); got != want {
		t.Error("zero doneStateEnv does not read the environment through os.Getenv")
	}
	if got, want := reflect.ValueOf(e.head()).Pointer(), reflect.ValueOf(done.CurrentReviewEvidenceHead).Pointer(); got != want {
		t.Error("zero doneStateEnv does not resolve HEAD through done.CurrentReviewEvidenceHead")
	}
	if _, ok := e.routedAt(t.TempDir()).(*beads.Beads); !ok {
		t.Error("zero doneStateEnv's routed store is not bd")
	}
	if got, want := reflect.ValueOf(e.sourceOpener()).Pointer(), reflect.ValueOf(done.OpenSourceStore).Pointer(); got != want {
		t.Error("zero doneStateEnv does not open source stores through done.OpenSourceStore")
	}
}

// TestDoneLeavesReadyToLandBeadOpenWithSubmissionNote: a bead gt done marked
// ready to land is the landing worker's to close, with the landed commit. gt
// done records the submission and leaves it open (ADR 0004).
func TestDoneLeavesReadyToLandBeadOpenWithSubmissionNote(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(doneAgentBead(), beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked),
		Labels: []string{"gt:ready-to-land"},
		Notes:  "READY TO LAND\nBranch: polecat/nux/gt-base-123\nHead: 0123456789abcdef\nTarget: main\nWorker: nux"})
	rec := runDoneStateUpdate(t, db)

	if got := rec.closed(); len(got) != 0 {
		t.Fatalf("gt done closed %v; a bead waiting to land stays open", got)
	}
	comments, err := db.Comments("gt-base-123")
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments = %+v, %v; want one submission note", comments, err)
	}
	if want := "Submitted for landing: polecat/nux/gt-base-123 @ 01234567 onto main (attempt 1)"; !strings.Contains(comments[0].Text, want) {
		t.Fatalf("submission comment = %q, want %q", comments[0].Text, want)
	}
}

// envMap is a getenv over a fixed set of variables; any other is unset.
func envMap(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}
