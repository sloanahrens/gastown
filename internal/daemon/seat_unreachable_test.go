package daemon

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// seatAlertLines renders alerts as comparable strings, the last error included,
// so a table wants the whole alert and not only its count.
func seatAlertLines(alerts []seatUnreachable) []string {
	lines := make([]string, len(alerts))
	for i, a := range alerts {
		lines[i] = fmt.Sprintf("%s/%s %s since %s: %v", a.Rig, a.Polecat, a.Bead, a.Since.Format(time.RFC3339), a.Err)
	}
	return lines
}

// TestTrackSeatUnreachableRaisesAtFiveMinutes drives one unreadable seat tick by
// tick: under five minutes it raises nothing, and at exactly five it names the
// seat with the bead it holds, the first-failure time and the last error.
func TestTrackSeatUnreachableRaisesAtFiveMinutes(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)
	gone := errors.New("sandbox pod gastown-dag is gone")
	dag := seatRead{Rig: "gastown", Polecat: "dag", Bead: "gt-p4aqut", Err: gone}
	atFive := fmt.Sprintf("gastown/dag gt-p4aqut since %s: %v", base.Format(time.RFC3339), gone)

	steps := []struct {
		name  string
		at    time.Duration
		reads []seatRead
		want  []string
	}{
		{"first failed read starts the clock", 0, []seatRead{dag}, nil},
		{"four minutes raises nothing", 4 * time.Minute, []seatRead{dag}, nil},
		{"one second short raises nothing", 5*time.Minute - time.Second, []seatRead{dag}, nil},
		{"five minutes names the seat and its bead", 5 * time.Minute, []seatRead{dag}, []string{atFive}},
		{"a later tick names it from the same first failure", 6 * time.Minute, []seatRead{dag}, []string{atFive}},
	}
	var prev map[seatKey]seatOutage
	var alerts []seatUnreachable
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			prev, alerts = trackSeatUnreachable(prev, base.Add(step.at), step.reads, true)
			if got := seatAlertLines(alerts); !slices.Equal(got, step.want) {
				t.Errorf("alerts = %q, want %q", got, step.want)
			}
		})
	}
}

// TestTrackSeatUnreachableStartsOverAfterAGoodRead: one good read clears the
// first-failure time, the seat stops being named, and the next failure waits out
// the whole five minutes from its own first failed read.
func TestTrackSeatUnreachableStartsOverAfterAGoodRead(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)
	gone := errors.New("session read timed out")
	dag := seatRead{Rig: "gastown", Polecat: "dag", Bead: "gt-p4aqut", Err: gone}
	answered := dag
	answered.Err = nil
	atFive := fmt.Sprintf("gastown/dag gt-p4aqut since %s: %v", base.Format(time.RFC3339), gone)
	restarted := base.Add(7 * time.Minute)

	steps := []struct {
		name string
		at   time.Duration
		read seatRead
		want []string
	}{
		{"first failed read starts the clock", 0, dag, nil},
		{"five minutes names the seat", 5 * time.Minute, dag, []string{atFive}},
		{"one good read clears it and names nothing", 6 * time.Minute, answered, nil},
		{"a failure after the good read starts over", 7 * time.Minute, dag, nil},
		{"four minutes after it raises nothing", 11 * time.Minute, dag, nil},
		{"five minutes after it names the seat again", 12 * time.Minute, dag, []string{fmt.Sprintf("gastown/dag gt-p4aqut since %s: %v", restarted.Format(time.RFC3339), gone)}},
	}
	var prev map[seatKey]seatOutage
	var alerts []seatUnreachable
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			prev, alerts = trackSeatUnreachable(prev, base.Add(step.at), []seatRead{step.read}, true)
			if got := seatAlertLines(alerts); !slices.Equal(got, step.want) {
				t.Errorf("alerts = %q, want %q", got, step.want)
			}
		})
	}
}

// TestTrackSeatUnreachableClearsOnlyTheSeatThatAnswered: two unreadable seats
// are named together, and one seat's good read clears its own item only.
func TestTrackSeatUnreachableClearsOnlyTheSeatThatAnswered(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)
	gone := errors.New("sandbox pod is gone")
	dag := seatRead{Rig: "gastown", Polecat: "dag", Bead: "gt-p4aqut", Err: gone}
	slit := seatRead{Rig: "gastown", Polecat: "slit", Bead: "gt-q6hafz", Err: gone}
	reads := []seatRead{dag, slit}

	prev, alerts := trackSeatUnreachable(nil, base, reads, true)
	if len(alerts) != 0 {
		t.Fatalf("alerts on the first failed read = %q, want none", seatAlertLines(alerts))
	}
	prev, alerts = trackSeatUnreachable(prev, base.Add(5*time.Minute), reads, true)
	want := []string{
		fmt.Sprintf("gastown/dag gt-p4aqut since %s: %v", base.Format(time.RFC3339), gone),
		fmt.Sprintf("gastown/slit gt-q6hafz since %s: %v", base.Format(time.RFC3339), gone),
	}
	if got := seatAlertLines(alerts); !slices.Equal(got, want) {
		t.Fatalf("alerts = %q, want %q", got, want)
	}

	reads[0].Err = nil
	prev, alerts = trackSeatUnreachable(prev, base.Add(6*time.Minute), reads, true)
	want = []string{fmt.Sprintf("gastown/slit gt-q6hafz since %s: %v", base.Format(time.RFC3339), gone)}
	if got := seatAlertLines(alerts); !slices.Equal(got, want) {
		t.Errorf("alerts after dag answered = %q, want only slit", got)
	}
}

// TestTrackSeatUnreachableDropsASeatItNoLongerReads: the seat list is passed in
// full every tick, so a seat that left it, as a released seat does, is dropped
// instead of being named from a first-failure time that would outlive it. If
// the seat comes back failing it waits out the five minutes again.
func TestTrackSeatUnreachableDropsASeatItNoLongerReads(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)
	dag := seatRead{Rig: "gastown", Polecat: "dag", Bead: "gt-p4aqut", Err: errors.New("gone")}
	prev, _ := trackSeatUnreachable(nil, base, []seatRead{dag}, true)

	prev, alerts := trackSeatUnreachable(prev, base.Add(5*time.Minute), nil, true)
	if len(alerts) != 0 {
		t.Errorf("alerts for a seat the reads no longer list = %q, want none", seatAlertLines(alerts))
	}
	_, alerts = trackSeatUnreachable(prev, base.Add(6*time.Minute), []seatRead{dag}, true)
	if len(alerts) != 0 {
		t.Errorf("alerts for a seat that failed again one minute after it was dropped = %q, want none", seatAlertLines(alerts))
	}
}

// TestTrackSeatUnreachableKeepsRunningAlarmsWhenTheListCouldNotBeRead: a tick
// whose seat list was not read, failed or empty, says nothing about any seat. A
// seat already unreadable keeps its first-failure time, so it is named on the
// next tick whose list is read, at the time it would have been.
func TestTrackSeatUnreachableKeepsRunningAlarmsWhenTheListCouldNotBeRead(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)
	gone := errors.New("sandbox pod is gone")
	dag := seatRead{Rig: "gastown", Polecat: "dag", Bead: "gt-p4aqut", Err: gone}
	prev, _ := trackSeatUnreachable(nil, base, []seatRead{dag}, true)

	prev, alerts := trackSeatUnreachable(prev, base.Add(5*time.Minute), nil, false)
	if len(alerts) != 0 {
		t.Errorf("alerts from a tick whose list was not read = %q, want none", seatAlertLines(alerts))
	}
	_, alerts = trackSeatUnreachable(prev, base.Add(6*time.Minute), []seatRead{dag}, true)
	want := []string{fmt.Sprintf("gastown/dag gt-p4aqut since %s: %v", base.Format(time.RFC3339), gone)}
	if got := seatAlertLines(alerts); !slices.Equal(got, want) {
		t.Errorf("alerts after the list read again = %q, want %q: the unread tick must not have cleared the seat", got, want)
	}
}

// TestTrackSeatUnreachableRestartsAFirstFailureTimeFromTheFuture: a clock that
// stepped back between ticks leaves a first-failure time later than now, and
// the read restarts it rather than suppressing the item until the wall clock
// catches up.
func TestTrackSeatUnreachableRestartsAFirstFailureTimeFromTheFuture(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)
	gone := errors.New("gone")
	dag := seatRead{Rig: "gastown", Polecat: "dag", Bead: "gt-p4aqut", Err: gone}
	ahead, _ := trackSeatUnreachable(nil, base.Add(10*time.Minute), []seatRead{dag}, true)

	prev, alerts := trackSeatUnreachable(ahead, base, []seatRead{dag}, true)
	if len(alerts) != 0 {
		t.Errorf("alerts from a restarted clock = %q, want none", seatAlertLines(alerts))
	}
	_, alerts = trackSeatUnreachable(prev, base.Add(5*time.Minute), []seatRead{dag}, true)
	want := []string{fmt.Sprintf("gastown/dag gt-p4aqut since %s: %v", base.Format(time.RFC3339), gone)}
	if got := seatAlertLines(alerts); !slices.Equal(got, want) {
		t.Errorf("alerts five minutes after the restart = %q, want %q", got, want)
	}
}
