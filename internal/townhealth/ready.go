package townhealth

import (
	"fmt"
	"sort"
	"time"
)

// Ready reports whether the town is fit to take traffic, which is a different
// question from whether it is green. A degraded or red report is ready when
// every non-green field is RECORDED: the condition is a known, accepted state
// (a rehearsal with dispatch off, a backup not yet taken) and not a fault a
// measurement found. A non-green attention row is ready too: it says a person
// should look, not that the daemon cannot serve, and a person's queue must not
// take a working daemon's Service endpoints away (gt-pittz6). Anything else is
// not ready: a stale or unknown report, a measured non-green serving field, an
// UNKNOWN field, and a red or degraded report with no non-green field to
// account for it.
//
// reason names the worst blocking field, in the order Line lists the fields; an
// attention row is never the reason.
func Ready(r Report, now time.Time, staleAfter time.Duration) (ok bool, reason string) {
	v := Effective(r, now, staleAfter)
	if v != r.Verdict {
		return false, fmt.Sprintf("report is stale (last %s)", r.Verdict)
	}
	switch v {
	case Green:
		return true, ""
	case Degraded, Red:
	default:
		return false, "verdict is unknown"
	}
	bad := nonGreen(r.Fields)
	if len(bad) == 0 {
		return false, fmt.Sprintf("%s verdict has no non-green field to record", v)
	}
	for _, f := range bad {
		if f.Verdict == VerdictUnknown {
			return false, fmt.Sprintf("%s verdict is unknown", f.Key())
		}
		if f.Tag != Recorded && !attention(f) {
			return false, fmt.Sprintf("%s is %s, not RECORDED", f.Key(), f.Tag)
		}
	}
	return true, ""
}

// attention reports whether f is a row that says a person should look, not that
// the daemon cannot serve: a bead waiting under a person label, or an open
// escalation. Such a row stays in the status line but does not fail readiness
// (gt-pittz6). Every other name is a serving row, and one this function does
// not know is not excused. An UNKNOWN attention row is not excused either: its
// read failed, and an unknown field is not ready.
func attention(f Field) bool {
	switch f.Name {
	case FieldNeedsOverseer, FieldNeedsOperator, FieldNeedsHuman, FieldEscalation:
		return true
	}
	return false
}

// nonGreen returns the non-green fields, worst first, as Line orders them; ties keep report order.
func nonGreen(fields []Field) []Field {
	var bad []Field
	for _, f := range fields {
		if f.Verdict != Green {
			bad = append(bad, f)
		}
	}
	sort.SliceStable(bad, func(i, j int) bool { return bad[i].Verdict.rank() > bad[j].Verdict.rank() })
	return bad
}
