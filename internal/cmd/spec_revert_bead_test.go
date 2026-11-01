package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/daemon"
)

// TestTickReadsRevertBeadOncePerBead is gt-kuax8: the RevertInFlight hook
// asks bd about a rig's revert bead once in a tick, however many times the
// tick asks whether the revert is still in flight. The hook is the tick's own
// — specRevertInFlight — so a wiring that dropped the cache fails here, not
// only the cache behind it (gt-zl5yq).
func TestTickReadsRevertBeadOncePerBead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	reverts := []struct{ rig, bead string }{
		{rig: "gastown", bead: "gt-rv-a"},
		{rig: "devops", bead: "gt-rv-b"},
	}
	db := beadsfake.New()
	for _, rv := range reverts {
		db.Seed(beads.Issue{ID: rv.bead, Status: string(beads.StatusOpen)})
		writeRevertState(t, townRoot, rv.rig, rv.bead)
	}
	store := newCountingStore(db)
	reads := newSpecTickReads()
	revertBeads := newSpecRevertBeads()
	hook := specRevertInFlight(townRoot, reads, revertBeads, func(string) beads.Client { return store })

	for call := 1; call <= 3; call++ {
		for _, rv := range reverts {
			if hook(rv.rig) == nil {
				t.Fatalf("call %d: %s read as no revert, want the filed bead", call, rv.rig)
			}
		}
	}

	for _, rv := range reverts {
		if got := store.shows[rv.bead]; got != 1 {
			t.Errorf("%s read %d times in one tick, want 1", rv.bead, got)
		}
	}
	if got, want := reads.Calls(), len(reverts); got != want {
		t.Errorf("the tick's ledger counted %d bd reads, want %d", got, want)
	}
}

// TestRevertBeadOpenFailureIsNotCached is gt-kuax8: a show that failed leaves
// the hook with no revert, as it always has, and the tick asks again rather
// than keeping the failure for its later calls.
func TestRevertBeadOpenFailureIsNotCached(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const bead = "gt-rv"
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: bead, Status: string(beads.StatusOpen)})
	writeRevertState(t, townRoot, "gastown", bead)
	store := newCountingStore(&failFirstShowClient{Client: db, id: bead, err: errors.New("bd show: dolt unreachable")})
	reads := newSpecTickReads()
	hook := specRevertInFlight(townRoot, reads, newSpecRevertBeads(), func(string) beads.Client { return store })

	if hook("gastown") != nil {
		t.Fatal("a failed show read as an open revert")
	}
	if hook("gastown") == nil {
		t.Fatal("the failure was kept: the second call did not ask bd again")
	}
	if got := store.shows[bead]; got != 2 {
		t.Errorf("the bead was read %d times, want 2: a failed read is not kept", got)
	}
	if got, want := reads.Calls(), 2; got != want {
		t.Errorf("the tick's ledger counted %d bd reads, want %d", got, want)
	}
}

// writeRevertState writes the red-main state a landing worker leaves while a
// revert of culprit is in flight, naming bead as the revert's work bead.
func writeRevertState(t *testing.T, townRoot, rig, bead string) {
	t.Helper()
	path := daemon.RedMainStatePath(townRoot, rig)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating the red-main state directory: %v", err)
	}
	state := `{"revert":{"culprit":"gt-culprit","bead":"` + bead + `"}}`
	if err := os.WriteFile(path, []byte(state), 0o644); err != nil {
		t.Fatalf("writing the red-main state: %v", err)
	}
}
