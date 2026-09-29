package cmd

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// Patrol discovery (findActivePatrol) and burn (burnPreviousPatrolWisps) run
// against beadsfake here. The fake models bd's batch-close refusals,
// argument order included, and RunClientContract pins it to real bd. The
// path through a real bd database, PatrolConfig{BeadsDir} with no injected
// client, is guarded by TestIntegrationFindActivePatrolReadsBeadsDir.

const (
	testPatrolMol      = "mol-test-patrol"
	testPatrolAssignee = "testrig/witness"
)

// newPatrolFake returns an empty database and a PatrolConfig that reads it.
func newPatrolFake() (*beadsfake.Fake, PatrolConfig) {
	bd := beadsfake.New()
	return bd, PatrolConfig{
		PatrolMolName: testPatrolMol,
		BeadsDir:      "/nonexistent/patrol-test", // never read: Beads is set
		Assignee:      testPatrolAssignee,
		Beads:         bd,
	}
}

// createHookedPatrol creates a bead with a patrol title and hooks it.
// If withOpenChild is true, creates an open child bead to simulate an active patrol.
func createHookedPatrol(t *testing.T, b beads.Client, molName, assignee string, withOpenChild bool) string {
	t.Helper()
	root, err := b.Create(beads.CreateOptions{
		Title:    molName + " (wisp)",
		Priority: -1,
	})
	if err != nil {
		t.Fatalf("create patrol root: %v", err)
	}

	hooked := beads.StatusHooked
	if err := b.Update(root.ID, beads.UpdateOptions{
		Status:   &hooked,
		Assignee: &assignee,
	}); err != nil {
		t.Fatalf("hook patrol: %v", err)
	}

	if withOpenChild {
		_, err := b.Create(beads.CreateOptions{
			Title:    "inbox-check",
			Parent:   root.ID,
			Priority: -1,
		})
		if err != nil {
			t.Fatalf("create child: %v", err)
		}
	}
	return root.ID
}

// createStalePatrol creates a hooked patrol whose only step is closed, the
// state a squash that did not close the root leaves behind.
func createStalePatrol(t *testing.T, b beads.Client) string {
	t.Helper()
	id := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, true)
	children, err := b.List(beads.ListOptions{Parent: id, Status: "all", Priority: -1})
	if err != nil {
		t.Fatalf("list children of %s: %v", id, err)
	}
	if len(children) != 1 {
		t.Fatalf("patrol %s has %d children, want 1", id, len(children))
	}
	for _, child := range children {
		if err := b.ForceCloseWithReason("test cleanup", child.ID); err != nil {
			t.Fatalf("close child of %s: %v", id, err)
		}
	}
	return id
}

func wantStatus(t *testing.T, b beads.Client, id, want string) {
	t.Helper()
	issue, err := b.Show(id)
	if err != nil {
		t.Fatalf("show %s: %v", id, err)
	}
	if issue.Status != want {
		t.Errorf("%s (%q) status = %q, want %q", id, issue.Title, issue.Status, want)
	}
}

func TestFindActivePatrolHooked(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	rootID := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, true /* withOpenChild */)

	patrolID, line, found, err := findActivePatrol(cfg)
	if err != nil {
		t.Fatalf("findActivePatrol error: %v", err)
	}
	if !found {
		t.Fatal("expected to find active patrol, got not found")
	}
	if patrolID != rootID {
		t.Errorf("patrolID = %q, want %q", patrolID, rootID)
	}
	if want := rootID + "  " + testPatrolMol + " (wisp) [hooked]"; line != want {
		t.Errorf("patrol line = %q, want %q", line, want)
	}
	wantStatus(t, b, rootID, beads.StatusHooked)
}

func TestFindActivePatrolStale(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	rootID := createStalePatrol(t, b)

	_, _, found, err := findActivePatrol(cfg)
	if err != nil {
		t.Fatalf("findActivePatrol error: %v", err)
	}
	if found {
		t.Fatal("expected stale patrol (all children closed) to NOT be found as active")
	}
	wantStatus(t, b, rootID, "closed")
	if got, _ := b.Show(rootID); got.CloseReason != "stale patrol cleanup" {
		t.Errorf("close reason = %q, want %q", got.CloseReason, "stale patrol cleanup")
	}
}

// TestFindActivePatrolZeroChildren: a freshly created wisp whose steps have
// not materialized yet is active, not stale, so discovery does not close a
// patrol in the window between root creation and step population.
func TestFindActivePatrolZeroChildren(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	rootID := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, false /* no children */)

	patrolID, _, found, err := findActivePatrol(cfg)
	if err != nil {
		t.Fatalf("findActivePatrol error: %v", err)
	}
	if !found {
		t.Fatal("expected zero-children patrol to be treated as active (not stale)")
	}
	if patrolID != rootID {
		t.Errorf("patrolID = %q, want %q", patrolID, rootID)
	}
	wantStatus(t, b, rootID, beads.StatusHooked)
}

func TestFindActivePatrolMultiple(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	stale1 := createStalePatrol(t, b)
	stale2 := createStalePatrol(t, b)
	activeID := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, true)

	patrolID, _, found, err := findActivePatrol(cfg)
	if err != nil {
		t.Fatalf("findActivePatrol error: %v", err)
	}
	if !found {
		t.Fatal("expected to find active patrol")
	}
	if patrolID != activeID {
		t.Errorf("patrolID = %q, want %q (should return the active one)", patrolID, activeID)
	}
	wantStatus(t, b, activeID, beads.StatusHooked)

	// findActivePatrol stops at the first active patrol, so it does not
	// promise to clean the stale ones (gt-18dzn6p); burnPreviousPatrolWisps
	// does at cycle end. Each is closed or still hooked, never in between.
	for _, id := range []string{stale1, stale2} {
		issue, err := b.Show(id)
		if err != nil {
			t.Fatalf("show stale %s: %v", id, err)
		}
		if issue.Status != "closed" && issue.Status != beads.StatusHooked {
			t.Errorf("stale patrol %s status = %q, want closed or hooked", id, issue.Status)
		}
	}
}

// TestFindActivePatrol_StaleCleanupCapped verifies that when many stale patrols
// accumulate with no active patrol, cleanup is capped at maxStalePurgePerRun per call
// to prevent overwhelming Dolt with sequential write queries (gt-18dzn6p).
func TestFindActivePatrol_StaleCleanupCapped(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	numStale := maxStalePurgePerRun + 3
	staleIDs := make([]string, numStale)
	for i := range staleIDs {
		staleIDs[i] = createStalePatrol(t, b)
	}

	_, _, found, err := findActivePatrol(cfg)
	if err != nil {
		t.Fatalf("findActivePatrol error: %v", err)
	}
	if found {
		t.Fatal("expected no active patrol (all stale)")
	}

	closedCount, hookedCount := 0, 0
	for _, id := range staleIDs {
		issue, err := b.Show(id)
		if err != nil {
			t.Fatalf("show stale %s: %v", id, err)
		}
		switch issue.Status {
		case "closed":
			closedCount++
		case beads.StatusHooked:
			hookedCount++
		default:
			t.Errorf("stale patrol %s unexpected status %q", id, issue.Status)
		}
	}
	// Every patrol is stale, so the scan reaches all of them and the cap is
	// the only limit.
	if closedCount != maxStalePurgePerRun {
		t.Errorf("closed %d stale patrols, want exactly %d (the cap)", closedCount, maxStalePurgePerRun)
	}
	if hookedCount != numStale-maxStalePurgePerRun {
		t.Errorf("hooked = %d, want %d left for burnPreviousPatrolWisps", hookedCount, numStale-maxStalePurgePerRun)
	}

	// The next call cleans the rest.
	if _, _, _, err := findActivePatrol(cfg); err != nil {
		t.Fatalf("second findActivePatrol error: %v", err)
	}
	for _, id := range staleIDs {
		wantStatus(t, b, id, "closed")
	}
}

// TestFindActivePatrolIgnoresOtherMoleculesAndAssignees: only this role's
// patrol molecule, hooked to this agent, is a candidate.
func TestFindActivePatrolIgnoresOtherMoleculesAndAssignees(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	createHookedPatrol(t, b, "mol-other-patrol", testPatrolAssignee, true)
	createHookedPatrol(t, b, testPatrolMol, "otherrig/witness", true)
	otherStale := createStalePatrol(t, b)
	other := "otherrig/witness"
	if err := b.Update(otherStale, beads.UpdateOptions{Assignee: &other}); err != nil {
		t.Fatal(err)
	}

	id, _, found, err := findActivePatrol(cfg)
	if err != nil || found {
		t.Fatalf("findActivePatrol = %q, found %v, %v; want nothing found", id, found, err)
	}
	wantStatus(t, b, otherStale, beads.StatusHooked)
}

// failingChildren fails every child listing, as a transient bd failure does.
type failingChildren struct{ beads.Client }

func (failingChildren) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("bd list: connection refused")
}

// TestFindActivePatrolChildListingErrorIsNotStale: a patrol whose children
// cannot be listed is neither closed nor reported absent, so the caller does
// not destroy an active patrol or spawn a duplicate.
func TestFindActivePatrolChildListingErrorIsNotStale(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	rootID := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, true)
	cfg.Beads = failingChildren{b}

	_, _, found, err := findActivePatrol(cfg)
	if err == nil || !strings.Contains(err.Error(), "discovery incomplete: 1 patrol(s) skipped") {
		t.Errorf("findActivePatrol error = %v, want discovery incomplete", err)
	}
	if found {
		t.Error("found = true for a patrol whose children could not be listed")
	}
	wantStatus(t, b, rootID, beads.StatusHooked)
}

func TestBurnPreviousPatrolWisps(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	id1 := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, true)
	id2 := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, false)
	id3 := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, true)

	burnPreviousPatrolWisps(cfg)

	for _, id := range []string{id1, id2, id3} {
		wantStatus(t, b, id, "closed")
		kids, err := b.Children(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range kids {
			if k.Status != "closed" {
				t.Errorf("step %s of burned patrol %s status = %q, want closed", k.ID, id, k.Status)
			}
		}
	}
}

func TestBurnPreviousPatrolWisps_IgnoresOtherBeads(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	patrolID := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, true)
	otherID := createHookedPatrol(t, b, "some-other-work", testPatrolAssignee, false)

	burnPreviousPatrolWisps(cfg)

	wantStatus(t, b, patrolID, "closed")
	wantStatus(t, b, otherID, beads.StatusHooked)
}

// createChainedPatrol creates a hooked patrol with n open steps chained by
// blocks dependencies, as bd mol bond makes them, linked in the order given
// so the chain does not follow ID order: step order[i] is the i-th link.
func createChainedPatrol(t *testing.T, b beads.Client, order []int) (string, []string) {
	t.Helper()
	id := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, false)
	steps := make([]string, len(order))
	for i := range order {
		is, err := b.Create(beads.CreateOptions{Title: fmt.Sprintf("step %d", i), Parent: id, Priority: -1})
		if err != nil {
			t.Fatal(err)
		}
		steps[i] = is.ID
	}
	for i := 1; i < len(order); i++ {
		if err := b.AddDependency(steps[order[i]], steps[order[i-1]]); err != nil {
			t.Fatal(err)
		}
	}
	return id, steps
}

// TestBurnPreviousPatrolWisps_ClosesAShuffledStepChain: bd closes a batch in
// argument order and refuses a step whose blocker comes later in the batch.
// The burn must still close every step of a chained patrol, and its root.
// A single batch close left the late links, and so the root, open.
func TestBurnPreviousPatrolWisps_ClosesAShuffledStepChain(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	id, steps := createChainedPatrol(t, b, []int{4, 2, 0, 3, 1})

	burnPreviousPatrolWisps(cfg)

	for _, s := range steps {
		wantStatus(t, b, s, "closed")
	}
	wantStatus(t, b, id, "closed")
	if got, _ := b.Show(id); got.CloseReason != "burned: replaced by new patrol cycle" {
		t.Errorf("close reason = %q", got.CloseReason)
	}
}

// TestBurnPreviousPatrolWisps_KeepsPatrolWithARefusedStep: a step bd refuses
// (another agent holds it) stays open, and so does its patrol's root, so the
// unfinished work stays visible (gt-7lx3). The other patrols still burn.
func TestBurnPreviousPatrolWisps_KeepsPatrolWithARefusedStep(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	kept, steps := createChainedPatrol(t, b, []int{1, 0, 2})
	other := "gastown/polecats/other"
	// Chain: steps[1] <- steps[0] <- steps[2]. Refuse steps[0].
	if err := b.Update(steps[0], beads.UpdateOptions{Assignee: &other}); err != nil {
		t.Fatal(err)
	}
	burned := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, true)

	burnPreviousPatrolWisps(cfg)

	wantStatus(t, b, steps[1], "closed")
	wantStatus(t, b, steps[0], "open")
	wantStatus(t, b, steps[2], "open")
	wantStatus(t, b, kept, beads.StatusHooked)
	wantStatus(t, b, burned, "closed")
}

// TestFindActivePatrolStaleCleanupKeepsRootOverRefusedGrandchild: a stale
// patrol (every direct step closed) whose cleanup bd refuses part of keeps
// its root, so discovery never closes a root over an open step.
func TestFindActivePatrolStaleCleanupKeepsRootOverRefusedGrandchild(t *testing.T) {
	t.Parallel()
	b, cfg := newPatrolFake()
	id := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, false)
	step, err := b.Create(beads.CreateOptions{Title: "step", Parent: id, Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := b.Create(beads.CreateOptions{Title: "sub-step", Parent: step.ID, Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	other := "gastown/polecats/other"
	if err := b.Update(sub.ID, beads.UpdateOptions{Assignee: &other}); err != nil {
		t.Fatal(err)
	}
	// The step is closed past its open sub-step, as a squash leaves it.
	if err := b.ForceCloseWithReason("squashed", step.ID); err != nil {
		t.Fatal(err)
	}

	if _, _, found, err := findActivePatrol(cfg); err != nil || found {
		t.Fatalf("findActivePatrol found %v, %v; want the stale patrol not found", found, err)
	}
	wantStatus(t, b, sub.ID, "open")
	wantStatus(t, b, id, beads.StatusHooked)
}

// TestPatrolConfigClient: an injected client is used as is, and without one
// the helpers read bd in BeadsDir. The real-bd end of that is
// TestIntegrationFindActivePatrolReadsBeadsDir.
func TestPatrolConfigClient(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	if got := (PatrolConfig{BeadsDir: "/x", Beads: fake}).client(); got != beads.Client(fake) {
		t.Errorf("client() = %T, want the injected fake", got)
	}
	dir := t.TempDir()
	got, ok := PatrolConfig{BeadsDir: dir}.client().(*beads.Beads)
	if !ok {
		t.Fatalf("client() = %T, want *beads.Beads", PatrolConfig{BeadsDir: dir}.client())
	}
	if !reflect.DeepEqual(got, beads.New(dir)) {
		t.Errorf("client() = %+v, want beads.New(%q)", got, dir)
	}
	if reflect.DeepEqual(got, beads.New(t.TempDir())) {
		t.Error("client() compares equal to bd in another directory; the check above proves nothing")
	}
}
