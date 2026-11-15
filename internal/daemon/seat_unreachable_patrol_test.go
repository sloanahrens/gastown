package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/stretchr/testify/require"
)

// unreachableSeatTown is a daemon whose town holds one rig, myr, with the
// polecat mycat in it. Its polecat-health pass walks that one seat.
func unreachableSeatTown(t *testing.T, bd *workBD) (*Daemon, *fakeTmux, *clockwork.FakeClock) {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "myr", "polecats", "mycat"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeKnownRigs(t, town, "myr")
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC))
	d := &Daemon{
		config:           &Config{TownRoot: town},
		logger:           log.New(&lockedBuffer{}, "", 0),
		clock:            clk,
		tmux:             newFakeTmux(clk),
		openWorkBeads:    bd.open,
		execCmd:          bd.run,
		prefixRegistryFn: myrPrefixes,
	}
	d.invalidateKnownRigsCache()
	d.ctx = t.Context()
	d.rigPool = newRigWorkerPool(2, 10*time.Second, nil)
	return d, d.tmux.(*fakeTmux), clk
}

// seatFrozen writes a held intent record for myr/mycat, so a polecat-health
// pass leaves the seat before it asks the session and has no read for it.
func seatFrozen(t *testing.T, town string) {
	t.Helper()
	seat := intent.Seat{Rig: "myr", Role: constants.RolePolecat, Name: "mycat"}
	if _, err := intent.Update(town, seat, func(r *intent.Record) error {
		r.Frozen = true
		return nil
	}); err != nil {
		t.Fatalf("holding the seat: %v", err)
	}
}

// seatUnreachableQueue is the attention queue's view of one daemon: the items
// the seat-unreachable collector, as the tick registers it, raises.
func seatUnreachableQueue(t *testing.T, d *Daemon) []attention.Item {
	t.Helper()
	c := collectorFor(t, d.attentionSources(d.clk().Now()), attention.KindSeatUnreachable)
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("seat-unreachable collector: %v", err)
	}
	return items
}

// TestSeatUnreachableRaisesAfterFiveMinutes drives the polecat-health pass over
// one seat whose session check keeps failing: under five minutes it raises
// nothing, and at five it queues seat-unreachable:myr/mycat naming the bead the
// seat holds and the error its check returned (gt-q6hafz).
func TestSeatUnreachableRaisesAfterFiveMinutes(t *testing.T) {
	t.Parallel()
	d, tm, clk := unreachableSeatTown(t, hookedWorkBD(t, "gt-p4aqut", time.Hour))
	gone := errors.New("sandbox pod myr-mycat is gone")
	tm.hasErr = gone

	steps := []struct {
		name    string
		advance time.Duration
		want    []attention.Item
	}{
		{"first failed read starts the clock", 0, nil},
		{"four minutes raises nothing", 4 * time.Minute, nil},
		{"one second short raises nothing", time.Minute - time.Second, nil},
		{"five minutes names the seat, its bead and its error", time.Second, []attention.Item{{
			Key:      "seat-unreachable:myr/mycat",
			Kind:     attention.KindSeatUnreachable,
			Severity: attention.SeverityHigh,
			Rig:      "myr",
			Bead:     "gt-p4aqut",
			Summary:  "myr/mycat unreadable for 5m, holding gt-p4aqut: " + gone.Error(),
		}}},
		{"a later tick names it from the same first failure", time.Minute, []attention.Item{{
			Key:      "seat-unreachable:myr/mycat",
			Kind:     attention.KindSeatUnreachable,
			Severity: attention.SeverityHigh,
			Rig:      "myr",
			Bead:     "gt-p4aqut",
			Summary:  "myr/mycat unreadable for 6m, holding gt-p4aqut: " + gone.Error(),
		}}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			clk.Advance(step.advance)
			d.checkPolecatSessionHealth()
			require.Equal(t, step.want, seatUnreachableQueue(t, d))
		})
	}
}

// TestSeatUnreachableKeepsItsItemWhenTheSeatListCouldNotBeRead: a pass whose rig
// seat list cannot be read says nothing about the seats it did not list, so a
// running item neither clears nor moves (gt-q6hafz).
func TestSeatUnreachableKeepsItsItemWhenTheSeatListCouldNotBeRead(t *testing.T) {
	t.Parallel()
	d, tm, clk := unreachableSeatTown(t, hookedWorkBD(t, "gt-p4aqut", time.Hour))
	tm.hasErr = errors.New("sandbox pod myr-mycat is gone")

	d.checkPolecatSessionHealth()
	clk.Advance(5 * time.Minute)
	d.checkPolecatSessionHealth()
	raised := seatUnreachableQueue(t, d)
	require.Len(t, raised, 1, "the seat must be named after five minutes")

	// The rig's seat list is now a file, not a directory: the pass cannot read
	// it, and the item it did not observe stays in the queue.
	polecats := filepath.Join(d.config.TownRoot, "myr", "polecats")
	require.NoError(t, os.RemoveAll(polecats))
	require.NoError(t, os.WriteFile(polecats, []byte("not a directory"), 0o644))
	clk.Advance(time.Minute)
	d.checkPolecatSessionHealth()

	require.Equal(t, raised, seatUnreachableQueue(t, d))
	if !strings.Contains(raised[0].Summary, "unreadable for 5m") {
		t.Errorf("the kept item moved on a pass that read no seat: %q", raised[0].Summary)
	}
}

// TestSeatUnreachableKeepsItsItemThroughASkippedPass: a seat the pass lists but
// leaves before its session check (its intent record says held) has no read for
// that tick, and no read answered for it, so the item it already raised keeps
// running from the same first failure rather than clearing (gt-q6hafz).
func TestSeatUnreachableKeepsItsItemThroughASkippedPass(t *testing.T) {
	t.Parallel()
	gone := errors.New("sandbox pod myr-mycat is gone")
	d, tm, clk := unreachableSeatTown(t, hookedWorkBD(t, "gt-p4aqut", time.Hour))
	tm.hasErr = gone

	d.checkPolecatSessionHealth()
	clk.Advance(5 * time.Minute)
	d.checkPolecatSessionHealth()
	require.Len(t, seatUnreachableQueue(t, d), 1, "the seat must be named after five minutes")

	// The seat is parked: this pass never asks the session, and the seat's last
	// answer (its bead and its error) is all the tracker has to go on.
	seatFrozen(t, d.config.TownRoot)
	clk.Advance(time.Minute)
	d.checkPolecatSessionHealth()

	kept := seatUnreachableQueue(t, d)
	require.Len(t, kept, 1, "a pass that skipped the seat's check cleared its running item")
	require.Equal(t, "seat-unreachable:myr/mycat", kept[0].Key)
	require.Equal(t, "gt-p4aqut", kept[0].Bead)
	require.Equal(t, "myr/mycat unreadable for 6m, holding gt-p4aqut: "+gone.Error(), kept[0].Summary,
		"the kept item must run on from the first failure and keep the last error")
}

// TestSeatUnreachableClearsItsItemWhenTheSeatAnswersAgain: a seat whose item ran
// through skipped passes clears on the first session check that answers, so the
// carried item is no news rather than a latch (gt-q6hafz).
func TestSeatUnreachableClearsItsItemWhenTheSeatAnswersAgain(t *testing.T) {
	t.Parallel()
	d, tm, clk := unreachableSeatTown(t, hookedWorkBD(t, "gt-p4aqut", time.Hour))
	tm.hasErr = errors.New("sandbox pod myr-mycat is gone")

	d.checkPolecatSessionHealth()
	clk.Advance(5 * time.Minute)
	d.checkPolecatSessionHealth()
	seatFrozen(t, d.config.TownRoot)
	clk.Advance(time.Minute)
	d.checkPolecatSessionHealth()
	require.Len(t, seatUnreachableQueue(t, d), 1, "the item must be running before the seat answers")

	// The seat is resumed and its session answers: a good read clears it.
	if _, err := intent.Update(d.config.TownRoot, intent.Seat{Rig: "myr", Role: constants.RolePolecat, Name: "mycat"}, func(r *intent.Record) error {
		r.Frozen = false
		return nil
	}); err != nil {
		t.Fatalf("releasing the seat: %v", err)
	}
	tm.hasErr = nil
	clk.Advance(time.Minute)
	d.checkPolecatSessionHealth()

	require.Empty(t, seatUnreachableQueue(t, d), "a good read after a skipped pass must clear the item")
}

// TestSeatUnreachableRaisesNothingForASeatPausedFromTheStart: a seat skipped on
// every pass that never answered has no outage to carry, so it raises nothing;
// a paused seat is not an unreachable one (gt-q6hafz).
func TestSeatUnreachableRaisesNothingForASeatPausedFromTheStart(t *testing.T) {
	t.Parallel()
	d, tm, clk := unreachableSeatTown(t, hookedWorkBD(t, "gt-p4aqut", time.Hour))
	tm.hasErr = errors.New("sandbox pod myr-mycat is gone")
	seatFrozen(t, d.config.TownRoot)

	d.checkPolecatSessionHealth()
	clk.Advance(5 * time.Minute)
	d.checkPolecatSessionHealth()
	clk.Advance(5 * time.Minute)
	d.checkPolecatSessionHealth()

	require.Empty(t, seatUnreachableQueue(t, d), "a seat no pass ever checked must not be named unreachable")
}

// TestSeatUnreachableClearsItsItemWhenTheSeatIsGone: a seat that leaves the
// rig's seat list is released, and its item clears on that pass, so the carried
// outage does not outlive the seat (gt-q6hafz).
func TestSeatUnreachableClearsItsItemWhenTheSeatIsGone(t *testing.T) {
	t.Parallel()
	d, tm, clk := unreachableSeatTown(t, hookedWorkBD(t, "gt-p4aqut", time.Hour))
	tm.hasErr = errors.New("sandbox pod myr-mycat is gone")

	d.checkPolecatSessionHealth()
	clk.Advance(5 * time.Minute)
	d.checkPolecatSessionHealth()
	require.Len(t, seatUnreachableQueue(t, d), 1, "the seat must be named after five minutes")

	require.NoError(t, os.RemoveAll(filepath.Join(d.config.TownRoot, "myr", "polecats", "mycat")))
	clk.Advance(time.Minute)
	d.checkPolecatSessionHealth()

	require.Empty(t, seatUnreachableQueue(t, d), "a released seat must clear its item")
}

// TestSeatUnreachableClearsWhenThePatrolIsOff: the items are the polecat-health
// pass's, so a patched-off pass raises none and the queue clears them rather
// than holding a seat the daemon no longer checks (gt-q6hafz).
func TestSeatUnreachableClearsWhenThePatrolIsOff(t *testing.T) {
	t.Parallel()
	d, tm, clk := unreachableSeatTown(t, hookedWorkBD(t, "gt-p4aqut", time.Hour))
	tm.hasErr = errors.New("sandbox pod myr-mycat is gone")

	d.checkPolecatSessionHealth()
	clk.Advance(5 * time.Minute)
	d.checkPolecatSessionHealth()
	require.Len(t, seatUnreachableQueue(t, d), 1, "the seat must be named after five minutes")

	d.disabledPatrols = map[string]bool{"polecat-health": true}
	require.Empty(t, seatUnreachableQueue(t, d), "a patrol that is off must not keep raising its items")
}

// TestSeatUnreachableRaisesWithoutABeadItCouldNotRead: a seat whose work-bead
// read fails is still named for its unreadable session, with no bead read
// rather than a guessed one, so a bead read that cannot answer does not silence
// the alarm (gt-q6hafz).
func TestSeatUnreachableRaisesWithoutABeadItCouldNotRead(t *testing.T) {
	t.Parallel()
	bd := hookedWorkBD(t, "gt-p4aqut", time.Hour)
	bd.listErr = errors.New("dolt: connection refused")
	d, tm, clk := unreachableSeatTown(t, bd)
	tm.hasErr = errors.New("sandbox pod myr-mycat is gone")

	d.checkPolecatSessionHealth()
	clk.Advance(5 * time.Minute)
	d.checkPolecatSessionHealth()

	raised := seatUnreachableQueue(t, d)
	require.Len(t, raised, 1)
	require.Equal(t, "", raised[0].Bead)
	require.Contains(t, raised[0].Summary, "holding no bead read")
}
