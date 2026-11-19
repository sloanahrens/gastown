package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
)

// gt-k6t2b3: the revival rule's cooldown is a stamp of its own on the seat's
// record. The host writes it with MarkRevived and reads it back for that bead
// only, so the cooldown cannot be lost to a later last action.
func TestReviveCooldownReadsTheRevivalStamp(t *testing.T) {
	t.Parallel()
	p := newPushedTown(t)
	at := time.Date(2026, 11, 19, 3, 30, 0, 0, time.UTC)

	if err := p.h.MarkRevived(pushedRig, pushedPolecat, pushedBead, at); err != nil {
		t.Fatalf("MarkRevived: %v", err)
	}
	got, err := p.h.ReviveCooldown(pushedRig, pushedPolecat, pushedBead)
	if err != nil {
		t.Fatalf("ReviveCooldown: %v", err)
	}
	if !got.Equal(at) {
		t.Errorf("ReviveCooldown = %v, want %v", got, at)
	}

	other, err := p.h.ReviveCooldown(pushedRig, pushedPolecat, "gt-other")
	if err != nil {
		t.Fatalf("ReviveCooldown(gt-other): %v", err)
	}
	if !other.IsZero() {
		t.Errorf("another bead's cooldown = %v, want the zero time", other)
	}

	rec, err := intent.Read(p.h.town(), pushedSeat())
	if err != nil {
		t.Fatalf("read the seat's record: %v", err)
	}
	if rec.LastAction != nil {
		t.Errorf("MarkRevived wrote a last action: %+v", rec.LastAction)
	}
}

// A record the cooldown cannot be read from is an error, never the zero time:
// the zero time would read as "never revived" and let a restart through.
func TestReviveCooldownReadFailureIsAnError(t *testing.T) {
	t.Parallel()
	p := newPushedTown(t)
	if err := p.h.MarkRevived(pushedRig, pushedPolecat, pushedBead, time.Date(2026, 11, 19, 3, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MarkRevived: %v", err)
	}
	if err := os.WriteFile(pushedSeat().Path(p.h.town()), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt the seat's record: %v", err)
	}

	got, err := p.h.ReviveCooldown(pushedRig, pushedPolecat, pushedBead)
	if err == nil {
		t.Fatalf("ReviveCooldown = %v with no error, want the unreadable record reported", got)
	}
	if !got.IsZero() {
		t.Errorf("ReviveCooldown = %v alongside the error, want the zero time", got)
	}
}
