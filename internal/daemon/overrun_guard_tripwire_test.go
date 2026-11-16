package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
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
// and stops nothing. The mark is already saved, so the seat runs on to the 20
// minute threshold (gt-uj5a1i).
func TestRunawayBeadTripwireCountFailsLeavesTheMark(t *testing.T) {
	t.Parallel()
	d, buf := tripwireDaemon(t, false, 300)
	f := newOverrunFake(12*time.Minute, stopSeat)
	seams := f.seams()
	seams.addedLines = func(overrunSeat) (int, error) {
		return 0, errors.New("git diff: no such ref")
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
