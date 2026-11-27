package rehearsal

import (
	"fmt"
	"strings"
)

// Compare writes two runs side by side: each run's id, change set and Judge
// verdict, then the gates nobody could measure, then one count line per measure
// with both values. A count over an empty sling or gate list prints "none";
// an empty seat, landing, call or intervention list is a measured zero. After
// the counts come one line per figure with both values: the sling p90 over every
// sling, the gate p90 over gates with a measured wall, each side run's wall in
// order, and the load max and p90; a figure with no data prints "none".
func Compare(before, after Record) string {
	var b strings.Builder
	for _, r := range []Record{before, after} {
		fmt.Fprintf(&b, "%s (%s): %s\n", r.RunID, r.ChangeSet, verdictText(Judge(r)))
	}
	pair := func(name, bef, aft string) {
		fmt.Fprintf(&b, "%s: %s | %s\n", name, bef, aft)
	}
	pair("gates unavailable",
		countText(before.Gates, unavailableGate), countText(after.Gates, unavailableGate))
	pair("slings not ok",
		countText(before.Slings, notOKSling), countText(after.Slings, notOKSling))
	pair("gates not passing",
		countText(before.Gates, notPassingGate), countText(after.Gates, notPassingGate))
	pair("stranded seats", fmt.Sprint(len(before.Stranded)), fmt.Sprint(len(after.Stranded)))
	pair("CI failures", fmt.Sprint(ciFailures(before)), fmt.Sprint(ciFailures(after)))
	pair("failed calls", fmt.Sprint(len(before.FailedCalls)), fmt.Sprint(len(after.FailedCalls)))
	pair("interventions", fmt.Sprint(len(before.Interventions)), fmt.Sprint(len(after.Interventions)))
	pair("sling p90", p90Text(slingWalls(before)), p90Text(slingWalls(after)))
	pair("gate p90", p90Text(measuredGateWalls(before)), p90Text(measuredGateWalls(after)))
	pair("side run walls", wallsText(before.SideRuns), wallsText(after.SideRuns))
	pair("load max", loadText(before.Load, before.Load.Max1), loadText(after.Load, after.Load.Max1))
	pair("load p90", loadText(before.Load, before.Load.P90One), loadText(after.Load, after.Load.P90One))
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

// p90Text is xs's nearest-rank p90 by Judge's function, none when xs is empty.
func p90Text(xs []float64) string {
	p, ok := nearestRankP90(xs)
	if !ok {
		return "none"
	}
	return fmt.Sprintf("%g", p)
}

// slingWalls is every sling's wall: Judge's sling p90 counts them all.
func slingWalls(r Record) []float64 {
	walls := make([]float64, len(r.Slings))
	for i, s := range r.Slings {
		walls[i] = s.Seconds
	}
	return walls
}

// measuredGateWalls drops a zero-second gate: the run never measured it.
func measuredGateWalls(r Record) []float64 {
	var walls []float64
	for _, g := range r.Gates {
		if g.Seconds != 0 {
			walls = append(walls, g.Seconds)
		}
	}
	return walls
}

// wallsText lists each side run's wall, and is none when there was no side run.
func wallsText(runs []SideRun) string {
	if len(runs) == 0 {
		return "none"
	}
	walls := make([]string, len(runs))
	for i, r := range runs {
		walls[i] = fmt.Sprintf("%g", r.Seconds)
	}
	return strings.Join(walls, ", ")
}

// loadText is one load figure, none when the record holds no sample.
func loadText(l Load, figure float64) string {
	if l.Samples <= 0 {
		return "none"
	}
	return fmt.Sprintf("%g", figure)
}
