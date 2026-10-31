package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestTickReadsRevertBeadOncePerBead is gt-kuax8: the RevertInFlight hook
// asks bd about a rig's revert bead once in a tick, however many times the
// tick asks whether the revert is still in flight.
func TestTickReadsRevertBeadOncePerBead(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	ids := []string{"gt-rv-a", "gt-rv-b"}
	for _, id := range ids {
		db.Seed(beads.Issue{ID: id, Status: string(beads.StatusOpen)})
	}
	store := newCountingStore(db)
	reads := newSpecTickReads()
	reverts := newSpecRevertBeads()
	storeFor := func(string) beads.Client { return store }

	for call := 1; call <= 3; call++ {
		for _, id := range ids {
			if !revertBeadOpen("gastown", id, reads, reverts, storeFor) {
				t.Fatalf("call %d: %s read as not open, want the seeded open bead", call, id)
			}
		}
	}

	for _, id := range ids {
		if got := store.shows[id]; got != 1 {
			t.Errorf("%s read %d times in one tick, want 1", id, got)
		}
	}
	if got, want := reads.Calls(), len(ids); got != want {
		t.Errorf("the tick's ledger counted %d bd reads, want %d", got, want)
	}
}

// TestRevertBeadOpenFailureIsNotCached is gt-kuax8: a show that failed reads
// as not open, as it always has, and the tick asks again rather than keeping
// the failure for its later calls.
func TestRevertBeadOpenFailureIsNotCached(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-rv", Status: string(beads.StatusOpen)})
	store := newCountingStore(&failFirstShowClient{Client: db, id: "gt-rv", err: errors.New("bd show: dolt unreachable")})
	reads := newSpecTickReads()
	reverts := newSpecRevertBeads()
	storeFor := func(string) beads.Client { return store }

	if revertBeadOpen("gastown", "gt-rv", reads, reverts, storeFor) {
		t.Fatal("a failed show read as an open revert")
	}
	if !revertBeadOpen("gastown", "gt-rv", reads, reverts, storeFor) {
		t.Fatal("the failure was kept: the second call did not ask bd again")
	}
	if got := store.shows["gt-rv"]; got != 2 {
		t.Errorf("the bead was read %d times, want 2: a failed read is not kept", got)
	}
	if got, want := reads.Calls(), 2; got != want {
		t.Errorf("the tick's ledger counted %d bd reads, want %d", got, want)
	}
}
