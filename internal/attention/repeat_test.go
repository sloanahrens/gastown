package attention

import (
	"testing"
	"time"
)

func answeredState(at time.Time, acked bool) State {
	it := Item{Key: "question:answered:hq-q1", Kind: KindQuestionAnswered, Severity: SeverityHigh, Summary: "answered (hq-q1)", FirstSeen: at}
	if acked {
		it.AckedAt = &at
	}
	return State{Updated: at, Items: []Item{it}}
}

// The answered rule repeats no more than every 20 minutes while its item
// holds, and each repeat carries the rule's flags.
func TestQuestionAnsweredRepeatsAtMostEveryTwentyMinutes(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	state := answeredState(t0, false)

	raised := []Event{{Key: "question:answered:hq-q1", State: EventNew}}
	last, got := Repeats(state, raised, nil, t0)
	if len(got) != 0 {
		t.Fatalf("the tick that raised the item repeated it: %+v", got)
	}

	var repeats []time.Duration
	for _, min := range []int{5, 19, 20, 25, 39, 40, 61} {
		at := t0.Add(time.Duration(min) * time.Minute)
		var evs []Event
		last, evs = Repeats(state, nil, last, at)
		if len(evs) > 0 {
			repeats = append(repeats, at.Sub(t0))
			e := evs[0]
			if e.Rule != RuleQuestionAnswered || !e.Wake || e.Handled != HandledOperatorAction || e.State != EventNew {
				t.Errorf("repeat = %+v, want the answered rule's wake flags", e)
			}
		}
	}
	want := []time.Duration{20 * time.Minute, 40 * time.Minute, 61 * time.Minute}
	if len(repeats) != len(want) {
		t.Fatalf("repeated at %v, want %v", repeats, want)
	}
	for i := range want {
		if repeats[i] != want[i] {
			t.Errorf("repeated at %v, want %v", repeats, want)
		}
	}
}

// A restart loses the clocks: an item with no record starts its clock then,
// so a restart delays a repeat and never doubles one.
func TestQuestionRepeatAfterRestartStartsTheClockAgain(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	state := answeredState(t0.Add(-3*time.Hour), false)
	last, got := Repeats(state, nil, nil, t0)
	if len(got) != 0 {
		t.Fatalf("a restart repeated an item it had no clock for: %+v", got)
	}
	if _, got := Repeats(state, nil, last, t0.Add(21*time.Minute)); len(got) != 1 {
		t.Errorf("repeats a full interval after the restart = %+v, want one", got)
	}
}

// An acked item is not repeated, and an item that cleared leaves no clock.
func TestQuestionRepeatSkipsAckedAndForgetsCleared(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	last := map[string]time.Time{"question:answered:hq-q1": t0}
	if _, got := Repeats(answeredState(t0, true), nil, last, t0.Add(time.Hour)); len(got) != 0 {
		t.Errorf("an acked item repeated: %+v", got)
	}
	next, got := Repeats(State{}, nil, last, t0.Add(time.Hour))
	if len(got) != 0 || len(next) != 0 {
		t.Errorf("a cleared item left repeats %+v and clocks %v", got, next)
	}
}

// Only the rules in the table repeat; the rest are said once.
func TestQuestionRepeatIsForTheQuestionRulesOnly(t *testing.T) {
	t.Parallel()
	for _, rule := range Rules() {
		_, ok := RepeatEvery(rule)
		want := rule == RuleQuestionAnswered || rule == RuleQuestionStale
		if ok != want {
			t.Errorf("RepeatEvery(%s) = %v, want %v", rule, ok, want)
		}
	}
	if d, _ := RepeatEvery(RuleQuestionStale); d != 6*time.Hour {
		t.Errorf("stale repeats every %v, want 6h", d)
	}
}
