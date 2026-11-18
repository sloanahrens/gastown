package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// tripwireDaemon is a guard whose split_added_lines is limit, live or dry.
func tripwireDaemon(t *testing.T, dryRun bool, limit int) (*Daemon, *bytes.Buffer) {
	t.Helper()
	d, buf := newOverrunDaemon(t, dryRun)
	d.patrolConfig.Patrols.OverrunGuard.SplitAddedLines = limit
	return d, buf
}

// TestRunawayBeadTripwireAtTheMarkStops: a seat the guard marks whose worktree
// adds at least split_added_lines lines is stopped on that tick, before the 20
// minute threshold, and the stop carries the tripwire's reason into the saved
// state and the bead's hand-back note (gt-uj5a1i).
func TestRunawayBeadTripwireAtTheMarkStops(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lines int
		limit int
	}{
		{"over the limit", 350, 300},
		{"at the limit", 300, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := tripwireDaemon(t, false, tc.limit)
			f := newOverrunFake(12*time.Minute, stopSeat)
			seams := f.seams()
			counted := 0
			seams.addedLines = func(seat overrunSeat) (int, error) {
				if seat != stopSeat {
					t.Errorf("added lines counted for %+v, want the seat %+v", seat, stopSeat)
				}
				counted++
				return tc.lines, nil
			}

			d.runOverrunGuard(seams)

			if len(f.stops) != 1 || f.stops[0] != "gastown/capable" {
				t.Fatalf("stops = %v, want one of gastown/capable", f.stops)
			}
			if counted != 1 {
				t.Errorf("added lines counted %d times, want once at the mark", counted)
			}
			want := fmt.Sprintf("tripwire: %d added lines at 12m0s, at or over split_added_lines %d; split the bead", tc.lines, tc.limit)
			if len(f.handBackNotes) != 1 || f.handBackNotes[0] != want+"." {
				t.Errorf("hand-back notes = %q, want %q", f.handBackNotes, want+".")
			}
			st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead)
			if err != nil {
				t.Fatalf("readOverrunState: %v", err)
			}
			if st.StopReason != want {
				t.Errorf("saved stop reason = %q, want %q", st.StopReason, want)
			}
			if st.NudgedAt.IsZero() {
				t.Error("the stop began with no mark saved")
			}
		})
	}
}

// TestRunawayBeadTripwireLeavesTheMark: a count under the limit, a limit of 0
// and an unset seam each leave the marked seat running, with the mark saved and
// no stop. The count is read only when a positive limit meets a set seam, so a
// town with the tripwire off reads no worktree at all (gt-uj5a1i).
func TestRunawayBeadTripwireLeavesTheMark(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		limit       int
		seam        func(overrunSeat) (int, error)
		wantCounted bool
	}{
		{name: "under the limit", limit: 300, seam: func(overrunSeat) (int, error) { return 250, nil }, wantCounted: true},
		{name: "limit off", limit: 0, seam: func(overrunSeat) (int, error) { return 350, nil }},
		{name: "no seam", limit: 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := tripwireDaemon(t, false, tc.limit)
			f := newOverrunFake(12*time.Minute, stopSeat)
			seams := f.seams()
			counted := false
			if tc.seam != nil {
				seams.addedLines = func(seat overrunSeat) (int, error) {
					counted = true
					return tc.seam(seat)
				}
			}

			d.runOverrunGuard(seams)

			if counted != tc.wantCounted {
				t.Errorf("counted = %v, want %v", counted, tc.wantCounted)
			}
			if len(f.stops) != 0 || len(f.handedBack) != 0 {
				t.Errorf("stops %v, hand-backs %v; want neither", f.stops, f.handedBack)
			}
			st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead)
			if err != nil {
				t.Fatalf("readOverrunState: %v", err)
			}
			if want := overrunSlung.Add(12 * time.Minute); !st.NudgedAt.Equal(want) {
				t.Errorf("mark = %v, want %v", st.NudgedAt, want)
			}
		})
	}
}

// TestRunawayBeadTripwireCountFailsLeavesTheMark: a count that fails is logged
// and alerted, and stops nothing. The mark is already saved, so the seat runs
// on to the 20 minute threshold (gt-uj5a1i); the alert is what keeps the blind
// tripwire from living in the log alone (gt-eb7bzu).
func TestRunawayBeadTripwireCountFailsLeavesTheMark(t *testing.T) {
	t.Parallel()
	d, buf := tripwireDaemon(t, false, 300)
	f := newOverrunFake(12*time.Minute, stopSeat)
	seams := f.seams()
	seams.addedLines = func(overrunSeat) (int, error) {
		return 0, errors.New("git diff: no such ref")
	}
	var alerted []string
	seams.countFailed = func(seat overrunSeat, err error) error {
		if seat != stopSeat {
			t.Errorf("count failure alerted for %+v, want the seat %+v", seat, stopSeat)
		}
		alerted = append(alerted, err.Error())
		return nil
	}

	d.runOverrunGuard(seams)

	if len(f.stops) != 0 || len(f.handedBack) != 0 {
		t.Errorf("stops %v, hand-backs %v; want neither", f.stops, f.handedBack)
	}
	log := buf.String()
	for _, want := range []string{"cannot count the lines added on gt-aaa11", "git diff: no such ref"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if want := []string{"git diff: no such ref"}; !slices.Equal(alerted, want) {
		t.Errorf("count failure alerted %q, want %q", alerted, want)
	}
	st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead)
	if err != nil {
		t.Fatalf("readOverrunState: %v", err)
	}
	if want := overrunSlung.Add(12 * time.Minute); !st.NudgedAt.Equal(want) {
		t.Errorf("mark = %v, want %v", st.NudgedAt, want)
	}
}

// TestRunawayBeadTripwireDryRunLogsOnly: a dry run over a seat past the
// tripwire logs the stop it would make and writes nothing (gt-uj5a1i).
func TestRunawayBeadTripwireDryRunLogsOnly(t *testing.T) {
	t.Parallel()
	d, buf := tripwireDaemon(t, true, 300)
	f := newOverrunFake(12*time.Minute, stopSeat)
	seams := f.seams()
	seams.addedLines = func(overrunSeat) (int, error) { return 350, nil }

	d.runOverrunGuard(seams)

	if len(f.nudges) != 0 || len(f.stops) != 0 || len(f.handedBack) != 0 {
		t.Errorf("dry run acted: nudges %v, stops %v, hand-backs %v", f.nudges, f.stops, f.handedBack)
	}
	want := "dry run: would stop gastown/capable on gt-aaa11 for the tripwire: " + tripwireReason
	if !strings.Contains(buf.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, buf.String())
	}
	if st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead); err != nil || !st.NudgedAt.IsZero() {
		t.Errorf("dry run wrote state %+v (err %v), want none", st, err)
	}
}

// TestRunawayBeadTripwireDryRunCountFailureLogsOnly: a dry run whose count
// fails logs it as a dry run, claiming no mark it never wrote, and raises no
// alert, because a dry run writes nothing (gt-eb7bzu).
func TestRunawayBeadTripwireDryRunCountFailureLogsOnly(t *testing.T) {
	t.Parallel()
	d, buf := tripwireDaemon(t, true, 300)
	f := newOverrunFake(12*time.Minute, stopSeat)
	seams := f.seams()
	seams.addedLines = func(overrunSeat) (int, error) {
		return 0, errors.New("git diff: no such ref")
	}
	alerted := 0
	seams.countFailed = func(overrunSeat, error) error { alerted++; return nil }

	d.runOverrunGuard(seams)

	if len(f.nudges) != 0 || len(f.stops) != 0 || len(f.handedBack) != 0 {
		t.Errorf("dry run acted: nudges %v, stops %v, hand-backs %v", f.nudges, f.stops, f.handedBack)
	}
	if alerted != 0 {
		t.Errorf("dry run alerted %d times, want none: a dry run writes nothing", alerted)
	}
	log := buf.String()
	want := "dry run: gastown/capable: cannot count the lines added on gt-aaa11"
	if !strings.Contains(log, want) {
		t.Errorf("log lacks %q:\n%s", want, log)
	}
	if strings.Contains(log, "leaving the mark") {
		t.Errorf("dry-run count failure claims a mark that was never written:\n%s", log)
	}
	if st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead); err != nil || !st.NudgedAt.IsZero() {
		t.Errorf("dry run wrote state %+v (err %v), want none", st, err)
	}
}

// TestRunawayBeadTripwireCountsThroughTheProductionSeam: the tripwire weighs a
// seat through the production addedLines seam, the same handle selection the
// stop's save, dirty check, push and reset use, and counts its lines over
// origin/main. The count side is production and the side-effect seams are the
// fake, so the tripwire's production wiring is covered in the unit tier rather
// than only by the integration tier (gt-eb7bzu).
func TestRunawayBeadTripwireCountsThroughTheProductionSeam(t *testing.T) {
	t.Parallel()
	d, _ := tripwireDaemon(t, false, 300)
	seedHostWorktreeStub(t, d)
	handle := &addedLinesStubGit{lines: 350}
	d.openGitFn = func(string) daemonGit { return handle }

	f := newOverrunFake(12*time.Minute, stopSeat)
	seams := f.seams()
	prod := d.overrunProdSeams()
	seams.addedLines = prod.addedLines
	seams.countFailed = prod.countFailed

	d.runOverrunGuard(seams)

	if len(f.stops) != 1 || f.stops[0] != "gastown/capable" {
		t.Fatalf("stops = %v, want one of gastown/capable", f.stops)
	}
	if len(f.handBackNotes) != 1 || f.handBackNotes[0] != tripwireReason+"." {
		t.Errorf("hand-back notes = %q, want %q", f.handBackNotes, tripwireReason+".")
	}
	if want := []string{"origin/main"}; !slices.Equal(handle.bases, want) {
		t.Errorf("production seam counted added lines over %q, want %q", handle.bases, want)
	}
}

// TestRunawayBeadTripwireCountFailureAlertsThroughTheProductionSeam: a count
// the production seat-handle seam cannot read alerts through the production
// countFailed closure under the seat-and-bead key, with the guard as the
// source and the failure in the reason. The failed count changes no decision:
// the mark stands and the seat is not stopped (gt-eb7bzu).
func TestRunawayBeadTripwireCountFailureAlertsThroughTheProductionSeam(t *testing.T) {
	t.Parallel()
	d, _ := tripwireDaemon(t, false, 300)
	rec := notifyfake.New()
	d.notifier = rec
	d.overrunSeatGitFn = func(overrunSeat) (daemonGit, error) {
		return nil, errors.New("no such worktree")
	}

	f := newOverrunFake(12*time.Minute, stopSeat)
	seams := f.seams()
	prod := d.overrunProdSeams()
	seams.addedLines = prod.addedLines
	seams.countFailed = prod.countFailed

	d.runOverrunGuard(seams)

	if len(f.stops) != 0 || len(f.handedBack) != 0 {
		t.Errorf("stops %v, hand-backs %v; want neither: a failed count changes no decision", f.stops, f.handedBack)
	}
	st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead)
	if err != nil {
		t.Fatalf("readOverrunState: %v", err)
	}
	if want := overrunSlung.Add(12 * time.Minute); !st.NudgedAt.Equal(want) {
		t.Errorf("mark = %v, want %v: the mark stands", st.NudgedAt, want)
	}
	got := rec.Escalations()
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want one", got)
	}
	wantReason := "the overrun guard could not count the lines gastown/capable added on " + stopSeat.Bead +
		", so the split_added_lines tripwire cannot weigh the seat; the mark stands and the seat runs on to the stop threshold: no such worktree"
	e := got[0].Escalation
	if want := "overrun-count-failed:gastown/capable:" + stopSeat.Bead; e.Fingerprint != want {
		t.Errorf("alert key = %q, want %q", e.Fingerprint, want)
	}
	if e.Reason != wantReason {
		t.Errorf("alert reason = %q, want %q", e.Reason, wantReason)
	}
	if e.Severity != "HIGH" {
		t.Errorf("alert severity = %q, want HIGH", e.Severity)
	}
	if want := escalationTitle(overrunActor, wantReason); e.Description != want {
		t.Errorf("alert description = %q, want %q", e.Description, want)
	}
}

// TestRunawayBeadTripwireAlertDeliveryFailureIsLogged: when the production
// alert cannot be delivered, the guard logs that the seat is blind and the
// track standing, rather than reporting a blind tripwire as alerted. The
// decision is still unchanged: the mark stands and the seat is not stopped
// (gt-eb7bzu).
func TestRunawayBeadTripwireAlertDeliveryFailureIsLogged(t *testing.T) {
	t.Parallel()
	d, buf := tripwireDaemon(t, false, 300)
	rec := notifyfake.New()
	rec.Fail(notifyfake.KindEscalate, errors.New("gt escalate: exit status 1"))
	d.notifier = rec
	clk := newFixedClock()
	d.clock = clk
	d.overrunSeatGitFn = func(overrunSeat) (daemonGit, error) {
		return nil, errors.New("no such worktree")
	}

	f := newOverrunFake(12*time.Minute, stopSeat)
	seams := f.seams()
	prod := d.overrunProdSeams()
	seams.addedLines = prod.addedLines
	seams.countFailed = prod.countFailed

	runOnClock(t, clk, time.Second, func() { d.runOverrunGuard(seams) })

	if len(f.stops) != 0 || len(f.handedBack) != 0 {
		t.Errorf("stops %v, hand-backs %v; want neither: a failed alert changes no decision", f.stops, f.handedBack)
	}
	st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead)
	if err != nil {
		t.Fatalf("readOverrunState: %v", err)
	}
	if want := overrunSlung.Add(12 * time.Minute); !st.NudgedAt.Equal(want) {
		t.Errorf("mark = %v, want %v: the mark stands", st.NudgedAt, want)
	}
	want := "the count-failure alert on " + stopSeat.Bead + " was not delivered: gt escalate: exit status 1"
	if log := buf.String(); !strings.Contains(log, want) {
		t.Errorf("log lacks %q:\n%s", want, log)
	}
	if got := rec.Escalations(); len(got) != maxEscalationRetries {
		t.Errorf("escalate attempts = %d, want %d: delivery was retried, not skipped", len(got), maxEscalationRetries)
	}
}
