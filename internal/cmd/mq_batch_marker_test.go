package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/slot"
)

// heldMarker reads the in-flight markers the town currently holds and returns
// the one named name, or nil. StatusPoolLocksOnly, not StatusPool: a marker
// never occupies a pool slot, and this reading must not shell out to docker
// ps to answer a question about one.
func heldMarker(t *testing.T, townRoot, name string) *slot.SlotState {
	t.Helper()

	rep, err := slot.StatusPoolLocksOnly(townRoot, slot.DefaultPool)
	if err != nil {
		t.Fatalf("slot.StatusPoolLocksOnly: %v", err)
	}
	for i := range rep.Slots {
		st := rep.Slots[i]
		if st.Marker && st.Name == name {
			return &st
		}
	}
	return nil
}

// TestAcquireBatchMarker_HeldForTheWholeRun is the contract plugins/stuck-work-dog
// keys on (gt-lhaum): while a batch run is in flight the rig's marker is
// readable from the same townwide lock directory the gate slot lives in, and
// Release — the deferred call on every return path of runMQBatchRun — clears it.
func TestAcquireBatchMarker_HeldForTheWholeRun(t *testing.T) {
	townRoot := t.TempDir()

	release, err := acquireBatchMarker(townRoot, "gastown")
	if err != nil {
		t.Fatalf("acquireBatchMarker: %v", err)
	}

	st := heldMarker(t, townRoot, batchMarkerName("gastown"))
	if st == nil {
		t.Fatalf("no in-flight marker %q while the batch run holds it", batchMarkerName("gastown"))
	}
	if st.Owner == nil {
		t.Fatalf("marker %q held with no owner metadata: %+v", st.Name, st)
	}
	if st.Owner.Role != batchMarkerRole("gastown") {
		t.Errorf("marker role = %q, want %q — the batch gate's own role, so a reader needs no new vocabulary",
			st.Owner.Role, batchMarkerRole("gastown"))
	}

	release()

	if st := heldMarker(t, townRoot, batchMarkerName("gastown")); st != nil {
		t.Errorf("marker %q still held after release: %+v", st.Name, st)
	}
}

// TestAcquireBatchMarker_SecondBatchIsRefusedNotEnforced pins the refusal as
// reported-but-not-enforced: a second batch of the same rig does not get the
// marker, gets a release it can still call, and leaves the first holder's
// marker untouched. Making the refusal an error the caller returns would turn
// a detector's hint into an admission gate (see acquireBatchMarker).
func TestAcquireBatchMarker_SecondBatchIsRefusedNotEnforced(t *testing.T) {
	townRoot := t.TempDir()

	firstRelease, err := acquireBatchMarker(townRoot, "gastown")
	if err != nil {
		t.Fatalf("first acquireBatchMarker: %v", err)
	}
	defer firstRelease()

	secondRelease, err := acquireBatchMarker(townRoot, "gastown")
	var held *slot.MarkerHeldError
	if !errors.As(err, &held) {
		t.Fatalf("second acquireBatchMarker error = %v, want a *slot.MarkerHeldError", err)
	}
	// Safe to call even though the second acquire failed, so runMQBatchRun can
	// always defer it unconditionally.
	secondRelease()

	if st := heldMarker(t, townRoot, batchMarkerName("gastown")); st == nil {
		t.Errorf("marker %q was released by the refused second acquire's release", batchMarkerName("gastown"))
	}
}

// TestAcquireBatchMarker_RigsDoNotCollide covers the name's rig key: two rigs
// batch at once on a town whose marker directory is shared, and neither may
// read as the other's batch.
func TestAcquireBatchMarker_RigsDoNotCollide(t *testing.T) {
	townRoot := t.TempDir()

	gastownRelease, err := acquireBatchMarker(townRoot, "gastown")
	if err != nil {
		t.Fatalf("acquireBatchMarker(gastown): %v", err)
	}
	defer gastownRelease()

	otherRelease, err := acquireBatchMarker(townRoot, "otherrig")
	if err != nil {
		t.Fatalf("acquireBatchMarker(otherrig) refused while gastown's batch is in flight: %v", err)
	}
	defer otherRelease()

	for _, rig := range []string{"gastown", "otherrig"} {
		if st := heldMarker(t, townRoot, batchMarkerName(rig)); st == nil {
			t.Errorf("no in-flight marker %q while %s's batch holds it", batchMarkerName(rig), rig)
		}
	}
}
