package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/polecat/spans"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// stopSeat is the seat the stop tests act on.
var stopSeat = overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11", Branch: "polecat/capable/gt-aaa11+x1"}

func TestStop_OrderIsPauseStopCheckpointHandBack(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)

	d.runOverrunGuard(f.seams())

	want := []string{
		"pause gastown/capable",
		"stop gastown/capable",
		"checkpoint gastown/capable",
		"push gastown/capable",
		"hand-back gastown/capable",
		"recheck gastown/capable", // the seat is still ours before the release
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("call order = %q, want %q (push before the hand-back, reset then cleanup then retire after it)", f.calls, want)
	}
	if len(f.pushed) != 1 || f.pushed[0] != "gt-aaa11 polecat/capable/gt-aaa11+x1" {
		t.Errorf("push got %q, want the bead and its branch", f.pushed)
	}
	if len(f.agentBeadsReset) != 1 || f.agentBeadsReset[0] != "gt-aaa11" {
		t.Errorf("agent bead reset for %q, want gt-aaa11", f.agentBeadsReset)
	}
	if len(f.handedBack) != 1 || f.handedBack[0] != "gt-aaa11 polecat/capable/gt-aaa11+x1 20m0s" {
		t.Errorf("hand-back got %q, want the bead, its branch and 20m0s", f.handedBack)
	}
	st, err := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if err != nil {
		t.Fatal(err)
	}
	if st.StoppedAt.IsZero() {
		t.Error("StoppedAt not persisted after the hand-back")
	}
}

// TestStop_ClosesTheAttemptItEnded: the 20-minute stop is what ended the run,
// so the attempt the sling opened is closed once the bead is handed back — at
// the moment the session was stopped, after which the seat did no work — with
// reason overrun (gt-dz78b).
func TestStop_ClosesTheAttemptItEnded(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.spanEnd = func(overrunSeat, time.Time) error { return nil }

	d.runOverrunGuard(f.seams())

	want := stopSeat.Bead + " " + f.now.Format(time.RFC3339)
	if len(f.spanEnds) != 1 || f.spanEnds[0] != want {
		t.Fatalf("span ends = %v, want %q", f.spanEnds, want)
	}
	handBack, spanEnd := -1, -1
	for i, c := range f.calls {
		switch {
		case strings.HasPrefix(c, "hand-back "):
			handBack = i
		case strings.HasPrefix(c, "span-end "):
			spanEnd = i
		}
	}
	if spanEnd < 0 || handBack < 0 || spanEnd < handBack {
		t.Errorf("calls = %q, want the span closed after the hand-back", f.calls)
	}
}

// TestSpanEndSeamWritesTheOverrunReason: the stop's own spanEnd closure is
// what writes the log, so it is checked against an attempt a sling opened: the
// end is at the stop, reason overrun, and the attempt counts as real time
// rather than a lower bound (gt-dz78b).
func TestSpanEndSeamWritesTheOverrunReason(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	began := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	store := spans.New(spans.DefaultDir(d.config.TownRoot), spanClock{at: began})
	if err := store.Start(stopSeat.Bead, stopSeat.Rig+"/"+stopSeat.Polecat); err != nil {
		t.Fatalf("start span: %v", err)
	}
	stopped := began.Add(20 * time.Minute)

	seams := d.overrunStopSeams(overrunSeams{})
	if seams.spanEnd == nil {
		t.Fatal("overrunStopSeams left spanEnd nil: nothing would close the attempt")
	}
	if err := seams.spanEnd(stopSeat, stopped); err != nil {
		t.Fatalf("spanEnd: %v", err)
	}

	sum, err := store.Summarize(stopSeat.Bead)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open || sum.Attempts[0].LowerBound {
		t.Fatalf("summary = %+v, want one closed, measured attempt", sum)
	}
	if got, want := sum.Attempts[0].Secs, (20 * time.Minute).Seconds(); got != want {
		t.Errorf("attempt secs = %v, want %v", got, want)
	}
}

// A stop that gave up on the push still hands the bead back, so it still ends
// the attempt: the seat is stopped either way, and leaving the span open would
// read as work in progress on a bead nobody holds.
func TestStop_GivenUpPushStillClosesTheAttempt(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"push": &overrunPushDivergedError{
		Branch: stopSeat.Branch, OriginSHA: "9f8e7d6", Err: errors.New("rejected (non-fast-forward)"),
	}}
	f.spanEnd = func(overrunSeat, time.Time) error { return nil }

	d.runOverrunGuard(f.seams())

	if len(f.spanEnds) != 1 {
		t.Errorf("span ends = %v, want the attempt closed after a given-up push", f.spanEnds)
	}
}

// TestStop_SurvivesAFailedSpanClose: the span is a measurement, not a step — a
// log that cannot be written is logged and the stop runs to its end.
func TestStop_SurvivesAFailedSpanClose(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.spanEnd = func(overrunSeat, time.Time) error { return errors.New("read-only file system") }

	d.runOverrunGuard(f.seams())

	st, err := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if err != nil {
		t.Fatal(err)
	}
	if st.StoppedAt.IsZero() {
		t.Error("stop not marked finished after a failed span write")
	}
	if !strings.Contains(buf.String(), "span not closed") {
		t.Errorf("log lacks the failed span write:\n%s", buf.String())
	}
}

// The run the stop ended is recorded on the bead before it is handed back. The
// record must land while the bead is still hooked here: written afterwards, it
// could date a re-sling that reached a new seat in between as the stopped run,
// and that seat's clock would never start (gt-tzbtf).
func TestStop_EndsTheRunBeforeTheHandBack(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	seams := f.seams()
	var order []string
	endRun, handBack := seams.endRun, seams.handBack
	seams.endRun = func(seat overrunSeat, at time.Time) error {
		order = append(order, "end-run")
		return endRun(seat, at)
	}
	seams.handBack = func(seat overrunSeat, elapsed time.Duration, note string) error {
		order = append(order, "hand-back")
		return handBack(seat, elapsed, note)
	}

	d.runOverrunGuard(seams)

	if len(f.endRuns) != 1 || f.endRuns[0] != stopSeat.Bead {
		t.Fatalf("run ends = %v, want one for %s", f.endRuns, stopSeat.Bead)
	}
	if want := []string{"end-run", "hand-back"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// A run end that cannot be recorded holds the hand-back: handing the bead back
// with its old clock intact would leave the next dispatch unpoliced, so the stop
// stays pending and the next tick records the end and hands it back (gt-tzbtf).
func TestStop_FailedRunEndHoldsTheHandBack(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.endRunErr = errors.New("events log is read-only")

	d.runOverrunGuard(f.seams())

	if len(f.handedBack) != 0 {
		t.Errorf("handed back %q although the run's end was not recorded", f.handedBack)
	}
	st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead)
	if err != nil {
		t.Fatal(err)
	}
	if !st.StoppedAt.IsZero() {
		t.Error("stop marked finished although the run's end was not recorded")
	}
	if !strings.Contains(buf.String(), "the run's end could not be recorded") {
		t.Errorf("log lacks the failed run-end record:\n%s", buf.String())
	}

	f.endRunErr = nil
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q, want the retried stop to hand the bead back", f.handedBack)
	}
}

// The stop's own endRun closure is what writes the record, so it is checked
// against the town's dispatch clock: the dispatch after the stop is the bead's
// clock, not the sling the stopped seat ran on, so the handed-back bead is
// policed afresh on its next run (gt-tzbtf). The record is stamped with the end
// the stop saved, not with when the record is written, so the same stop retried
// on a later tick cannot move the boundary past the new dispatch.
func TestEndRunSeamDatesTheDispatchAfterTheStopAfresh(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	slingLine := func(at time.Time) string {
		return fmt.Sprintf(`{"ts":%q,"source":"gt","type":"sling","actor":"x","payload":{"bead":%q,"target":"gastown/polecats/%s"}}`,
			at.UTC().Format(time.RFC3339), stopSeat.Bead, stopSeat.Polecat)
	}
	logPath := filepath.Join(d.config.TownRoot, events.EventsFile)
	first := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(logPath, []byte(slingLine(first)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	seams := d.overrunStopSeams(overrunSeams{})
	if seams.endRun == nil {
		t.Fatal("overrunStopSeams left endRun nil: nothing would end the run")
	}
	stopped := first.Add(20 * time.Minute)
	if err := seams.endRun(stopSeat, stopped); err != nil {
		t.Fatalf("endRun: %v", err)
	}
	// A retried stop writes the record again, later, for the same end.
	if err := seams.endRun(stopSeat, stopped); err != nil {
		t.Fatalf("endRun again: %v", err)
	}
	// A dispatch after the stop, which the stop's own record must make the new
	// run's clock.
	resling := stopped.Add(time.Minute)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(slingLine(resling) + "\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, ok := (&overrunSlungReader{d: d}).slungAt(stopSeat)
	if !ok || !got.Equal(resling) {
		t.Errorf("slungAt = %v (%t), want the dispatch after the stop, %v", got, ok, resling)
	}
}

// A seat that cannot be retired after the hand-back stays parked, so the stop
// stays pending: the next tick repeats the hand-back and the release instead of
// finishing with the park unannounced.
func TestStop_FailedReleaseAfterHandBackKeepsTheStopPending(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"retire": errors.New("disk full")}

	d.runOverrunGuard(f.seams())

	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if !st.StoppedAt.IsZero() {
		t.Error("StoppedAt written although the seat is still parked")
	}
	if !st.SlingAt.Equal(overrunSlung) {
		t.Errorf("SlingAt = %v, want the sling %v saved for the resumed stop", st.SlingAt, overrunSlung)
	}
	if !strings.Contains(buf.String(), "seat not retired after the hand-back") {
		t.Errorf("log lacks the failed release:\n%s", buf.String())
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	want := []string{"recheck gastown/capable", "hand-back gastown/capable", "recheck gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("resumed call order = %q, want %q", f.calls, want)
	}
	st, _ = readOverrunState(d.config.TownRoot, "gt-aaa11")
	if st.StoppedAt.IsZero() {
		t.Error("StoppedAt not persisted once the seat was released")
	}
}

func TestStop_FailedSessionStopClearsMarkerAndSkipsHandBack(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute, stopSeat)
	f.failOn = map[string]error{"stop": errors.New("tmux refused")}

	d.runOverrunGuard(f.seams())

	want := []string{"pause gastown/capable", "stop gastown/capable", "clear-pause gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("call order = %q, want %q (marker cleared, nothing after the failed stop)", f.calls, want)
	}
	if len(f.handedBack) != 0 {
		t.Errorf("bead handed back after a failed stop: %q", f.handedBack)
	}
	if !strings.Contains(buf.String(), "session stop failed: tmux refused") {
		t.Errorf("log lacks the stop failure:\n%s", buf.String())
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if !st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
		t.Errorf("state %+v still records a stop under way", st)
	}

	// The next tick starts over from the pause, never from the middle.
	f.calls, f.failOn = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.calls) != 9 || f.calls[0] != "pause gastown/capable" || f.calls[4] != "hand-back gastown/capable" || f.calls[8] != "retire gastown/capable" {
		t.Errorf("retry call order = %q, want the full sequence", f.calls)
	}
}

func TestStop_FailedMarkerClearKeepsTheStopPending(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute, stopSeat)
	f.failOn = map[string]error{"stop": errors.New("tmux refused"), "clear-pause": errors.New("disk full")}

	d.runOverrunGuard(f.seams())
	// The seat is parked now, so the working-seat list no longer shows it; the
	// saved state is what brings the stop back.
	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	// The resumed stop looks at the seat before and after stopping the session.
	want := []string{"recheck gastown/capable", "stop gastown/capable", "recheck gastown/capable", "checkpoint gastown/capable", "push gastown/capable", "hand-back gastown/capable", "recheck gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("resumed call order = %q, want %q (no second pause)", f.calls, want)
	}
}

// A stop begun on an earlier tick but not past stopping the session is resumed
// with StopBeganAt set and SessionStoppedAt zero.
func TestOverrunStop(t *testing.T) {
	t.Parallel()

	// A pause marker that could not be cleared after a failed session stop
	// leaves exactly that. If an operator ran gt agent resume and a new session
	// started meanwhile, the resumed stop looks at the seat before it stops the
	// session, and ends without killing it or handing the bead back.
	t.Run("resumed before the session stop keeps a new session", func(t *testing.T) {
		t.Parallel()
		d, buf := newOverrunDaemon(t, false)
		f := newOverrunFake(21*time.Minute, stopSeat)
		f.failOn = map[string]error{"stop": errors.New("tmux refused"), "clear-pause": errors.New("disk full")}
		d.runOverrunGuard(f.seams())

		st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
		if st.StopBeganAt.IsZero() || !st.SessionStoppedAt.IsZero() {
			t.Fatalf("state %+v, want a stop begun with the session not stopped", st)
		}

		f.seats, f.calls, f.failOn = nil, nil, nil
		f.lapsed = "the pause marker is gone"
		f.now = f.now.Add(time.Minute)
		d.runOverrunGuard(f.seams())

		if want := []string{"recheck gastown/capable"}; !reflect.DeepEqual(f.calls, want) {
			t.Errorf("calls = %q, want only the re-check (the new session is not stopped, nothing handed back)", f.calls)
		}
		if len(f.handedBack) != 0 {
			t.Errorf("handed back a bead whose seat was taken back: %q", f.handedBack)
		}
		if !strings.Contains(buf.String(), "abandoning the stop") {
			t.Errorf("log lacks the abandon line:\n%s", buf.String())
		}
		if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() {
			t.Error("StoppedAt not saved; the next tick would resume the abandoned stop")
		}
	})

	// A failed session stop clears the pause and saves the stop as not begun.
	// When that save fails the saved stop stays begun, and a begun stop with no
	// pause marker is what an operator resume looks like: the pause goes back so
	// the next tick stops the session instead of abandoning the stop.
	t.Run("failed save after a failed session stop restores the pause", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		f := newOverrunFake(21*time.Minute, stopSeat)
		f.failOn = map[string]error{"stop": errors.New("tmux refused")}
		s := f.seams()
		stop := s.stopSession
		s.stopSession = func(seat overrunSeat) error {
			// Make the next save fail: a directory where the state file goes.
			path, err := overrunStatePath(d.config.TownRoot, seat.Bead)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			return stop(seat)
		}
		d.runOverrunGuard(s)

		want := []string{"pause gastown/capable", "stop gastown/capable", "clear-pause gastown/capable", "pause gastown/capable"}
		if !reflect.DeepEqual(f.calls, want) {
			t.Errorf("calls = %q, want %q (the pause put back once its clearing could not be saved)", f.calls, want)
		}
	})
}

func TestStop_RunsOnceAfterStoppedAt(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)

	d.runOverrunGuard(f.seams())
	first := len(f.calls)
	if first != 9 {
		t.Fatalf("first tick made %d calls (%q), want 9", first, f.calls)
	}

	// The seat is still listed (say the session came back): the stop is done
	// for this sling and must not run again.
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.calls) != first {
		t.Errorf("next tick repeated the stop: %q", f.calls[first:])
	}

	// A restart is a new Daemon over the same town.
	d2, _ := newOverrunDaemon(t, false)
	d2.config.TownRoot = d.config.TownRoot
	d2.runOverrunGuard(f.seams())
	if len(f.calls) != first {
		t.Errorf("a restarted daemon repeated the stop: %q", f.calls[first:])
	}
}

func TestStop_ReslingAfterStopRunsAFreshStop(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	d.runOverrunGuard(f.seams())

	resling := overrunSlung.Add(40 * time.Minute)
	f.slung["gt-aaa11"] = resling
	f.calls = nil
	f.now = resling.Add(21 * time.Minute)
	d.runOverrunGuard(f.seams())

	if len(f.calls) != 9 {
		t.Errorf("calls for the new sling = %q, want a full stop", f.calls)
	}
}

func TestStop_FailedCheckpointRetriesWithoutRepeatingEarlierSteps(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("git busy")}

	d.runOverrunGuard(f.seams())
	want := []string{"pause gastown/capable", "stop gastown/capable", "checkpoint gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %q, want %q (no hand-back past a failed checkpoint)", f.calls, want)
	}
	if !strings.Contains(buf.String(), "checkpoint failed (1 of 5): git busy") {
		t.Errorf("log lacks the checkpoint failure:\n%s", buf.String())
	}

	// The session is gone, so the seat is no longer listed; the record
	// carries the retry.
	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	want = []string{"recheck gastown/capable", "checkpoint gastown/capable", "push gastown/capable", "hand-back gastown/capable", "recheck gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("retry calls = %q, want %q", f.calls, want)
	}
	// The elapsed time is the one measured when the stop began.
	if len(f.handedBack) != 1 || !strings.HasSuffix(f.handedBack[0], " 22m0s") {
		t.Errorf("hand-back %q, want elapsed 22m0s from the first tick", f.handedBack)
	}
}

func TestStop_FailedHandBackRetriesAndStaysUnfinished(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"hand-back": errors.New("bd down")}

	d.runOverrunGuard(f.seams())
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if !st.StoppedAt.IsZero() {
		t.Fatalf("StoppedAt set although the hand-back failed: %+v", st)
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if want := []string{"recheck gastown/capable", "hand-back gastown/capable", "recheck gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("retry calls = %q, want the hand-back and the release", f.calls)
	}
	st, _ = readOverrunState(d.config.TownRoot, "gt-aaa11")
	if st.StoppedAt.IsZero() {
		t.Error("StoppedAt not set after the retried hand-back")
	}
}

func TestStop_FailedPauseDoesNothingElse(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(25*time.Minute, stopSeat)
	f.failOn = map[string]error{"pause": errors.New("no intent dir")}

	d.runOverrunGuard(f.seams())

	if want := []string{"pause gastown/capable"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %q, want only the failed pause", f.calls)
	}
}

func TestStop_DryRunCallsNoStopSeam(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, true)
	f := newOverrunFake(30*time.Minute, stopSeat)

	d.runOverrunGuard(f.seams())

	if len(f.calls) != 0 {
		t.Errorf("dry run called stop seams: %q", f.calls)
	}
}

// seedStopSandbox pins the rig prefix the stop's sandbox lookup reads and writes
// the record of stopSeat's sandboxed session.
func seedStopSandbox(t *testing.T, d *Daemon) {
	t.Helper()
	reg := session.NewPrefixRegistry()
	reg.Register(stopSeat.Rig, overrunStopPrefix)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	rec := session.SandboxRecord{Session: overrunStopSessionName, Rig: stopSeat.Rig, Polecat: stopSeat.Polecat, Branch: stopSeat.Branch}
	if err := session.WriteSandboxRecord(d.config.TownRoot, rec); err != nil {
		t.Fatal(err)
	}
}

// The sandbox of a stopped seat is removed after its session stop and before
// the checkpoint, the order the session manager's stop ran it in (gt-ft2vg).
func TestStop_SandboxRemovalIsOrderedAfterTheStopBeforeTheCheckpoint(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }

	d.runOverrunGuard(f.seams())

	want := []string{
		"pause gastown/capable",
		"stop gastown/capable",
		"sandbox gastown/capable",
		"checkpoint gastown/capable",
		"push gastown/capable",
		"hand-back gastown/capable",
		"recheck gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("call order = %q, want %q", f.calls, want)
	}
}

// A refused sandbox push holds the stop: the sandbox and its record are kept,
// the seat stays paused, the bead is not handed back, and the log names the
// sandbox. The next tick retries the removal and finishes the stop (gt-ft2vg).
func TestStop_RefusedSandboxPushHoldsTheStopAndNamesTheSandbox(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	f := newOverrunFake(20*time.Minute, stopSeat)
	var deleted []string
	refuse := func(string, session.SandboxRecord) error { return errors.New("rejected by origin") }
	f.removeSandbox = func(seat overrunSeat) error {
		return d.overrunRemoveSandbox(seat, refuse, func(_ string, rec session.SandboxRecord) error {
			deleted = append(deleted, rec.Session)
			return nil
		})
	}

	d.runOverrunGuard(f.seams())

	want := []string{"pause gastown/capable", "stop gastown/capable", "sandbox gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %q, want %q (no checkpoint, push or hand-back past a refused sandbox push)", f.calls, want)
	}
	if len(deleted) != 0 {
		t.Errorf("sandbox deleted despite the refused push: %q", deleted)
	}
	if len(f.handedBack) != 0 {
		t.Errorf("bead handed back with its sandbox unpushed: %q", f.handedBack)
	}
	if !strings.Contains(buf.String(), "waits, sandbox kept") || !strings.Contains(buf.String(), "sandbox "+overrunStopSessionName) {
		t.Errorf("log does not name the kept sandbox:\n%s", buf.String())
	}
	if _, ok, _ := session.ReadSandboxRecord(d.config.TownRoot, overrunStopSessionName); !ok {
		t.Error("the sandbox record was removed despite the refused push")
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if st.SessionStoppedAt.IsZero() || st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
		t.Errorf("state %+v: want the session recorded stopped and the stop still under way", st)
	}

	// The seat is gone and the record carries the stop: the next tick pushes
	// again, and the stop finishes without stopping the session a second time.
	f.seats, f.calls = nil, nil
	f.removeSandbox = func(overrunSeat) error { return nil }
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	want = []string{
		"recheck gastown/capable",
		"sandbox gastown/capable",
		"checkpoint gastown/capable",
		"push gastown/capable",
		"hand-back gastown/capable",
		"recheck gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("retry calls = %q, want %q", f.calls, want)
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-back after the retry = %q, want one", f.handedBack)
	}
}

// A stop of a sandboxed seat pushes its sandbox's commits, deletes the sandbox
// and its record, and then hands the bead back (gt-ft2vg).
func TestStop_SandboxedSeatIsPushedAndRemovedBeforeTheHandBack(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	f := newOverrunFake(20*time.Minute, stopSeat)
	var order []string
	f.removeSandbox = func(seat overrunSeat) error {
		return d.overrunRemoveSandbox(seat,
			func(_ string, rec session.SandboxRecord) error {
				order = append(order, "push "+rec.Branch)
				return nil
			},
			func(_ string, rec session.SandboxRecord) error {
				order = append(order, "delete "+rec.Session)
				return nil
			})
	}

	d.runOverrunGuard(f.seams())

	want := []string{"push " + stopSeat.Branch, "delete " + overrunStopSessionName}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("sandbox order = %q, want %q (push before delete)", order, want)
	}
	if _, ok, _ := session.ReadSandboxRecord(d.config.TownRoot, overrunStopSessionName); ok {
		t.Error("the sandbox record survived the stop")
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-back = %q, want the bead handed back once its sandbox is gone", f.handedBack)
	}
}

// overrunRemoveSandbox is the production sandbox step: push, then delete; a
// refused push keeps the sandbox, and a host seat (no record) does nothing.
func TestOverrunRemoveSandbox_PushesBeforeDeletingAndKeepsARefusedSandbox(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		sandboxed  bool
		pushErr    error
		wantCalls  []string
		wantRecord bool
		wantErr    string
	}{
		{name: "push_then_delete", sandboxed: true, wantCalls: []string{"push", "delete"}},
		{name: "gone_sandbox_has_nothing_to_push_and_is_still_removed", sandboxed: true, pushErr: session.ErrNothingToPush, wantCalls: []string{"push", "delete"}},
		{name: "refused_push_keeps_the_sandbox_and_names_it", sandboxed: true, pushErr: errors.New("rejected by origin"), wantCalls: []string{"push"}, wantRecord: true, wantErr: "sandbox " + overrunStopSessionName},
		{name: "host_seat_has_no_sandbox", wantCalls: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newOverrunDaemon(t, false)
			if tt.sandboxed {
				seedStopSandbox(t, d)
			} else {
				reg := session.NewPrefixRegistry()
				reg.Register(stopSeat.Rig, overrunStopPrefix)
				d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
			}
			var calls []string
			push := func(string, session.SandboxRecord) error {
				calls = append(calls, "push")
				return tt.pushErr
			}
			del := func(string, session.SandboxRecord) error {
				calls = append(calls, "delete")
				return nil
			}

			err := d.overrunRemoveSandbox(stopSeat, push, del)

			if tt.wantErr == "" && err != nil {
				t.Fatalf("overrunRemoveSandbox = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("overrunRemoveSandbox = %v, want an error naming %q", err, tt.wantErr)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Errorf("calls = %q, want %q", calls, tt.wantCalls)
			}
			if tt.sandboxed {
				_, ok, _ := session.ReadSandboxRecord(d.config.TownRoot, overrunStopSessionName)
				if ok != tt.wantRecord {
					t.Errorf("record kept = %v, want %v", ok, tt.wantRecord)
				}
			}
		})
	}
}

func TestNudge_MessageAsksForSplitProposal(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"split proposal", "bead notes"} {
		if !strings.Contains(overrunNudgeMessage, want) {
			t.Errorf("nudge message lacks %q: %s", want, overrunNudgeMessage)
		}
	}
	if strings.Contains(strings.ToLower(overrunNudgeMessage), "flaky") {
		t.Errorf("nudge message uses the word flaky: %s", overrunNudgeMessage)
	}
}

// bdFake is an in-memory bead behind the production BeadWriter: HandBack runs
// over it for real, so a test can read the bead the roster would read.
type bdFake struct {
	issue beads.Issue
	// failLabel makes the needs-split label write fail, the partial hand-back.
	failLabel bool
}

func (b *bdFake) Show(id string) (*beads.Issue, error) {
	is := b.issue
	return &is, nil
}

func (b *bdFake) Update(id string, opts beads.UpdateOptions) error {
	if len(opts.AddLabels) > 0 && b.failLabel {
		return errors.New("bd down")
	}
	if opts.Status != nil {
		b.issue.Status = *opts.Status
	}
	if opts.Assignee != nil {
		b.issue.Assignee = *opts.Assignee
	}
	b.issue.Labels = append(b.issue.Labels, opts.AddLabels...)
	return nil
}

func (b *bdFake) AddCommentAs(id, author, comment string) error { return nil }

func TestStop_HandBackLeavesTheBeadUnhooked(t *testing.T) {
	t.Parallel()
	bd := &bdFake{issue: beads.Issue{
		ID: "gt-aaa11", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/capable",
	}}
	w := polecat.NewBeadWriter(bd, overrunActor)

	if err := polecat.HandBack(context.Background(), w, "gt-aaa11", "gastown/polecats/capable", "polecat/capable/gt-aaa11+x1", 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	if bd.issue.Status != "open" || bd.issue.Assignee != "" {
		t.Errorf("bead is %q assigned to %q, want open and unassigned (nothing hooked)", bd.issue.Status, bd.issue.Assignee)
	}
	if !reflect.DeepEqual(bd.issue.Labels, []string{polecat.NeedsSplitLabel}) {
		t.Errorf("labels = %v, want [needs-split]", bd.issue.Labels)
	}
}

// The first hand-back opened and unassigned the bead but failed at the label;
// the dispatcher slung it on, or it was closed. The retry must not reopen it or
// clear the new polecat's assignee.
func TestStop_HandBackRetryLeavesAReSlungOrClosedBeadAlone(t *testing.T) {
	t.Parallel()
	const stopped = "gastown/polecats/capable"
	for _, tc := range []struct {
		name, status, assignee string
	}{
		{"re-slung to another polecat", string(beads.IssueStatusHooked), "gastown/polecats/nux"},
		{"re-slung and started", "in_progress", "gastown/polecats/nux"},
		{"closed", "closed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := &bdFake{
				issue:     beads.Issue{ID: "gt-aaa11", Status: string(beads.IssueStatusHooked), Assignee: stopped},
				failLabel: true,
			}
			w := polecat.NewBeadWriter(bd, overrunActor)
			err := polecat.HandBack(context.Background(), w, "gt-aaa11", stopped, "b", 20*time.Minute)
			if err == nil || errors.Is(err, polecat.ErrHandBackSkipped) {
				t.Fatalf("first hand-back err = %v, want the label failure", err)
			}
			if bd.issue.Status != "open" || bd.issue.Assignee != "" {
				t.Fatalf("partial hand-back left %q assigned to %q, want open and unassigned", bd.issue.Status, bd.issue.Assignee)
			}

			// Between the ticks: the dispatcher re-slings it, or it lands.
			bd.issue.Status, bd.issue.Assignee, bd.failLabel = tc.status, tc.assignee, false
			err = polecat.HandBack(context.Background(), w, "gt-aaa11", stopped, "b", 20*time.Minute)
			if !errors.Is(err, polecat.ErrHandBackSkipped) {
				t.Errorf("retry err = %v, want ErrHandBackSkipped", err)
			}
			if bd.issue.Status != tc.status || bd.issue.Assignee != tc.assignee || len(bd.issue.Labels) != 0 {
				t.Errorf("retry changed the bead to %q assigned to %q labels %v, want %q assigned to %q untouched",
					bd.issue.Status, bd.issue.Assignee, bd.issue.Labels, tc.status, tc.assignee)
			}
		})
	}
}

// With nobody else on the bead the retry finishes the half-released hand-back.
func TestStop_HandBackRetryFinishesAnUntouchedPartialHandBack(t *testing.T) {
	t.Parallel()
	const stopped = "gastown/polecats/capable"
	bd := &bdFake{
		issue:     beads.Issue{ID: "gt-aaa11", Status: string(beads.IssueStatusHooked), Assignee: stopped},
		failLabel: true,
	}
	w := polecat.NewBeadWriter(bd, overrunActor)
	if err := polecat.HandBack(context.Background(), w, "gt-aaa11", stopped, "b", 20*time.Minute); err == nil {
		t.Fatal("first hand-back succeeded, want the label failure")
	}
	bd.failLabel = false
	if err := polecat.HandBack(context.Background(), w, "gt-aaa11", stopped, "b", 20*time.Minute); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if bd.issue.Status != "open" || bd.issue.Assignee != "" || !reflect.DeepEqual(bd.issue.Labels, []string{polecat.NeedsSplitLabel}) {
		t.Errorf("bead = %q assigned to %q labels %v, want open, unassigned, [needs-split]", bd.issue.Status, bd.issue.Assignee, bd.issue.Labels)
	}
}

func TestCollectOverrunStopped_ItemNamesBeadAndElapsed(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	if err := writeOverrunState(d.config.TownRoot, overrunState{
		Bead: "gt-aaa11", Elapsed: 20*time.Minute + 3*time.Second, StoppedAt: overrunSlung,
	}); err != nil {
		t.Fatal(err)
	}
	d.openWorkBeads = func([]string, time.Duration) workBeadReader {
		return needsSplitReader{&beads.Issue{ID: "gt-aaa11", Status: "open", Labels: []string{polecat.NeedsSplitLabel}}}
	}
	d.knownRigsCache, d.knownRigsCacheValid = []string{"gastown"}, true
	s := &attentionSources{d: d, now: overrunSlung}

	items, err := s.collectOverrunStopped(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	it := items[0]
	if it.Kind != attention.KindOverrunStopped || it.Severity != attention.SeverityLow || it.Bead != "gt-aaa11" {
		t.Errorf("item = %+v, want a low overrun-stopped item on gt-aaa11", it)
	}
	if !strings.Contains(it.Summary, "gt-aaa11") || !strings.Contains(it.Summary, "20m3s") {
		t.Errorf("summary %q lacks the bead or the elapsed time", it.Summary)
	}
}

// needsSplitReader answers every list with its one issue.
type needsSplitReader struct{ issue *beads.Issue }

func (r needsSplitReader) Show(string) (*beads.Issue, error) { return r.issue, nil }
func (r needsSplitReader) List(beads.ListOptions) ([]*beads.Issue, error) {
	return []*beads.Issue{r.issue}, nil
}

// A seat past the limit whose gt done is running (the re-check says submitted)
// is not paused, stopped or handed back, and nothing is recorded, so the guard
// judges it again next tick.
func TestStop_SeatInGtDoneIsNotStopped(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(25*time.Minute, stopSeat)
	f.submitted = true

	d.runOverrunGuard(f.seams())

	if len(f.calls) != 0 {
		t.Errorf("a seat in gt done had stop steps run: %q", f.calls)
	}
	if !strings.Contains(buf.String(), "submitted before the stop") {
		t.Errorf("log lacks the leaving-it line:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
		t.Errorf("state %+v recorded a stop that did not happen", st)
	}
}

// A bead that gains gt:ready-to-land between the seat listing and the
// hand-back is not handed back: it would carry needs-split and ready-to-land
// at once. The pause is released and the stop is recorded finished.
func TestStop_BeadReadyToLandMidStopIsNotHandedBack(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute, stopSeat)
	f.readyAfterStop = true

	d.runOverrunGuard(f.seams())

	want := []string{
		"pause gastown/capable",
		"stop gastown/capable",
		"checkpoint gastown/capable",
		"clear-pause gastown/capable",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("call order = %q, want %q (no hand-back)", f.calls, want)
	}
	if len(f.handedBack) != 0 {
		t.Errorf("submitted bead handed back: %q", f.handedBack)
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() {
		t.Error("StoppedAt not saved; the next tick would repeat the stop")
	}
}

// With no gt done signal the seat is stopped and handed back as before.
func TestStop_SeatWithNoGtDoneSignalIsStillStopped(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute, stopSeat)

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 1 || len(f.handedBack) != 1 {
		t.Errorf("stops %v, hand-backs %v; want one of each", f.stops, f.handedBack)
	}
}

func TestHeartbeatShowsGtDone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	hb := func(state polecat.HeartbeatState, age time.Duration) *polecat.SessionHeartbeat {
		return &polecat.SessionHeartbeat{Timestamp: now.Add(-age), State: state}
	}
	tests := []struct {
		name string
		hb   *polecat.SessionHeartbeat
		want bool
	}{
		{"no heartbeat", nil, false},
		{"fresh exiting", hb(polecat.HeartbeatExiting, 30*time.Second), true},
		{"exiting just inside the stale window", hb(polecat.HeartbeatExiting, polecat.SessionHeartbeatStaleThreshold-time.Second), true},
		{"stale exiting is a dead gt done", hb(polecat.HeartbeatExiting, polecat.SessionHeartbeatStaleThreshold), false},
		{"fresh working", hb(polecat.HeartbeatWorking, time.Second), false},
		{"fresh idle", hb(polecat.HeartbeatIdle, time.Second), false},
		{"v1 heartbeat without state", hb("", time.Second), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := heartbeatShowsGtDone(tt.hb, now); got != tt.want {
				t.Errorf("heartbeatShowsGtDone = %v, want %v", got, tt.want)
			}
		})
	}
}

// gtDoneInFlight reads the heartbeat gt done writes through the polecat package.
func TestGtDoneInFlight_ReadsTheHeartbeatFile(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	const sess = "gt-capable"
	if d.gtDoneInFlight(sess) {
		t.Error("in flight with no heartbeat file")
	}
	polecat.TouchSessionHeartbeatWithState(d.config.TownRoot, sess, polecat.HeartbeatWorking, "work", "gt-aaa11")
	if d.gtDoneInFlight(sess) {
		t.Error("a working heartbeat read as gt done")
	}
	polecat.TouchSessionHeartbeatWithState(d.config.TownRoot, sess, polecat.HeartbeatExiting, "gt done", "gt-aaa11")
	if !d.gtDoneInFlight(sess) {
		t.Error("a fresh exiting heartbeat not read as gt done")
	}
}

// A checkpoint the worktree refuses (mid-rebase, a failed git add) fails step 3:
// the hand-back comment names the branch as holding the work, so no hand-back
// runs until a retry commits it (gt-0frud).
func TestStop_RefusedCheckpointKeepsTheStopPendingAndIsRetried(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("checkpoint refused; uncommitted work may remain in the worktree")}

	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 0 {
		t.Fatalf("handed back past a refused checkpoint: %q", f.handedBack)
	}
	if !strings.Contains(buf.String(), "checkpoint failed (1 of 5): checkpoint refused") {
		t.Errorf("log lacks the refusal:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.CheckpointedAt.IsZero() || !st.StoppedAt.IsZero() {
		t.Errorf("state %+v counts a refused checkpoint as done", st)
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs after the retried checkpoint = %q, want one", f.handedBack)
	}
}

// A checkpoint refused on every tick is bounded (gt-98rlt): after
// overrunCheckpointMaxAttempts the bead is handed back with a note, the pause
// stays down so the worktree is not reused, and the stop does not run again.
func TestStop_PersistentlyRefusedCheckpointIsGivenUpOnAndHandedBack(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("checkpoint refused; uncommitted work may remain in the worktree")}

	for i := 1; i < overrunCheckpointMaxAttempts; i++ {
		d.runOverrunGuard(f.seams())
		if len(f.handedBack) != 0 {
			t.Fatalf("tick %d: handed back before the cap: %q", i, f.handedBack)
		}
		f.seats = nil
		f.now = f.now.Add(time.Minute)
	}
	f.calls = nil
	d.runOverrunGuard(f.seams())

	want := []string{"recheck gastown/capable", "checkpoint gastown/capable", "push gastown/capable", "hand-back gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls at the cap = %q, want %q (no retire: the seat stays parked)", f.calls, want)
	}
	if len(f.handedBack) != 1 || len(f.handBackNotes) != 1 {
		t.Fatalf("hand-backs = %q notes = %q, want one each", f.handedBack, f.handBackNotes)
	}
	for _, w := range []string{"could not be checkpointed", "5 attempts", "gastown/capable stays paused", "gt agent resume"} {
		if !strings.Contains(f.handBackNotes[0], w) {
			t.Errorf("hand-back note %q lacks %q", f.handBackNotes[0], w)
		}
	}
	if !strings.Contains(buf.String(), "giving up on the checkpoint") {
		t.Errorf("log lacks the give-up line:\n%s", buf.String())
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if st.StoppedAt.IsZero() || !st.CheckpointedAt.IsZero() || !st.CheckpointGaveUp {
		t.Errorf("state %+v, want stopped, not checkpointed, gave up", st)
	}

	f.calls = nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.calls) != 0 || len(f.handedBack) != 1 {
		t.Errorf("a finished stop ran again: calls %q hand-backs %q", f.calls, f.handedBack)
	}
}

// A seat taken back before the cap is never handed back, however many
// checkpoints had failed: the give-up path sits behind the same re-check.
func TestStop_TakenBackSeatIsNotHandedBackAtTheCap(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("git busy")}
	for i := 1; i < overrunCheckpointMaxAttempts; i++ {
		d.runOverrunGuard(f.seams())
		f.seats = nil
		f.now = f.now.Add(time.Minute)
	}
	f.lapsed = "a session is running on the seat"
	f.calls = nil
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 0 || !reflect.DeepEqual(f.calls, []string{"recheck gastown/capable"}) {
		t.Errorf("calls %q hand-backs %q, want only the re-check", f.calls, f.handedBack)
	}
}

// Failures short of the cap that end in a good checkpoint hand the bead back
// with no note and release the seat as before.
func TestStop_CheckpointThatRecoversHandsBackWithoutANote(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("git busy")}
	d.runOverrunGuard(f.seams())
	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.handBackNotes) != 1 || f.handBackNotes[0] != "" {
		t.Errorf("notes = %q, want one empty note", f.handBackNotes)
	}
	if got := f.calls[len(f.calls)-1]; got != "retire gastown/capable" {
		t.Errorf("last call %q, want the seat retired", got)
	}
}

// A resumed stop whose seat was taken back is abandoned: the bead belongs to
// whoever holds it now (gt-0frud).
func TestStop_ResumedStopOnATakenBackSeatIsAbandoned(t *testing.T) {
	t.Parallel()
	for _, why := range []string{"the pause marker is gone", "a session is running on the seat"} {
		t.Run(why, func(t *testing.T) {
			d, buf := newOverrunDaemon(t, false)
			f := newOverrunFake(22*time.Minute, stopSeat)
			f.failOn = map[string]error{"checkpoint": errors.New("git busy")}
			d.runOverrunGuard(f.seams())

			f.seats, f.calls, f.failOn = nil, nil, nil
			f.lapsed = why
			f.now = f.now.Add(time.Minute)
			d.runOverrunGuard(f.seams())

			if want := []string{"recheck gastown/capable"}; !reflect.DeepEqual(f.calls, want) {
				t.Errorf("calls = %q, want only the re-check (no checkpoint, hand-back or pause change)", f.calls)
			}
			if len(f.handedBack) != 0 {
				t.Errorf("handed back a bead whose seat was taken back: %q", f.handedBack)
			}
			if !strings.Contains(buf.String(), "abandoning the stop") || !strings.Contains(buf.String(), why) {
				t.Errorf("log lacks the abandon line naming %q:\n%s", why, buf.String())
			}
			if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() {
				t.Error("StoppedAt not saved; the next tick would resume the abandoned stop")
			}

			f.calls = nil
			f.lapsed = ""
			f.now = f.now.Add(time.Minute)
			d.runOverrunGuard(f.seams())
			if len(f.calls) != 0 {
				t.Errorf("an abandoned stop ran again: %q", f.calls)
			}
		})
	}
}

// A re-check that cannot read the seat keeps the stop pending.
func TestStop_ResumedStopRetriesWhenTheSeatCannotBeRead(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("git busy")}
	d.runOverrunGuard(f.seams())

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.lapsedErr = errors.New("tmux down")
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if want := []string{"recheck gastown/capable"}; !reflect.DeepEqual(f.calls, want) || len(f.handedBack) != 0 {
		t.Fatalf("calls = %q handed back %q, want the re-check only", f.calls, f.handedBack)
	}

	f.calls, f.lapsedErr = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs once the seat could be read = %q, want one", f.handedBack)
	}
}

// A stop that is not resumed does not re-check: it has just stopped the session.
func TestStop_FirstRunDoesNotRecheckTheSeat(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.lapsed = "a session is running on the seat"
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 1 {
		t.Errorf("a first-run stop was abandoned: calls %q", f.calls)
	}
}

// checkpointSeat builds a polecat worktree under the daemon's town and returns
// the checkout and the gitfake world d reads it through.
func checkpointSeat(t *testing.T, d *Daemon, files map[string]string) (string, *gitfake.Fake) {
	t.Helper()
	f := useGitfake(t, d)
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	workDir := filepath.Join(d.config.TownRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig)
	f.InitBare(t, origin)
	f.Commit(t, origin, "main", "initial", files)
	f.Clone(t, origin, workDir)
	return workDir, f
}

// The production checkpoint seam fails a refused checkpoint and passes a
// created one and a clean worktree.
func TestOverrunCheckpointSeam_FailsOnlyARefusedCheckpoint(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	workDir, _ := checkpointSeat(t, d, map[string]string{"a.go": "package a\n"})
	seam := d.overrunStopSeams(overrunSeams{}).checkpoint

	if err := seam(stopSeat); err != nil {
		t.Errorf("clean worktree: %v", err)
	}
	writeWorkFiles(t, workDir, map[string]string{"a.go": "package a\n// work\n"})
	if err := seam(stopSeat); err != nil {
		t.Errorf("created checkpoint: %v", err)
	}
	writeWorkFiles(t, workDir, map[string]string{"a.go": "package a\n// more work\n"})
	if err := os.WriteFile(filepath.Join(workDir, ".git", "MERGE_HEAD"), []byte(strings.Repeat("0", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := seam(stopSeat); err == nil {
		t.Error("a worktree mid-merge must fail the checkpoint step")
	}
}

// The production re-check reads the pause marker and then the session.
func TestOverrunStopLapsed_ReadsThePauseMarkerAndTheSession(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	tm := newFakeTmux(newFixedClock())
	d.tmux = tm

	if why, err := d.overrunStopLapsed(stopSeat, true); err != nil || why != "the pause marker is gone" {
		t.Errorf("no marker: got %q, %v", why, err)
	}

	if err := agentpause.Pause(d.config.TownRoot, stopSeat.Rig, constants.RolePolecat, stopSeat.Polecat, overrunPauseReason, overrunActor, ""); err != nil {
		t.Fatal(err)
	}
	if why, err := d.overrunStopLapsed(stopSeat, true); err != nil || why != "" {
		t.Errorf("paused, no session: got %q, %v; want the stop to still own the seat", why, err)
	}

	name := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(stopSeat.Rig), stopSeat.Polecat)
	tm.addSession(name, "node", time.Now())
	if why, err := d.overrunStopLapsed(stopSeat, true); err != nil || why != "a session is running on the seat" {
		t.Errorf("paused with a live session: got %q, %v", why, err)
	}
}

// A seat whose HEAD is detached has no branch the stop can push, so its
// checkpoint commit exists only in the worktree. The recorded cleanup status
// must say so: clean would let gt polecat nuke remove the worktree and strand
// the commit.
func TestOverrunCleanupStatusDetachedHeadUnpushed(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	workDir, f := checkpointSeat(t, d, map[string]string{"a.go": "package a\n"})
	g := d.gitAt(workDir)

	if err := f.OpenBranchRepo(workDir).CheckoutDetachForce("HEAD"); err != nil {
		t.Fatal(err)
	}
	writeWorkFiles(t, workDir, map[string]string{"a.go": "package a\n// checkpoint\n"})
	f.CommitWorktree(t, workDir, "checkpoint")

	// The push is skipped for a detached HEAD (overrunPushBranch has no branch
	// to name), so nothing reaches origin before the status is read.
	if err := d.overrunPushBranch(overrunSeat{Rig: stopSeat.Rig, Polecat: stopSeat.Polecat}); err != nil {
		t.Fatal(err)
	}
	if got := overrunCleanupStatus(g); got != string(polecat.CleanupUnpushed) {
		t.Errorf("detached HEAD with a checkpoint commit = %q, want has_unpushed", got)
	}
}

// An operator can resume the seat and sling new work onto the name while the
// stop is still pushing or handing the bead back. Step 6 would then overwrite
// the new sling's agent bead, cleanup status and intent record, so the stop
// looks at the seat again first and, finding it taken back, leaves the release
// to its new holder.
func TestOverrunStopResumeDuringHandBackKeepsNewSling(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	seams := f.seams()
	handBack := seams.handBack
	seams.handBack = func(seat overrunSeat, elapsed time.Duration, note string) error {
		err := handBack(seat, elapsed, note)
		// The resume and the new sling land after the stop's last look at the
		// seat and before its release.
		f.lapsed = "the pause marker is gone"
		return err
	}

	d.runOverrunGuard(seams)

	for _, call := range f.calls {
		for _, release := range []string{"reset-agent-bead", "record-cleanup", "retire"} {
			if strings.HasPrefix(call, release) {
				t.Errorf("%s ran after the seat was taken back; calls = %q", release, f.calls)
			}
		}
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-back ran %d times, want the one that preceded the resume", len(f.handedBack))
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() {
		t.Error("StoppedAt not written: the next tick would repeat the stop over the new sling")
	}
	if !strings.Contains(buf.String(), "seat not released") {
		t.Errorf("log lacks the abandoned release:\n%s", buf.String())
	}
}

// reworkWorld stands for the seat's kept branch: head is the commit the
// worktree holds, origin the commit origin holds for the branch ("" for none),
// and checkpointCommits makes the stop's checkpoint commit the dirty worktree,
// which moves head off whatever origin holds.
type reworkWorld struct {
	head, origin      string
	checkpointCommits bool
}

// seams is f's seams with the branch model behind checkpoint and
// finishedOnOrigin and the production hand-back, over store, after the
// recording one, so a test reads the label and comment the bead ends up with.
// handBackFails makes the first n hand-backs fail before writing.
func (w *reworkWorld) seams(f *overrunFake, store *molStore, handBackFails int) overrunSeams {
	s := f.seams()
	checkpoint := s.checkpoint
	s.checkpoint = func(seat overrunSeat) error {
		if err := checkpoint(seat); err != nil {
			return err
		}
		if w.checkpointCommits {
			w.head = "c0ffee2"
		}
		return nil
	}
	s.finishedOnOrigin = func(overrunSeat) string {
		if w.head != "" && w.head == w.origin {
			return w.head
		}
		return ""
	}
	handBack := s.handBack
	s.handBack = func(seat overrunSeat, elapsed time.Duration, note string) error {
		if err := handBack(seat, elapsed, note); err != nil {
			return err
		}
		if handBackFails > 0 {
			handBackFails--
			return errors.New("dolt timeout")
		}
		return handBackEndingMolecule(context.Background(), store, func(string, ...any) {}, seat, elapsed, note)
	}
	return s
}

// handedBackBead is the work bead and its comments after the stop.
func handedBackBead(t *testing.T, store *molStore) (*beads.Issue, string) {
	t.Helper()
	work, err := store.Show(molWorkBead)
	if err != nil {
		t.Fatal(err)
	}
	return work, strings.Join(commentTexts(t, store, molWorkBead), "\n")
}

// A seat whose gt done ended after the branch reached origin leaves the finished
// work there: the worktree head is origin's head and the checkpoint had nothing
// to commit. The bead goes back as rework, not for a split.
func TestStop_FinishedBranchAlreadyOnOriginIsHandedBackAsRework(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	store := newMolStore(t)
	w := &reworkWorld{head: "abc1234", origin: "abc1234"}

	d.runOverrunGuard(w.seams(f, store, 0))

	work, comments := handedBackBead(t, store)
	if work.Status != "open" || work.Assignee != "" {
		t.Errorf("bead = status %q assignee %q, want open and unassigned", work.Status, work.Assignee)
	}
	if !beads.HasLabel(work, polecat.ReworkLabel) || beads.HasLabel(work, polecat.NeedsSplitLabel) {
		t.Errorf("bead labels %v, want %s and not %s", work.Labels, polecat.ReworkLabel, polecat.NeedsSplitLabel)
	}
	for _, part := range []string{"abc1234", stopSeat.Branch, "on origin", "reuse branch " + stopSeat.Branch, "run gt done"} {
		if !strings.Contains(comments, part) {
			t.Errorf("comment %q lacks %q", comments, part)
		}
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() || st.ReworkSHA != "abc1234" {
		t.Errorf("state %+v, want a finished stop that remembers the rework commit", st)
	}
}

// A seat slung a rework bead resumes the branch the landing worker rejected, so
// its worktree head is already origin's head. A seat that committed nothing
// since it was slung has produced no work: the stop is an overrun, and the bead
// goes back for a split rather than back as finished work, which the next
// polecat's stand-down would turn into another wasted session. A head the seat
// did commit after its sling still goes back as rework.
func TestOverrunStopIdleReworkSeatIsNotHandedBackAsFinished(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		committed  time.Time
		wantLabel  string
		wantRework string
	}{
		{
			name:      "idle on the resumed branch",
			committed: overrunSlung.Add(-time.Hour),
			wantLabel: polecat.NeedsSplitLabel,
		},
		{
			name:       "head produced after the sling",
			committed:  overrunSlung.Add(5 * time.Minute),
			wantLabel:  polecat.ReworkLabel,
			wantRework: "abc1234",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newOverrunDaemon(t, false)
			f := newOverrunFake(20*time.Minute, stopSeat)
			store := newMolStore(t)
			s := f.seams()

			// The production finishedOnOrigin seam reads the seat's worktree:
			// its branch head is the commit origin holds for the branch (the
			// rejected branch of a rework bead), committed at tc.committed.
			worktree := filepath.Join(d.config.TownRoot, "gastown", "polecats", "capable", "gastown")
			if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			g := &pushStubGit{
				branch: stopSeat.Branch, sha: "abc1234", commitTime: tc.committed,
				originRefs: []git.RemoteRef{{Hash: "abc1234", Name: "refs/heads/" + stopSeat.Branch}},
			}
			d.openGitFn = func(string) daemonGit { return g }
			s.finishedOnOrigin = d.overrunFinishedOnOrigin

			// The production hand-back over the store, so the label and the
			// comment are the ones the bead ends up with.
			handBack := s.handBack
			s.handBack = func(seat overrunSeat, elapsed time.Duration, note string) error {
				if err := handBack(seat, elapsed, note); err != nil {
					return err
				}
				return handBackEndingMolecule(context.Background(), store, func(string, ...any) {}, seat, elapsed, note)
			}

			d.runOverrunGuard(s)

			work, comments := handedBackBead(t, store)
			other := polecat.ReworkLabel
			if tc.wantLabel == polecat.ReworkLabel {
				other = polecat.NeedsSplitLabel
			}
			if !beads.HasLabel(work, tc.wantLabel) || beads.HasLabel(work, other) {
				t.Errorf("bead labels %v, want %s and not %s", work.Labels, tc.wantLabel, other)
			}
			if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.ReworkSHA != tc.wantRework {
				t.Errorf("state ReworkSHA = %q, want %q", st.ReworkSHA, tc.wantRework)
			}
			if tc.wantRework == "" && !strings.Contains(comments, "Split the bead") {
				t.Errorf("comment %q lacks the split instruction", comments)
			}
			if tc.wantRework != "" && !strings.Contains(comments, "on origin") {
				t.Errorf("comment %q does not name the finished work on origin", comments)
			}
		})
	}
}

// Every stop that cannot show the finished work on origin keeps needs-split.
func TestStop_UnfinishedOrUnpushedWorkKeepsNeedsSplit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		world reworkWorld
		fail  map[string]error
	}{
		{name: "uncommitted work, the checkpoint commits it", world: reworkWorld{head: "abc1234", origin: "abc1234", checkpointCommits: true}},
		{name: "origin holds another commit", world: reworkWorld{head: "abc1234", origin: "9f8e7d6"}},
		{name: "no branch on origin", world: reworkWorld{head: "abc1234"}},
		{
			name: "push refused as non-fast-forward", world: reworkWorld{head: "abc1234", origin: "9f8e7d6"},
			fail: map[string]error{"push": &overrunPushDivergedError{
				Branch: stopSeat.Branch, OriginSHA: "9f8e7d6", Err: errors.New("rejected (non-fast-forward)"),
			}},
		},
		{
			// The head is origin's, but the checkpoint was refused: the worktree
			// may hold work no commit has.
			name: "checkpoint given up", world: reworkWorld{head: "abc1234", origin: "abc1234"},
			fail: map[string]error{"checkpoint": errors.New("checkpoint refused; uncommitted work may remain in the worktree")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newOverrunDaemon(t, false)
			f := newOverrunFake(20*time.Minute, stopSeat)
			f.failOn = tc.fail
			store := newMolStore(t)
			w := tc.world

			// A refused checkpoint is retried each tick until the stop gives up on it.
			for i := 0; i < overrunCheckpointMaxAttempts && len(f.handedBack) == 0; i++ {
				d.runOverrunGuard(w.seams(f, store, 0))
				f.seats = nil
				f.now = f.now.Add(time.Minute)
			}

			if len(f.handedBack) != 1 {
				t.Fatalf("hand-backs = %q, want one", f.handedBack)
			}
			work, comments := handedBackBead(t, store)
			if !beads.HasLabel(work, polecat.NeedsSplitLabel) || beads.HasLabel(work, polecat.ReworkLabel) {
				t.Errorf("bead labels %v, want %s and not %s", work.Labels, polecat.NeedsSplitLabel, polecat.ReworkLabel)
			}
			if !strings.Contains(comments, "Split the bead") {
				t.Errorf("comment %q lacks the split instruction", comments)
			}
			if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.ReworkSHA != "" {
				t.Errorf("state %+v remembers a rework commit", st)
			}
		})
	}
}

// The decision is read before the push and saved with it. A hand-back that
// fails and is repeated on a later tick finds origin holding the seat's head
// either way (the stop's own push put it there), so the repeat must not read
// it again: a stop whose work was not on origin stays needs-split, and one whose
// work was stays rework.
func TestStop_ReworkDecisionSurvivesARepeatedHandBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		originBefore string
		wantLabel    string
	}{
		{name: "finished work on origin", originBefore: "abc1234", wantLabel: polecat.ReworkLabel},
		{name: "work only in the worktree", originBefore: "", wantLabel: polecat.NeedsSplitLabel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newOverrunDaemon(t, false)
			f := newOverrunFake(20*time.Minute, stopSeat)
			store := newMolStore(t)
			w := &reworkWorld{head: "abc1234", origin: tc.originBefore}

			d.runOverrunGuard(w.seams(f, store, 1))
			if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.StoppedAt.IsZero() {
				t.Fatal("the stop finished although its hand-back failed")
			}
			w.origin = w.head // the stop's push has landed
			f.seats, f.calls = nil, nil
			f.now = f.now.Add(time.Minute)
			d.runOverrunGuard(w.seams(f, store, 0))

			work, _ := handedBackBead(t, store)
			if !beads.HasLabel(work, tc.wantLabel) || len(work.Labels) != 1 {
				t.Errorf("bead labels %v after the repeat, want only %s", work.Labels, tc.wantLabel)
			}
		})
	}
}

// The rework decision is saved before the push. A save that fails after the
// push leaves the branch on origin but not the push stamp, so the next tick
// pushes again; it must keep the saved decision rather than read origin after
// the stop's own push and turn a needs-split hand-back into rework.
func TestStop_ReworkDecisionSavedBeforeThePushOutlivesAFailedPostPushSave(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	store := newMolStore(t)
	w := &reworkWorld{head: "abc1234"} // origin holds nothing: the push is its first copy
	s := w.seams(f, store, 0)

	path, err := overrunStatePath(d.config.TownRoot, "gt-aaa11")
	if err != nil {
		t.Fatal(err)
	}
	aside := path + ".aside"
	push := s.push
	failSave := true
	s.push = func(seat overrunSeat) error {
		if !failSave {
			return push(seat)
		}
		failSave = false
		// Let the push land, then make the save that follows it fail: a
		// directory where the state file goes. The state saved before the
		// push is kept aside and put back once the tick returns.
		err := push(seat)
		if rerr := os.Rename(path, aside); rerr != nil {
			t.Fatal(rerr)
		}
		if merr := os.Mkdir(path, 0o755); merr != nil {
			t.Fatal(merr)
		}
		return err
	}

	d.runOverrunGuard(s)
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.StoppedAt.IsZero() {
		t.Fatal("the stop finished although its save after the push failed")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(aside, path); err != nil {
		t.Fatal(err)
	}
	w.origin = w.head // the first tick's push reached origin
	f.seats, f.calls = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(s)

	work, _ := handedBackBead(t, store)
	if !beads.HasLabel(work, polecat.NeedsSplitLabel) || beads.HasLabel(work, polecat.ReworkLabel) {
		t.Errorf("bead labels %v, want only %s: the decision was read again after the stop's own push",
			work.Labels, polecat.NeedsSplitLabel)
	}
	if len(f.pushed) != 2 {
		t.Errorf("pushes = %q, want the retry to push again (PushedAt was not saved)", f.pushed)
	}
}

// Only a needs-split bead raises the stop's attention item; a rework hand-back
// is not waiting for an operator.
func TestCollectOverrunStopped_RaisesNoItemForReworkHandBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		world     reworkWorld
		wantItems int
	}{
		{name: "rework", world: reworkWorld{head: "abc1234", origin: "abc1234"}, wantItems: 0},
		{name: "needs-split", world: reworkWorld{head: "abc1234", origin: "9f8e7d6"}, wantItems: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newOverrunDaemon(t, false)
			f := newOverrunFake(20*time.Minute, stopSeat)
			store := newMolStore(t)
			w := tc.world
			d.runOverrunGuard(w.seams(f, store, 0))

			d.openWorkBeads = func([]string, time.Duration) workBeadReader { return store }
			d.knownRigsCache, d.knownRigsCacheValid = []string{"gastown"}, true
			s := &attentionSources{d: d, now: overrunSlung}
			items, err := s.collectOverrunStopped(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != tc.wantItems {
				t.Errorf("items = %+v, want %d", items, tc.wantItems)
			}
		})
	}
}

// The production seam answers the commit only when the worktree's branch head
// is the one origin holds for the branch.
func TestOverrunFinishedOnOrigin_NeedsTheSameCommitOnBoth(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	worktree := filepath.Join(d.config.TownRoot, "gastown", "polecats", "capable", "gastown")
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ref := "refs/heads/" + stopSeat.Branch
	// The head is dated after the seat's sling: the seat produced it.
	slung := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	seated := stopSeat
	seated.SlingAt = slung
	g := &pushStubGit{branch: stopSeat.Branch, sha: "abc123", commitTime: slung.Add(time.Minute),
		originRefs: []git.RemoteRef{
			{Hash: "1111111", Name: ref + "-other"},
			{Hash: "abc123", Name: ref},
		}}
	d.openGitFn = func(string) daemonGit { return g }

	if got := d.overrunFinishedOnOrigin(seated); got != "abc123" {
		t.Errorf("same commit on both: got %q, want abc123", got)
	}
	if len(g.pushes) != 0 {
		t.Errorf("the look pushed: %q", g.pushes)
	}

	g.originRefs = []git.RemoteRef{{Hash: "9f8e7d6", Name: ref}}
	if got := d.overrunFinishedOnOrigin(seated); got != "" {
		t.Errorf("origin holds another commit: got %q, want none", got)
	}
	g.originRefs = []git.RemoteRef{{Hash: "abc123", Name: ref + "-other"}}
	if got := d.overrunFinishedOnOrigin(seated); got != "" {
		t.Errorf("no branch on origin: got %q, want none", got)
	}
	g.originRefs, g.originErr = []git.RemoteRef{{Hash: "abc123", Name: ref}}, errors.New("unreachable")
	if got := d.overrunFinishedOnOrigin(seated); got != "" {
		t.Errorf("origin cannot be asked: got %q, want none", got)
	}
	other := seated
	other.Polecat = "gone"
	if got := d.overrunFinishedOnOrigin(other); got != "" {
		t.Errorf("seat with no worktree: got %q, want none", got)
	}

	// The head must be one the seat committed after its sling: the same commit
	// on both ends is not finished work when the seat only resumed it.
	g.originRefs, g.originErr = []git.RemoteRef{{Hash: "abc123", Name: ref}}, nil
	g.commitTime = slung.Add(-time.Hour)
	if got := d.overrunFinishedOnOrigin(seated); got != "" {
		t.Errorf("head committed before the sling: got %q, want none", got)
	}
	g.commitTime = slung.Add(time.Minute)
	if got := d.overrunFinishedOnOrigin(seated); got != "abc123" {
		t.Errorf("head committed after the sling: got %q, want abc123", got)
	}
	g.commitTime = time.Time{}
	if got := d.overrunFinishedOnOrigin(seated); got != "" {
		t.Errorf("commit time unknown: got %q, want none", got)
	}

	// An unknown sling time cannot show the head was produced by this run, so
	// it answers needs-split (empty) rather than reviving the old behaviour.
	g.commitTime = slung.Add(time.Minute)
	if got := d.overrunFinishedOnOrigin(stopSeat); got != "" {
		t.Errorf("sling time unknown: got %q, want none", got)
	}
}

// A stop retried on later ticks records the run's end once, stamped with the
// session stop's time: a hand-back that keeps failing runs step 5 every tick,
// and a later end written each time would creep past the dispatch it ended,
// leaving the bead dated from the agent bead's write, which reads short
// (gt-tzbtf).
func TestStop_RecordsTheRunEndOnceAcrossHandBackRetries(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"hand-back": errors.New("bd is down")}

	for range 3 {
		d.runOverrunGuard(f.seams())
	}
	if len(f.handedBack) != 0 {
		t.Fatalf("handed back %v although the hand-back failed", f.handedBack)
	}
	if len(f.endRuns) != 1 {
		t.Fatalf("run ends = %d across three retried ticks, want 1", len(f.endRuns))
	}
	if want := f.now; !f.endRunAts[0].Equal(want) {
		t.Errorf("run end stamped %v, want the session stop's time, %v", f.endRunAts[0], want)
	}

	f.failOn = nil
	d.runOverrunGuard(f.seams())
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %d, want the successful tick to hand the bead back", len(f.handedBack))
	}
	if len(f.endRuns) != 1 {
		t.Errorf("run ends = %d after the hand-back succeeded, want still 1", len(f.endRuns))
	}
}

// A bead that is no longer the stopped seat's — closed, or re-slung to another
// polecat — is not given a run end: the record would end that holder's run and
// date its clock by the old one (gt-tzbtf).
func TestStop_DoesNotEndTheRunOfABeadAnotherHolderHas(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.notOurs = true

	d.runOverrunGuard(f.seams())

	if len(f.endRuns) != 0 {
		t.Errorf("run ends = %v, want none for a bead another polecat holds", f.endRuns)
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %d, want the stop to release the seat", len(f.handedBack))
	}
	if !strings.Contains(buf.String(), "no longer this seat's") {
		t.Errorf("log lacks the skipped record:\n%s", buf.String())
	}
}

// A bead that cannot be read before the run's end fails the step: nothing is
// written on a guess, and the next tick reads it again.
func TestStop_UnreadableBeadBeforeTheRunEndHoldsTheHandBack(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.stillOursErr = errors.New("bd timed out")

	d.runOverrunGuard(f.seams())

	if len(f.endRuns) != 0 || len(f.handedBack) != 0 {
		t.Errorf("run ends %v, hand-backs %v; want nothing written on an unreadable bead", f.endRuns, f.handedBack)
	}
	st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead)
	if err != nil {
		t.Fatal(err)
	}
	if !st.StoppedAt.IsZero() {
		t.Error("stop marked finished although the bead could not be read")
	}
	if !strings.Contains(buf.String(), "cannot read the bead before ending its run") {
		t.Errorf("log lacks the unreadable-bead step failure:\n%s", buf.String())
	}
}

// The production re-check reports a bead it cannot read, so the stop writes no
// run end on a guess. A bd that cannot run is that read's failure.
func TestStillOursSeamReportsAnUnreadableBead(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	d.bdPath = filepath.Join(t.TempDir(), "no-such-bd")
	seams := d.overrunStopSeams(overrunSeams{})
	if seams.stillOurs == nil {
		t.Fatal("overrunStopSeams left stillOurs nil: nothing checks whose bead it is")
	}
	if _, err := seams.stillOurs(stopSeat); err == nil {
		t.Fatal("stillOurs read a bead with no bd to run as one it could read")
	}
}

// overrunSeatSessionSeams is the fake's seams with the three steps that touch
// the seat itself (pause, clear-pause, stop) in their production form: the
// park the stop writes and the supervisor's kill, over the daemon's fake tmux.
// The rest stay the fake's, so the stop runs its whole sequence without git,
// bd or a real town.
func overrunSeatSessionSeams(d *Daemon, f *overrunFake) overrunSeams {
	s := f.seams()
	prod := d.overrunStopSeams(overrunSeams{})
	s.pause, s.clearPause, s.stopSession = prod.pause, prod.clearPause, prod.stopSession
	return s
}

// overrunStopPrefix is the rig prefix the stop's supervisor tests pin for the
// seat's rig, so the session they seed is the one the kill must reach. The
// process-wide registry the daemon would otherwise read is shared with every
// parallel test.
const overrunStopPrefix = "gt"

// overrunStopSessionName is the tmux session of stopSeat under that prefix.
var overrunStopSessionName = overrunStopPrefix + "-" + stopSeat.Polecat

// seedOverrunStopSession pins the daemon's rig prefix and seeds the seat's
// session, returning the tmux the stop's kill must reach.
func seedOverrunStopSession(t *testing.T, d *Daemon) *fakeTmux {
	t.Helper()
	reg := session.NewPrefixRegistry()
	reg.Register(stopSeat.Rig, overrunStopPrefix)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	tm := newFakeTmux(newFixedClock())
	tm.addSession(overrunStopSessionName, "", overrunSlung)
	d.tmux = tm
	return tm
}

// overrunSupervisorActions reads the supervisor's action log.
func overrunSupervisorActions(t *testing.T, townRoot string) []supervisor.ActionLine {
	t.Helper()
	data, err := os.ReadFile(supervisor.ActionLogPath(townRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []supervisor.ActionLine
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var l supervisor.ActionLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("action log line %q: %v", line, err)
		}
		out = append(out, l)
	}
	return out
}

// The stop goes through the supervisor, the town's one kill path: the kill is
// recorded with the overrun guard's actor and the rule as its reason, and the
// park the stop wrote itself does not refuse it.
func TestOverrunGuardStop_GoesThroughTheSupervisor(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	tm := seedOverrunStopSession(t, d)
	f := newOverrunFake(21*time.Minute, stopSeat)

	d.runOverrunGuard(overrunSeatSessionSeams(d, f))

	if alive, _ := tm.HasSession(overrunStopSessionName); alive {
		t.Fatalf("session %s still running after the stop: %s", overrunStopSessionName, buf.String())
	}
	wantReason := "overrun (20 minute rule) on " + stopSeat.Bead
	lines := overrunSupervisorActions(t, d.config.TownRoot)
	if len(lines) != 1 {
		t.Fatalf("supervisor action log = %+v, want one kill line", lines)
	}
	got := lines[0]
	if got.Verb != "kill" || got.Actor != overrunActor || got.Outcome != "done" ||
		got.Reason != wantReason || got.Session != overrunStopSessionName {
		t.Errorf("action line = %+v, want a done kill of %s by %s for %q", got, overrunStopSessionName, overrunActor, wantReason)
	}
	// The stop ran to its end: the bead was handed back with its branch.
	if len(f.handedBack) != 1 || !strings.HasPrefix(f.handedBack[0], stopSeat.Bead+" "+stopSeat.Branch) {
		t.Errorf("hand-backs = %q, want the bead and its branch", f.handedBack)
	}
}

// An active e-stop holds the stop before it parks the seat (gt-m5b31). On every
// tick the e-stop is up the seat keeps running, is never parked, and no kill is
// attempted, so nothing of the stop runs and nothing is saved for it.
func TestOverrunGuardStop_EstopHoldsTheStopBeforeThePark(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"town", "rig"} {
		d, buf := newOverrunDaemon(t, false)
		tm := seedOverrunStopSession(t, d)
		if which == "town" {
			if err := estop.Activate(d.config.TownRoot, estop.TriggerManual, "operator halt"); err != nil {
				t.Fatal(err)
			}
		} else if err := estop.ActivateRig(d.config.TownRoot, stopSeat.Rig, estop.TriggerManual, "operator halt"); err != nil {
			t.Fatal(err)
		}
		f := newOverrunFake(21*time.Minute, stopSeat)
		s := overrunSeatSessionSeams(d, f)
		parks := 0
		pause := s.pause
		s.pause = func(seat overrunSeat) error {
			parks++
			return pause(seat)
		}

		for tick := 0; tick < 3; tick++ {
			d.runOverrunGuard(s)
			f.now = f.now.Add(time.Minute)
		}

		if alive, _ := tm.HasSession(overrunStopSessionName); !alive {
			t.Fatalf("%s e-stop: session %s was stopped under the e-stop", which, overrunStopSessionName)
		}
		if parks != 0 {
			t.Errorf("%s e-stop: the seat was parked %d time(s) over three ticks, want none", which, parks)
		}
		if !strings.Contains(buf.String(), "waits: e-stop active") {
			t.Errorf("%s e-stop: log lacks the wait line:\n%s", which, buf.String())
		}
		if lines := overrunSupervisorActions(t, d.config.TownRoot); len(lines) != 0 {
			t.Errorf("%s e-stop: supervisor action log = %+v, want no kill attempted", which, lines)
		}
		if paused, _, _ := agentpause.IsPaused(d.config.TownRoot, stopSeat.Rig, constants.RolePolecat, stopSeat.Polecat); paused {
			t.Errorf("%s e-stop: the seat is parked", which)
		}
		st, _ := readOverrunState(d.config.TownRoot, stopSeat.Bead)
		if !st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
			t.Errorf("%s e-stop: state %+v records a stop that never ran", which, st)
		}
		if len(f.handedBack) != 0 || len(f.pushed) != 0 {
			t.Errorf("%s e-stop: hand-backs %q, pushes %q; want nothing under the e-stop", which, f.handedBack, f.pushed)
		}
	}
}

// A town sentinel that cannot be read holds the stop too: the e-stop check fails
// closed, so a stat that fails for any reason but absence waits, parks nothing,
// and says why (gt-m5b31). A symlink that points at itself makes the stat fail
// with a loop error, not absence.
func TestOverrunGuardStop_UnreadableEstopHoldsTheStop(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	tm := seedOverrunStopSession(t, d)
	sentinel := estop.FilePath(d.config.TownRoot)
	if err := os.Symlink(filepath.Base(sentinel), sentinel); err != nil {
		t.Fatal(err)
	}
	f := newOverrunFake(21*time.Minute, stopSeat)
	s := overrunSeatSessionSeams(d, f)
	parks := 0
	pause := s.pause
	s.pause = func(seat overrunSeat) error {
		parks++
		return pause(seat)
	}

	d.runOverrunGuard(s)

	if alive, _ := tm.HasSession(overrunStopSessionName); !alive {
		t.Fatalf("session %s was stopped under an unreadable e-stop", overrunStopSessionName)
	}
	if parks != 0 {
		t.Errorf("the seat was parked %d time(s) under an unreadable e-stop, want none", parks)
	}
	if !strings.Contains(buf.String(), "waits: cannot read the e-stop") {
		t.Errorf("log lacks the unreadable-e-stop wait line:\n%s", buf.String())
	}
	if lines := overrunSupervisorActions(t, d.config.TownRoot); len(lines) != 0 {
		t.Errorf("supervisor action log = %+v, want no kill attempted", lines)
	}
	if st, _ := readOverrunState(d.config.TownRoot, stopSeat.Bead); !st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
		t.Errorf("state %+v records a stop that never ran", st)
	}
}

// An e-stop that lands after the seat is parked, between the check and the
// kill, is still refused by the supervisor: the session keeps running, the
// refusal is logged with the supervisor's text, the park is cleared so the live
// seat is not left parked (gt-fojqs), and nothing of the stop after it runs.
func TestOverrunGuardStop_EstopRaisedAfterThePark_IsRefused(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"town", "rig"} {
		d, buf := newOverrunDaemon(t, false)
		tm := seedOverrunStopSession(t, d)
		raise := func() error {
			if which == "town" {
				return estop.Activate(d.config.TownRoot, estop.TriggerManual, "operator halt")
			}
			return estop.ActivateRig(d.config.TownRoot, stopSeat.Rig, estop.TriggerManual, "operator halt")
		}
		f := newOverrunFake(21*time.Minute, stopSeat)
		s := overrunSeatSessionSeams(d, f)
		pause := s.pause
		s.pause = func(seat overrunSeat) error {
			if err := pause(seat); err != nil {
				return err
			}
			if err := raise(); err != nil {
				t.Fatal(err)
			}
			return nil
		}

		d.runOverrunGuard(s)

		if alive, _ := tm.HasSession(overrunStopSessionName); !alive {
			t.Fatalf("%s e-stop: session %s was stopped under the e-stop", which, overrunStopSessionName)
		}
		if !strings.Contains(buf.String(), stopSeat.Bead+" refused: supervisor refused: e-stop is active") {
			t.Errorf("%s e-stop: log lacks the refusal line with the supervisor's text:\n%s", which, buf.String())
		}
		lines := overrunSupervisorActions(t, d.config.TownRoot)
		if len(lines) != 1 || lines[0].Outcome != "refused" || lines[0].Verb != "kill" || lines[0].Actor != overrunActor {
			t.Errorf("%s e-stop: supervisor action log = %+v, want one refused kill by %s", which, lines, overrunActor)
		}
		if paused, _, _ := agentpause.IsPaused(d.config.TownRoot, stopSeat.Rig, constants.RolePolecat, stopSeat.Polecat); paused {
			t.Errorf("%s e-stop: the refused stop left the seat parked", which)
		}
		st, _ := readOverrunState(d.config.TownRoot, stopSeat.Bead)
		if !st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
			t.Errorf("%s e-stop: state %+v records a stop that never ran", which, st)
		}
		if len(f.handedBack) != 0 || len(f.pushed) != 0 {
			t.Errorf("%s e-stop: hand-backs %q, pushes %q; want nothing after a refused stop", which, f.handedBack, f.pushed)
		}
	}
}
