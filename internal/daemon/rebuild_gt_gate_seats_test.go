package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/slot"
)

// holdGateSlot holds slot i of town as role, the way a running suite does: the
// flock is what makes the slot held, and the owner file is what a reader names
// the holder by. It returns a func that releases both.
func holdGateSlot(t *testing.T, town string, i int, role string) func() {
	t.Helper()
	if err := os.MkdirAll(slot.LockDir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := lock.FlockTryAcquire(slot.SlotLockPath(town, i))
	if err != nil || !ok {
		t.Fatalf("holding gate slot %d: ok=%v err=%v", i, ok, err)
	}
	owner := slot.Owner{Role: role, PID: os.Getpid(), AcquiredAt: time.Now(), Slot: i, Settled: true}
	data, err := json.Marshal(owner)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	if err := os.WriteFile(slot.SlotOwnerPath(town, i), data, 0o644); err != nil {
		unlock()
		t.Fatal(err)
	}
	return func() {
		_ = os.Remove(slot.SlotOwnerPath(town, i))
		unlock()
	}
}

// TestRebuildGTGate_NamesTheHoldingSeats: the gate reading that feeds the
// starvation escalation names who holds each slot — role, pid and slot — for a
// gate role and for a saturated pool alike, so the alarm says which runs to
// look at (gt-t135z).
func TestRebuildGTGate_NamesTheHoldingSeats(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		role     string
		wantOpen string
	}{
		{"a gate role holds a slot", "gastown/post-land", "a gate suite holds a slot ("},
		{"a non-gate role holds the only slot", "gastown/polecats/dinki", "every container-gate slot is held ("},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := daemonWithRecorder(t)
			town := d.config.TownRoot
			defer holdGateSlot(t, town, 0, tc.role)()

			busy, reservable := d.rebuildGTGate()
			seat := fmt.Sprintf("%s pid %d (slot 0)", tc.role, os.Getpid())
			if !strings.HasPrefix(busy, tc.wantOpen) || !strings.Contains(busy, seat) {
				t.Errorf("busy = %q, want %q naming %q", busy, tc.wantOpen, seat)
			}
			if !reservable {
				t.Errorf("busy = %q reads as not reservable; a held slot is what the reserve wait takes", busy)
			}
		})
	}
}

// TestRebuildGTStarvedEscalation_NamesTheBlockingSeats: the MEDIUM starvation
// escalation carries the gate reading, so an operator reads which runs hold the
// slots rather than only that the town is busy (gt-t135z).
func TestRebuildGTStarvedEscalation_NamesTheBlockingSeats(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)
	town := d.config.TownRoot
	defer holdGateSlot(t, town, 0, "gastown/post-land")()

	busy, _ := d.rebuildGTGate()
	d.rebuildGTBlock.open(time.Now().Add(-(rebuildGTStarveAfter + time.Minute)))
	cycle := d.startDogCycle("rebuild_gt")
	defer cycle.close()
	d.rebuildGTNoteBlocked(cycle, true, "not quiet: "+busy)

	esc := rec.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %+v, want one", esc)
	}
	got := esc[0].Escalation
	if got.Severity != "MEDIUM" {
		t.Errorf("severity = %q, want MEDIUM", got.Severity)
	}
	if got.Fingerprint != alertKeyRebuildGTStarved {
		t.Errorf("fingerprint = %q, want %s", got.Fingerprint, alertKeyRebuildGTStarved)
	}
	seat := fmt.Sprintf("gastown/post-land pid %d (slot 0)", os.Getpid())
	if !strings.Contains(got.Reason, seat) {
		t.Errorf("escalation reason %q does not name the holding seat %q", got.Reason, seat)
	}
}
