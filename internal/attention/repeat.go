package attention

import "time"

// Reconcile raises an item once, when it first appears. A condition that keeps
// holding and still needs somebody — an answer the overseer has not acted on, a
// question nobody has closed — has to be said again or the one line it wrote
// scrolls past. A rule in repeatEvery is said again at most that often while
// its item holds.
var repeatEvery = map[string]time.Duration{
	// Sloan's answer waits on the overseer; every 20 minutes until it comments.
	RuleQuestionAnswered: 20 * time.Minute,
	// A question open for a day is mentioned every 6 hours, as info.
	RuleQuestionStale: 6 * time.Hour,
}

// RepeatEvery is how often rule is said again while its item holds, and false
// for a rule that is said once.
func RepeatEvery(rule string) (time.Duration, bool) {
	d, ok := repeatEvery[rule]
	return d, ok
}

// Repeats returns the lines that say a still-holding item again, and the
// updated record of when each repeating item was last said. It is pure: last is
// the record this call is handed, and the caller keeps what it returns.
//
// An item is said at the moment it is raised, by Reconcile, which is when its
// clock starts: events are that tick's transitions. An item with no record —
// the daemon restarted since it was raised — starts its clock now, so a restart
// delays a repeat and never doubles one. An acked item is not repeated, since
// an ack hides an item until it clears. A repeat carries its rule's flags, as
// the raised line did.
func Repeats(state State, events []Event, last map[string]time.Time, now time.Time) (map[string]time.Time, []Event) {
	next := make(map[string]time.Time, len(last))
	raised := map[string]bool{}
	for _, e := range events {
		if e.State == EventNew {
			raised[e.Key] = true
		}
	}
	var out []Event
	for _, it := range state.Items {
		rule := RuleFor(it)
		every, ok := RepeatEvery(rule)
		if !ok {
			continue
		}
		at, known := last[it.Key]
		if raised[it.Key] || !known {
			next[it.Key] = now
			continue
		}
		next[it.Key] = at
		if it.AckedAt != nil || now.Sub(at) < every {
			continue
		}
		next[it.Key] = now
		f, _ := Flags(rule)
		out = append(out, Event{
			TS:       now,
			Class:    it.Kind,
			Severity: it.Severity,
			Text:     it.Summary + " (still holding)",
			Key:      it.Key,
			State:    EventNew,
			Rule:     rule,
			Handled:  f.Handled,
			Wake:     f.Wake,
		})
	}
	return next, out
}
