package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// tripwireReason is the reason the mid-run tripwire gives a stop (the bead that
// sets it, gt-uj5a1i), used here as any caller's reason would be.
const tripwireReason = "tripwire: 350 added lines at 12m0s, at or over split_added_lines 300; split the bead"

// reasonSeat is the stop test's seat, stopped for the tripwire's reason.
func reasonSeat() overrunSeat {
	s := stopSeat
	s.StopReason = tripwireReason
	return s
}

// overrunStoppedSummary is the summary of the one needs-split item the collector
// raises for bead, read through d.
func overrunStoppedSummary(t *testing.T, d *Daemon, bead string) string {
	t.Helper()
	d.openWorkBeads = func([]string, time.Duration) workBeadReader {
		return needsSplitReader{&beads.Issue{ID: bead, Status: "open", Labels: []string{polecat.NeedsSplitLabel}}}
	}
	d.knownRigsCache, d.knownRigsCacheValid = []string{stopSeat.Rig}, true
	s := &attentionSources{d: d, now: overrunSlung}
	items, err := s.collectOverrunStopped(context.Background())
	if err != nil {
		t.Fatalf("collectOverrunStopped: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	if items[0].Kind != attention.KindOverrunStopped {
		t.Errorf("item kind = %v, want overrun-stopped", items[0].Kind)
	}
	return items[0].Summary
}

// TestRunawayBeadHandBackNoteNamesTheStopReason: a stop the seat's reason began
// puts that reason in the bead's hand-back note as its own sentence.
func TestRunawayBeadHandBackNoteNamesTheStopReason(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, reasonSeat())

	d.runOverrunGuard(f.seams())

	if len(f.handBackNotes) != 1 {
		t.Fatalf("hand-backs = %q, want one", f.handBackNotes)
	}
	if got, want := f.handBackNotes[0], tripwireReason+"."; got != want {
		t.Errorf("hand-back note = %q, want %q", got, want)
	}
}

// TestRunawayBeadHandBackNoteUnchangedWithoutStopReason: a stop the 20 minute
// rule began writes the hand-back note and the attention summary byte for byte
// as it did before a seat could carry a reason.
func TestRunawayBeadHandBackNoteUnchangedWithoutStopReason(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)

	d.runOverrunGuard(f.seams())

	if len(f.handBackNotes) != 1 || f.handBackNotes[0] != "" {
		t.Errorf("hand-back notes = %q, want one empty note", f.handBackNotes)
	}
	want := "gt-aaa11 stopped after 20m0s (20 minute rule); needs a split"
	if got := overrunStoppedSummary(t, d, stopSeat.Bead); got != want {
		t.Errorf("attention summary = %q, want %q", got, want)
	}
}

// TestRunawayBeadAttentionSummaryNamesTheStopReason: the needs-split item for a
// bead stopped for the seat's reason names the reason in place of the 20 minute
// rule's wording.
func TestRunawayBeadAttentionSummaryNamesTheStopReason(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, reasonSeat())

	d.runOverrunGuard(f.seams())

	want := "gt-aaa11 stopped after 20m0s; " + tripwireReason + "; needs a split"
	if got := overrunStoppedSummary(t, d, stopSeat.Bead); got != want {
		t.Errorf("attention summary = %q, want %q", got, want)
	}
}

// TestRunawayBeadResumedStopKeepsTheStopReason: a stop begun on an earlier tick
// is resumed from its saved state, where the paused seat it is read from carries
// no reason; the note the resumed stop writes still names the reason.
func TestRunawayBeadResumedStopKeepsTheStopReason(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, reasonSeat())
	f.failOn = map[string]error{"hand-back": errors.New("bd unreachable")}

	d.runOverrunGuard(f.seams())

	if len(f.handBackNotes) != 0 {
		t.Fatalf("hand-backs = %q, want the failed one to record no note", f.handBackNotes)
	}
	if st, _ := readOverrunState(d.config.TownRoot, stopSeat.Bead); st.StopReason != tripwireReason {
		t.Fatalf("saved stop reason = %q, want %q", st.StopReason, tripwireReason)
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	if len(f.handBackNotes) != 1 {
		t.Fatalf("resumed hand-backs = %q, want one", f.handBackNotes)
	}
	if got, want := f.handBackNotes[0], tripwireReason+"."; got != want {
		t.Errorf("resumed hand-back note = %q, want %q", got, want)
	}
}
