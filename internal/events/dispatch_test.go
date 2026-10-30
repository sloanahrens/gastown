package events

import (
	"testing"
	"time"
)

// dispatchEv builds one log line for the dispatch clock's tests: a sling when
// the type is a sling, a done event otherwise.
func dispatchEv(typ, at, bead string) Event {
	e := Event{Timestamp: at, Type: typ, Payload: map[string]interface{}{"bead": bead}}
	if typ == TypeSling {
		e.Payload["target"] = "gastown/polecats/" + bead
	}
	return e
}

// dispatchSlingEv builds a sling line written by actor to the seat it names,
// so a test can tell the spec dispatcher's slings from anyone else's.
func dispatchSlingEv(at, bead, actor, seat string) Event {
	return Event{
		Timestamp: at,
		Type:      TypeSling,
		Actor:     actor,
		Payload:   SlingPayload(bead, seat),
	}
}

// specSlingRun is the gt-3ga5l sequence: three spec-dispatch slings of one
// bead to one seat whose run never wrote an end, then the sling that put the
// bead on a second seat once the first had let it go back to the pool.
var specSlingRun = []Event{
	dispatchSlingEv("2026-10-30T12:10:59Z", "gt-a", SpecDispatchActor, "gastown/nux"),
	dispatchSlingEv("2026-10-30T12:17:39Z", "gt-a", SpecDispatchActor, "gastown/nux"),
	dispatchSlingEv("2026-10-30T12:23:46Z", "gt-a", SpecDispatchActor, "gastown/nux"),
	dispatchSlingEv("2026-10-30T14:27:05Z", "gt-a", SpecDispatchActor, "gastown/capable"),
}

const (
	t9  = "2026-10-27T09:00:00Z"
	t10 = "2026-10-27T10:00:00Z"
	t11 = "2026-10-27T11:00:00Z"
	t12 = "2026-10-27T12:00:00Z"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// The clock belongs to the bead, not the seat: a bead slung again — to the
// same seat or another one — keeps the dispatch it first went out on, and a
// done event ends the run so the dispatch after it starts over.
func TestDispatchStartCountsTheBeadsFirstDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		evs  []Event
		want string // "" for not found
	}{
		{
			name: "one dispatch",
			evs:  []Event{dispatchEv(TypeSling, t9, "gt-a")},
			want: t9,
		},
		{
			name: "re-slung to a second seat keeps the first dispatch",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				dispatchEv(TypeSling, t11, "gt-a"),
			},
			want: t9,
		},
		{
			name: "a done event ends the run, and the dispatch after it starts over",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				dispatchEv(TypeDone, t10, "gt-a"),
				dispatchEv(TypeSling, t11, "gt-a"),
			},
			want: t11,
		},
		{
			name: "a dispatch older than the last done event belongs to an earlier run",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				dispatchEv(TypeDone, t10, "gt-a"),
				dispatchEv(TypeDone, t12, "gt-a"),
			},
			want: "",
		},
		{
			// The overrun guard's stop ends the run as gt done does: the seat's
			// clock is done, and the dispatch after the stop is policed afresh
			// rather than read as an old run that keeps its first sling.
			name: "an overrun stop ends the run too",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				dispatchEv(TypeOverrunStop, t10, "gt-a"),
				dispatchEv(TypeSling, t11, "gt-a"),
			},
			want: t11,
		},
		{
			// The boundary is inclusive: a sling written in the same second as
			// the run's end is the new run's dispatch, not the old run's.
			name: "a dispatch at the run end's own timestamp starts the new run",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				dispatchEv(TypeDone, t10, "gt-a"),
				dispatchEv(TypeSling, t10, "gt-a"),
			},
			want: t10,
		},
		{
			// A bead closed by hand ends its run as a done event does, so a
			// bead reopened and dispatched later is judged from the new sling.
			name: "a hand-closed bead ends its run",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				dispatchEv(TypeBeadClosed, t10, "gt-a"),
				dispatchEv(TypeSling, t11, "gt-a"),
			},
			want: t11,
		},
		{
			// The run end a record carries dates it, not when the record was
			// written: a stop whose hand-back is retried keeps writing its end,
			// and a later write must not creep past a dispatch that started a
			// new run in between.
			name: "a run end's recorded time dates it, not the record's write time",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				{Timestamp: t12, Type: TypeOverrunStop, Payload: map[string]interface{}{"bead": "gt-a", "at": t10}},
				dispatchEv(TypeSling, t11, "gt-a"),
			},
			want: t11,
		},
		{
			name: "another bead's events do not date this one",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-b"),
				dispatchEv(TypeDone, t10, "gt-b"),
				dispatchEv(TypeSling, t11, "gt-a"),
			},
			want: t11,
		},
		{
			name: "an unparseable dispatch is skipped",
			evs: []Event{
				dispatchEv(TypeSling, "not a time", "gt-a"),
				dispatchEv(TypeSling, t9, "gt-a"),
			},
			want: t9,
		},
		{
			name: "no dispatch at all",
			evs:  []Event{dispatchEv(TypeDone, t9, "gt-a")},
			want: "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := DispatchStart(tt.evs, "gt-a")
			if tt.want == "" {
				if ok {
					t.Fatalf("DispatchStart = %v, want no clock", got)
				}
				return
			}
			if !ok || !got.Equal(mustTime(t, tt.want)) {
				t.Errorf("DispatchStart = %v (%t), want %v", got, ok, tt.want)
			}
		})
	}
}

// DispatchStarts is the whole-town reading the dashboard takes: one value per
// bead, each what DispatchStart answers for it.
func TestDispatchStartsMatchesDispatchStartForEveryBead(t *testing.T) {
	t.Parallel()
	evs := []Event{
		dispatchEv(TypeSling, t9, "gt-a"),
		dispatchEv(TypeSling, t10, "gt-b"),
		dispatchEv(TypeDone, t10, "gt-a"),
		dispatchEv(TypeSling, t11, "gt-a"),
		dispatchEv(TypeOverrunStop, t11, "gt-b"),
		dispatchEv(TypeSling, t12, "gt-b"),
	}
	got := DispatchStarts(evs)
	for bead, want := range map[string]string{"gt-a": t11, "gt-b": t12} {
		single, ok := DispatchStart(evs, bead)
		if !ok || !single.Equal(mustTime(t, want)) {
			t.Errorf("DispatchStart(%s) = %v (%t), want %v", bead, single, ok, want)
		}
		if all, ok := got[bead]; !ok || !all.Equal(single) {
			t.Errorf("DispatchStarts[%s] = %v (%t), want %v", bead, all, ok, single)
		}
	}
	if len(got) != 2 {
		t.Errorf("DispatchStarts dated %d beads, want 2: %v", len(got), got)
	}
}

// The clock a reader keeps current from the lines appended since its last read
// must answer what a whole-log read answers: the dashboard's Age column and the
// overrun guard read the same map (gt-tzbtf). Every chunk boundary is tried,
// including ones that split a bead's events.
func TestDispatchClockMatchesAWholeLogReadInAnyChunking(t *testing.T) {
	t.Parallel()
	evs := []Event{
		dispatchEv(TypeSling, t9, "gt-a"),
		dispatchEv(TypeSling, t10, "gt-b"),
		dispatchEv(TypeDone, t10, "gt-a"),
		dispatchEv(TypeSling, t11, "gt-a"),
		dispatchEv(TypeOverrunStop, t11, "gt-b"),
		dispatchEv(TypeSling, t12, "gt-b"),
		dispatchEv(TypeSling, t11, "gt-c"),
		dispatchEv(TypeBeadClosed, t12, "gt-c"),
		dispatchEv(TypeSling, t12, "gt-c"),
	}
	want := DispatchStarts(evs)
	for cut := 0; cut <= len(evs); cut++ {
		c := NewDispatchClock()
		c.Add(evs[:cut])
		c.Add(evs[cut:])
		got := c.Starts()
		if len(got) != len(want) {
			t.Fatalf("cut %d: clock dated %d beads, want %d: %v", cut, len(got), len(want), got)
		}
		for bead, at := range want {
			if g, ok := got[bead]; !ok || !g.Equal(at) {
				t.Errorf("cut %d: %s = %v (%t), want %v", cut, bead, g, ok, at)
			}
		}
	}
}

// Starts hands back a copy: a caller that keeps the map across polls must not
// see a later Add change it under the dashboard.
func TestDispatchClockStartsIsACopy(t *testing.T) {
	t.Parallel()
	c := NewDispatchClock()
	c.Add([]Event{dispatchEv(TypeSling, t9, "gt-a")})
	got := c.Starts()
	delete(got, "gt-a")
	if _, ok := c.Start("gt-a"); !ok {
		t.Fatal("deleting a bead from the returned map emptied the clock")
	}
}

// A sling dated before a run's end belongs to the run that ended, even when the
// log lists it after the end (a line written late): it must not start the next
// run's clock. A sling dated exactly at the end is the new run's dispatch
// (gt-tzbtf).
func TestDispatchStartIgnoresASlingDatedBeforeTheRunEnd(t *testing.T) {
	t.Parallel()
	late := []Event{
		dispatchEv(TypeDone, t11, "gt-a"),
		dispatchEv(TypeSling, t10, "gt-a"),
	}
	if got, ok := DispatchStart(late, "gt-a"); ok {
		t.Errorf("start = %v, want none: the only sling is dated before the run's end", got)
	}
	atEnd := []Event{
		dispatchEv(TypeDone, t11, "gt-a"),
		dispatchEv(TypeSling, t11, "gt-a"),
	}
	got, ok := DispatchStart(atEnd, "gt-a")
	if !ok || !got.Equal(mustTime(t, t11)) {
		t.Errorf("start = %v, %v, want %s: a sling dated at the end is the new run's dispatch", got, ok, t11)
	}
}

// A run ended outside the log — a submission for landing, whose done event can
// be lost — starts its successor at the first sling dated at or after that end,
// so a rework slung after a landing rejection is dated from its re-sling. A
// bead whose only sling is before the end is dated from the end itself until
// the re-sling is logged, and a bead the log never dispatched has no clock from
// this call at all (gt-chd44).
func TestDispatchClockStartSinceAnEndOutsideTheLog(t *testing.T) {
	t.Parallel()
	evs := []Event{
		dispatchEv(TypeSling, t9, "gt-a"),
		dispatchEv(TypeSling, t11, "gt-a"),
		dispatchEv(TypeSling, t11, "gt-b"),
	}
	for _, tt := range []struct {
		name string
		bead string
		end  string
		want string
		ok   bool
	}{
		{name: "a rework is dated from the first sling since the end", bead: "gt-a", end: "2026-10-27T10:30:00Z", want: t11, ok: true},
		{name: "a sling at the end is the new run's", bead: "gt-a", end: t11, want: t11, ok: true},
		{name: "an end after every sling dates the bead from the end", bead: "gt-a", end: "2026-10-27T13:00:00Z", want: "2026-10-27T13:00:00Z", ok: true},
		{name: "an end before the first sling leaves the clock alone", bead: "gt-a", end: "2026-10-27T08:00:00Z", want: t9, ok: true},
		{name: "no end is the log's clock", bead: "gt-a", want: t9, ok: true},
		{name: "a bead the log never dispatched has no clock", bead: "gt-none", end: t12, ok: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := NewDispatchClock()
			clock.Add(evs)
			var end time.Time
			if tt.end != "" {
				end = mustTime(t, tt.end)
			}
			got, ok := clock.StartSince(tt.bead, end)
			if ok != tt.ok {
				t.Fatalf("found = %t, want %t (start %v)", ok, tt.ok, got)
			}
			if tt.ok && !got.Equal(mustTime(t, tt.want)) {
				t.Errorf("start = %v, want %s", got, tt.want)
			}
		})
	}
}

// An end recorded outside the log is a boundary the clock keeps: folding the
// same slings in again, or a sling dated before the end arriving later, does
// not resurrect the dispatch it ended. A reader that rebuilds its clock from a
// replaced log depends on this.
func TestDispatchClockEndRunKeepsTheBoundary(t *testing.T) {
	t.Parallel()
	clock := NewDispatchClock()
	clock.Add([]Event{dispatchEv(TypeSling, t9, "gt-a")})
	clock.EndRun("gt-a", mustTime(t, t10))
	if _, ok := clock.Start("gt-a"); ok {
		t.Fatalf("the dispatch before the end is still the clock after EndRun")
	}

	clock.Add([]Event{dispatchEv(TypeSling, t9, "gt-a"), dispatchEv(TypeSling, t11, "gt-a")})
	if got, ok := clock.Start("gt-a"); !ok || !got.Equal(mustTime(t, t11)) {
		t.Errorf("start = %v, %v, want %s: the sling before the end came back", got, ok, t11)
	}

	clock.EndRun("gt-a", mustTime(t, t9))
	if got, ok := clock.Start("gt-a"); !ok || !got.Equal(mustTime(t, t11)) {
		t.Errorf("start = %v, %v, want %s: an older end moved the boundary back", got, ok, t11)
	}
}

// The spec dispatcher only slings a bead that is back in the pool, so no seat
// holds the run its earlier sling began: a sling it writes starts a fresh
// clock, and the time the held run spent is not the new seat's to answer for
// (gt-3ga5l).
func TestDispatchClockSpecDispatchSlingStartsNewRun(t *testing.T) {
	t.Parallel()
	clock := NewDispatchClock()
	clock.Add(specSlingRun)
	// The bead was submitted for landing at 12:20:47, between the two runs.
	got, ok := clock.StartSince("gt-a", mustTime(t, "2026-10-30T12:20:47Z"))
	if !ok || !got.Equal(mustTime(t, "2026-10-30T14:27:05Z")) {
		t.Errorf("start = %v (%t), want 2026-10-30T14:27:05Z: the re-sling starts the new run", got, ok)
	}
}

// Anyone but the spec dispatcher keeps today's rule: a bead slung from one
// seat to another keeps the dispatch it first went out on, so moving between
// seats cannot restart its clock.
func TestDispatchClockOtherActorSlingKeepsTheRun(t *testing.T) {
	t.Parallel()
	clock := NewDispatchClock()
	clock.Add([]Event{
		dispatchSlingEv(t9, "gt-a", "hq-crew-egret", "gastown/nux"),
		dispatchSlingEv(t11, "gt-a", "hq-crew-egret", "gastown/capable"),
	})
	if got, ok := clock.Start("gt-a"); !ok || !got.Equal(mustTime(t, t9)) {
		t.Errorf("start = %v (%t), want %s: a seat-to-seat sling keeps the first dispatch", got, ok, t9)
	}
}

// The spec-dispatch rule is both order and replay safe: folding the same lines
// in twice, or folding in a run end for the run before the last sling, leaves
// the new run's clock where that sling put it.
func TestDispatchClockSpecDispatchSlingIsReplaySafe(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		chunks [][]Event
	}{
		{
			name:   "the log in order",
			chunks: [][]Event{specSlingRun},
		},
		{
			name:   "the same lines folded twice",
			chunks: [][]Event{specSlingRun, specSlingRun},
		},
		{
			name: "the submitted run's end between the slings",
			chunks: [][]Event{
				specSlingRun[:3],
				{dispatchEv(TypeDone, "2026-10-30T12:20:47Z", "gt-a")},
				specSlingRun[3:],
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := NewDispatchClock()
			for _, chunk := range tt.chunks {
				clock.Add(chunk)
			}
			if got, ok := clock.Start("gt-a"); !ok || !got.Equal(mustTime(t, "2026-10-30T14:27:05Z")) {
				t.Errorf("start = %v (%t), want 2026-10-30T14:27:05Z", got, ok)
			}
		})
	}
}

// A repeat spec-dispatch sling to the seat that already holds the bead is the
// same run: it must not restart the clock, or a repeated sling could keep a
// runaway bead from ever being stopped.
func TestDispatchClockSpecDispatchRepeatToTheSameSeatKeepsTheRun(t *testing.T) {
	t.Parallel()
	clock := NewDispatchClock()
	clock.Add([]Event{
		dispatchSlingEv("2026-10-30T12:00:00Z", "gt-a", SpecDispatchActor, "gastown/nux"),
		dispatchSlingEv("2026-10-30T12:10:00Z", "gt-a", SpecDispatchActor, "gastown/nux"),
	})
	if got, ok := clock.Start("gt-a"); !ok || !got.Equal(mustTime(t, "2026-10-30T12:00:00Z")) {
		t.Errorf("start = %v (%t), want 12:00:00Z: a repeat to the same seat keeps the first dispatch", got, ok)
	}
}

// A same-seat sling after a real stop is a new run, because the stop wrote its
// own run end.
func TestDispatchClockStopThenSameSeatSlingStartsNewRun(t *testing.T) {
	t.Parallel()
	clock := NewDispatchClock()
	clock.Add([]Event{
		dispatchSlingEv("2026-10-30T12:00:00Z", "gt-a", SpecDispatchActor, "gastown/nux"),
		dispatchEv(TypeOverrunStop, "2026-10-30T12:30:00Z", "gt-a"),
		dispatchSlingEv("2026-10-30T13:00:00Z", "gt-a", SpecDispatchActor, "gastown/nux"),
	})
	if got, ok := clock.Start("gt-a"); !ok || !got.Equal(mustTime(t, "2026-10-30T13:00:00Z")) {
		t.Errorf("start = %v (%t), want 13:00:00Z: a stop ends the run, so the next sling starts a new one", got, ok)
	}
}

// DispatchedBy reads the run that was current at a bead's end, so a landing can
// time a bead whose seat wrote no start event. The run's own done event is at
// or after that end, so the clock must not let it drop the sling (gt-v7ptp).
func TestDispatchedByReadsTheRunCurrentAtEnd(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		evs  []Event
		end  string
		want string // "" for not found
	}{
		{
			name: "the done event at the end does not drop the run's sling",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				dispatchEv(TypeDone, t11, "gt-a"),
			},
			end:  t11,
			want: t9,
		},
		{
			name: "a rework after the end is not what the landing times",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				dispatchEv(TypeDone, t10, "gt-a"),
				dispatchEv(TypeSling, t11, "gt-a"),
				dispatchEv(TypeDone, t12, "gt-a"),
			},
			end:  t10,
			want: t9,
		},
		{
			name: "a re-sling to the same seat before the end keeps the run's first dispatch",
			evs: []Event{
				dispatchEv(TypeSling, t9, "gt-a"),
				dispatchEv(TypeSling, t10, "gt-a"),
				dispatchEv(TypeDone, t11, "gt-a"),
			},
			end:  t11,
			want: t9,
		},
		{
			name: "a bead the log never dispatched has no clock",
			evs:  []Event{dispatchEv(TypeSling, t9, "gt-b")},
			end:  t11,
			want: "",
		},
		{
			name: "a sling at or after the end dates no run before it",
			evs:  []Event{dispatchEv(TypeSling, t11, "gt-a")},
			end:  t11,
			want: "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			end := mustTime(t, tt.end)
			got, seat, ok := DispatchedBy(tt.evs, "gt-a", end)
			if tt.want == "" {
				if ok {
					t.Errorf("DispatchedBy = %v; want no dispatch", got)
				}
				if seat != "" {
					t.Errorf("seat = %q; want none with no dispatch", seat)
				}
				return
			}
			if !ok || !got.Equal(mustTime(t, tt.want)) {
				t.Errorf("DispatchedBy = %v (%t), want %s", got, ok, tt.want)
			}
			// the seat is the one the dated sling named, so a landing can tell
			// whose clock it is reading (gt-v7ptp).
			if want := "gastown/polecats/gt-a"; seat != want {
				t.Errorf("seat = %q, want %q", seat, want)
			}
		})
	}
}
