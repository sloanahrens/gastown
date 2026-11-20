package daemon

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// newOverrunSeatDaemon is a guard daemon whose polecat capable holds gt-aaa11,
// on a fixed clock, with a work-bead store the test swaps for an unreadable
// one.
func newOverrunSeatDaemon(t *testing.T) (*Daemon, *bytes.Buffer, *clockwork.FakeClock) {
	t.Helper()
	d, buf := newOverrunDaemon(t, false)
	clk := clockwork.NewFakeClockAt(testEpoch)
	d.clock = clk
	tm := newFakeTmux(clk)
	d.tmux = tm
	d.openWorkBeads = workBeadStore{"gt-aaa11": {ID: "gt-aaa11"}}.open

	seatFor := supervisor.SeatFor("gastown", string(constants.RolePolecat), "capable")
	if _, err := intent.Update(d.config.TownRoot, supervisor.IntentSeat(seatFor), func(r *intent.Record) error {
		r.WorkBead = "gt-aaa11"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tm.addSession(session.PolecatSessionName(d.prefixRegistry().PrefixForRig("gastown"), "capable"), "node", testEpoch)
	return d, buf, clk
}

// primeKeptRead reads the seat while its bead is readable, so the guard keeps
// that read, and then makes the store unreadable.
func primeKeptRead(t *testing.T, d *Daemon, buf *bytes.Buffer) {
	t.Helper()
	if _, ok := d.overrunSeatFor(t.TempDir(), "gastown", "capable"); !ok {
		t.Fatalf("the readable seat was dropped:\n%s", buf.String())
	}
	d.openWorkBeads = unreadableBeads{}.open
}

// TestOverrunGuardStaleNudge: a seat whose bead cannot be read this tick is
// judged on the last good read the guard kept, and marked at the nudge
// threshold like any other; a kept read too old to judge, or none at all,
// leaves the seat unjudged as before (gt-80rmki).
func TestOverrunGuardStaleNudge(t *testing.T) {
	t.Parallel()

	t.Run("a five minute old read is judged, and the seat nudged at 12 minutes", func(t *testing.T) {
		t.Parallel()
		d, buf, clk := newOverrunSeatDaemon(t)
		primeKeptRead(t, d, buf)
		clk.Advance(5 * time.Minute)

		seat, ok := d.overrunSeatFor(t.TempDir(), "gastown", "capable")
		if !ok {
			t.Fatalf("dropped a seat with a kept read 5m old:\n%s", buf.String())
		}
		if !seat.Stale || seat.StaleFor != 5*time.Minute {
			t.Errorf("seat Stale/StaleFor = %v/%v, want true/5m", seat.Stale, seat.StaleFor)
		}
		if !strings.Contains(buf.String(), "judged on a read 5 minutes old") {
			t.Errorf("the log does not say the seat was judged on a kept read:\n%s", buf.String())
		}

		f := newOverrunFake(12*time.Minute, seat)
		d.runOverrunGuard(f.seams())

		if len(f.nudges) != 1 || f.nudges[0] != "gastown/capable" {
			t.Errorf("nudges = %v, want one of gastown/capable", f.nudges)
		}
		if len(f.stops) != 0 {
			t.Errorf("stops = %v, want none at the nudge threshold", f.stops)
		}
	})

	t.Run("a twenty minute old read is not judged", func(t *testing.T) {
		t.Parallel()
		d, buf, clk := newOverrunSeatDaemon(t)
		primeKeptRead(t, d, buf)
		clk.Advance(20 * time.Minute)

		if seat, ok := d.overrunSeatFor(t.TempDir(), "gastown", "capable"); ok {
			t.Fatalf("judged a seat on a read 20m old: %+v", seat)
		}
		if !strings.Contains(buf.String(), "cannot read bead gt-aaa11, not judging it this tick") {
			t.Errorf("the log does not say the seat went unjudged:\n%s", buf.String())
		}
	})

	t.Run("no kept read is not judged", func(t *testing.T) {
		t.Parallel()
		d, buf, _ := newOverrunSeatDaemon(t)
		d.openWorkBeads = unreadableBeads{}.open

		if seat, ok := d.overrunSeatFor(t.TempDir(), "gastown", "capable"); ok {
			t.Fatalf("judged a seat with no kept read: %+v", seat)
		}
		if !strings.Contains(buf.String(), "cannot read bead gt-aaa11, not judging it this tick") {
			t.Errorf("the log does not say the seat went unjudged:\n%s", buf.String())
		}
	})
}

// TestOverrunGuardStaleNoStop: a stale seat past the stop threshold is not
// stopped, and the log names why, so a stop always rests on a fresh read of
// the bead (gt-80rmki).
func TestOverrunGuardStaleNoStop(t *testing.T) {
	t.Parallel()
	d, buf, clk := newOverrunSeatDaemon(t)
	primeKeptRead(t, d, buf)
	clk.Advance(5 * time.Minute)

	seat, ok := d.overrunSeatFor(t.TempDir(), "gastown", "capable")
	if !ok || !seat.Stale {
		t.Fatalf("seat = %+v, ok = %v, want a stale seat", seat, ok)
	}

	f := newOverrunFake(21*time.Minute, seat)
	d.runOverrunGuard(f.seams())

	if len(f.stops) != 0 || len(f.calls) != 0 {
		t.Fatalf("stopped a seat judged on a 5m old read: stops %v, steps %v", f.stops, f.calls)
	}
	if !strings.Contains(buf.String(), "not stopping gastown/capable on gt-aaa11") {
		t.Errorf("no log line refusing the stop:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "the kept read is 5 minutes old; a stop rests on a fresh read") {
		t.Errorf("the refusal does not name its reason:\n%s", buf.String())
	}
}

// TestOverrunGuardStaleNoTripwireStop: the split_added_lines tripwire stops a
// seat at the mark, but not a stale one. The mark still stands, because a mark
// acts on nothing a fresh read is needed for (gt-80rmki).
func TestOverrunGuardStaleNoTripwireStop(t *testing.T) {
	t.Parallel()
	d, buf, clk := newOverrunSeatDaemon(t)
	primeKeptRead(t, d, buf)
	clk.Advance(5 * time.Minute)

	seat, ok := d.overrunSeatFor(t.TempDir(), "gastown", "capable")
	if !ok || !seat.Stale {
		t.Fatalf("seat = %+v, ok = %v, want a stale seat", seat, ok)
	}

	d.patrolConfig.Patrols.OverrunGuard.SplitAddedLines = 10
	f := newOverrunFake(12*time.Minute, seat)
	seams := f.seams()
	seams.addedLines = func(overrunSeat) (int, error) { return 50, nil }
	d.runOverrunGuard(seams)

	if len(f.nudges) != 1 {
		t.Errorf("nudges = %v, want the mark to stand", f.nudges)
	}
	if len(f.stops) != 0 || len(f.calls) != 0 {
		t.Fatalf("tripwire stopped a stale seat: stops %v, steps %v", f.stops, f.calls)
	}
	if !strings.Contains(buf.String(), "not stopping gastown/capable on gt-aaa11 for the tripwire") {
		t.Errorf("no log line refusing the tripwire stop:\n%s", buf.String())
	}
}

// TestOverrunGuardStaleDryRunWouldMarkOnly: a dry run of a stale seat reports
// the mark the live guard would make, and not the tripwire stop it refuses
// (gt-80rmki).
func TestOverrunGuardStaleDryRunWouldMarkOnly(t *testing.T) {
	t.Parallel()
	d, buf, clk := newOverrunSeatDaemon(t)
	d.patrolConfig.Patrols.OverrunGuard.DryRun = true
	d.patrolConfig.Patrols.OverrunGuard.SplitAddedLines = 10
	primeKeptRead(t, d, buf)
	clk.Advance(5 * time.Minute)

	seat, ok := d.overrunSeatFor(t.TempDir(), "gastown", "capable")
	if !ok || !seat.Stale {
		t.Fatalf("seat = %+v, ok = %v, want a stale seat", seat, ok)
	}

	f := newOverrunFake(12*time.Minute, seat)
	seams := f.seams()
	counted := 0
	seams.addedLines = func(overrunSeat) (int, error) {
		counted++
		return 50, nil
	}
	d.runOverrunGuard(seams)

	if !strings.Contains(buf.String(), "dry run: would mark gastown/capable on gt-aaa11") {
		t.Errorf("no dry-run mark:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "dry run: would stop") {
		t.Errorf("the dry run claims a stop the live guard refuses:\n%s", buf.String())
	}
	if counted != 0 {
		t.Errorf("the tripwire read the worktree %d times for a seat it never stops", counted)
	}
}

// TestOverrunKeptReadStoreDropsClosedAndOldReads: the kept reads are memory
// only, so the store never keeps a closed bead's read, refuses one older than
// the judging window, and drops what is past its retention on the next keep
// (gt-80rmki).
func TestOverrunKeptReadStoreDropsClosedAndOldReads(t *testing.T) {
	t.Parallel()

	t.Run("a closed bead's read is not kept", func(t *testing.T) {
		t.Parallel()
		var s overrunKeptReadStore
		s.keep("gt-closed", workBeadState{closed: true}, testEpoch)
		if _, _, ok := s.judgeable("gt-closed", testEpoch); ok {
			t.Error("kept a closed bead's read")
		}
	})

	t.Run("a read past the judging window is not judged", func(t *testing.T) {
		t.Parallel()
		var s overrunKeptReadStore
		s.keep("gt-aaa11", workBeadState{}, testEpoch)
		if _, _, ok := s.judgeable("gt-aaa11", testEpoch.Add(overrunKeptReadMaxAge+time.Second)); ok {
			t.Errorf("judged on a read older than %v", overrunKeptReadMaxAge)
		}
	})

	t.Run("a read past retention is dropped on the next keep", func(t *testing.T) {
		t.Parallel()
		var s overrunKeptReadStore
		s.keep("gt-aaa11", workBeadState{}, testEpoch)
		s.keep("gt-bbb22", workBeadState{}, testEpoch.Add(overrunKeptReadRetention+time.Minute))
		s.mu.Lock()
		_, kept := s.entries["gt-aaa11"]
		n := len(s.entries)
		s.mu.Unlock()
		if kept || n != 1 {
			t.Errorf("entries = %d with gt-aaa11 kept = %v, want the new read alone", n, kept)
		}
	})

	t.Run("a read dated ahead of the clock is judged as just read", func(t *testing.T) {
		t.Parallel()
		var s overrunKeptReadStore
		s.keep("gt-aaa11", workBeadState{}, testEpoch.Add(time.Minute))
		_, age, ok := s.judgeable("gt-aaa11", testEpoch)
		if !ok || age != 0 {
			t.Errorf("age = %v, ok = %v; want 0, true", age, ok)
		}
	})
}
