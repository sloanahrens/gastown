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
