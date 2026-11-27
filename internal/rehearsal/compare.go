package rehearsal

import (
	"fmt"
	"strings"
)

// Compare writes two runs side by side: each run's id, change set and Judge
// verdict, then the gates nobody could measure, then one count line per measure
// with both values. A count over an empty sling or gate list prints "none";
// an empty seat, landing, call or intervention list is a measured zero.
func Compare(before, after Record) string {
	var b strings.Builder
	for _, r := range []Record{before, after} {
		fmt.Fprintf(&b, "%s (%s): %s\n", r.RunID, r.ChangeSet, verdictText(Judge(r)))
	}
	count := func(name, bef, aft string) {
		fmt.Fprintf(&b, "%s: %s | %s\n", name, bef, aft)
	}
	count("gates unavailable",
		countText(before.Gates, unavailableGate), countText(after.Gates, unavailableGate))
	count("slings not ok",
		countText(before.Slings, notOKSling), countText(after.Slings, notOKSling))
	count("gates not passing",
		countText(before.Gates, notPassingGate), countText(after.Gates, notPassingGate))
	count("stranded seats", fmt.Sprint(len(before.Stranded)), fmt.Sprint(len(after.Stranded)))
	count("CI failures", fmt.Sprint(ciFailures(before)), fmt.Sprint(ciFailures(after)))
	count("failed calls", fmt.Sprint(len(before.FailedCalls)), fmt.Sprint(len(after.FailedCalls)))
	count("interventions", fmt.Sprint(len(before.Interventions)), fmt.Sprint(len(after.Interventions)))
	return b.String()
}

// verdictText is Judge's verdict in Judge's own words.
func verdictText(v Verdict) string {
	if v.Pass {
		return "pass"
	}
	return "fail: " + strings.Join(v.Reasons, "; ")
}

// unavailableGate reports a gate nobody could measure.
func unavailableGate(g Gate) bool { return g.Verdict == GateUnavailable }

// notOKSling reports a sling that did not finish ok; an outcome the record
// does not name counts here rather than reading as ok.
func notOKSling(s Sling) bool { return s.Outcome != SlingOK }

// notPassingGate reports a gate that failed or carries a verdict the run never
// records: only an unavailable gate is missing data rather than a failure.
func notPassingGate(g Gate) bool { return g.Verdict != GatePass && g.Verdict != GateUnavailable }

// ciFailures counts the landings that failed CI.
func ciFailures(r Record) int {
	n := 0
	for _, l := range r.Landings {
		if l.CIFailed {
			n++
		}
	}
	return n
}

// countText counts the xs pred selects, and is none when xs is empty: a count
// over an unmeasured list would read as a clean zero.
func countText[T any](xs []T, pred func(T) bool) string {
	if len(xs) == 0 {
		return "none"
	}
	n := 0
	for _, x := range xs {
		if pred(x) {
			n++
		}
	}
	return fmt.Sprint(n)
}
