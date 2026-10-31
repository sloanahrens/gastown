package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/sling"
)

// dockedBeads answers the rig identity bead reads a docked check makes, and
// counts them, so a test can hold a tick to the reads that reached the store.
type dockedBeads struct {
	beads.Client
	fail  bool
	shows map[string]int
}

func newDockedBeads(fail bool) *dockedBeads {
	return &dockedBeads{fail: fail, shows: map[string]int{}}
}

func (d *dockedBeads) Show(id string) (*beads.Issue, error) {
	d.shows[id]++
	if d.fail {
		return nil, errors.New("dolt down")
	}
	return nil, beads.ErrNotFound
}

// tickDockedTown is a town whose two rigs are registered and whose directories
// exist, so the tick's board scan reaches its parked-or-docked check for each.
func tickDockedTown(t *testing.T) string {
	t.Helper()
	town := parkTown(t)
	for _, rig := range []string{"testrig", "otherrig"} {
		if err := os.MkdirAll(filepath.Join(town, rig), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

// TestTickReadsRigDockedStateOncePerRig: the tick checks each rig's docked
// state while it scans the board and again for every candidate it slings, but
// the answer cannot change inside the tick, so the identity bead is read once
// per rig (gt-ze7z5). Mutation: drop the memo and every check reads again.
func TestTickReadsRigDockedStateOncePerRig(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(false)
	docked := newRigDockedMemo(store)
	board := func(string) ([]*beads.Issue, error) { return nil, nil }

	// The board scan asks each rig once.
	specCandidatesRetrying(town, 2, board, docked.parkedOrDocked, nil)
	// Each sling behind the scan asks again: the sling's guard and the spawn's.
	rigs := []string{"testrig", "otherrig"}
	for i := 0; i < 2; i++ {
		for _, rig := range rigs {
			if blocked, reason := docked.parkedOrDocked(town, rig); blocked || reason != "" {
				t.Errorf("docked.parkedOrDocked(%s) = %v, %q; want not blocked", rig, blocked, reason)
			}
		}
	}

	want := map[string]int{"tr-rig-testrig": 1, "or-rig-otherrig": 1}
	for id, n := range want {
		if got := store.shows[id]; got != n {
			t.Errorf("identity bead %s read %d time(s) in one tick, want %d", id, got, n)
		}
	}
	for id, n := range store.shows {
		if _, known := want[id]; !known {
			t.Errorf("unexpected read of %s (%d times)", id, n)
		}
	}
}

// TestTickDockedMemoIsSafeUnderConcurrentChecks: the tick slings in parallel,
// and every sling checks the same rig's docked state. The memo answers them
// all from the one read: no second read, and no unsynchronized map access.
func TestTickDockedMemoIsSafeUnderConcurrentChecks(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(false)
	docked := newRigDockedMemo(store)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if blocked, reason := docked.parkedOrDocked(town, "testrig"); blocked || reason != "" {
				t.Errorf("concurrent check = %v, %q; want not blocked", blocked, reason)
			}
		}()
	}
	wg.Wait()

	if got := store.shows["tr-rig-testrig"]; got != 1 {
		t.Errorf("identity bead read %d time(s) by %d concurrent checks, want 1", got, n)
	}
}

// TestSpecSlingSpawnCarriesTheTicksDockedMemo: the spawn behind a tick's sling
// answers the rig guard from the tick's memo, not a read of its own
// (gt-ze7z5). Mutation: drop the assignment and the spawn asks bd again.
func TestSpecSlingSpawnCarriesTheTicksDockedMemo(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(false)
	docked := newRigDockedMemo(store)
	var got SlingSpawnOptions
	spawn := func(_ string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		got = opts
		return &SpawnedPolecatInfo{}, nil
	}

	if _, err := specSlingSpawn(spawn, nil, nil, docked)("testrig", SlingSpawnOptions{}); err != nil {
		t.Fatalf("spawn error = %v", err)
	}
	if got.rigParked == nil {
		t.Fatal("the spawn carries no docked check, so its guard would read the rig's identity bead again")
	}
	for i := 0; i < 2; i++ {
		if blocked, reason := got.rigParked(town, "testrig"); blocked || reason != "" {
			t.Errorf("spawn's docked check = %v, %q; want not blocked", blocked, reason)
		}
	}
	if n := store.shows["tr-rig-testrig"]; n != 1 {
		t.Errorf("the spawn's checks read the identity bead %d time(s), want the tick's 1", n)
	}
}

// TestTickDockedReadFailureStaysBlocked: a docked read that fails blocks the
// rig as unverified, and the failure is not remembered as an answer: the next
// check in the same tick asks the store again, so a bd that recovers inside
// the tick is seen (gt-ze7z5).
func TestTickDockedReadFailureStaysBlocked(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(true)
	docked := newRigDockedMemo(store)

	for i := 1; i <= 2; i++ {
		blocked, reason := docked.parkedOrDocked(town, "testrig")
		if !blocked || reason != sling.RigStatusUnverified {
			t.Fatalf("check %d = %v, %q; want blocked as %q", i, blocked, reason, sling.RigStatusUnverified)
		}
		if got := store.shows["tr-rig-testrig"]; got != i {
			t.Errorf("after check %d the store was read %d time(s), want %d: a failed read must not be cached", i, got, i)
		}
	}
}
