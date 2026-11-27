package rehearsal

import (
	"fmt"
	"sort"
)

// Verdict is the Judge's call on one record; Reasons names each measure it broke.
type Verdict struct {
	Pass    bool
	Reasons []string
}

// Ruled p90 limits in seconds (operator measures, 2026-11-26). A p90 equal to its limit passes.
const (
	slingP90Limit     = 120
	presubmitP90Limit = 472
)

// Judge applies the ruled measures to r. A sling that did not finish ok, a
// stranded seat, a landing whose CI failed, a bead that did not land within the
// run, a failed call, an intervention and a stopped run each fail with their own
// reason. The sling p90 counts every sling, the presubmit p90 every gate with a
// measured wall whatever its verdict: a zero-second gate was never measured and
// is left out. A record with fewer than setSize slings, no measured gate and no
// host load sample is unmeasured and fails with a reason of its own for each: an
// unmeasured run never passes. Landing CI time and the dispatcher tick are not
// judged.
func Judge(r Record) Verdict {
	var reasons []string
	var slingSecs, gateSecs []float64
	for _, s := range r.Slings {
		if s.Outcome != SlingOK {
			reasons = append(reasons, fmt.Sprintf("sling %s outcome %q is not ok", s.Bead, s.Outcome))
		}
		slingSecs = append(slingSecs, s.Seconds)
	}
	for _, g := range r.Gates {
		if g.Seconds != 0 {
			gateSecs = append(gateSecs, g.Seconds)
		}
	}
	for _, seat := range r.Stranded {
		reasons = append(reasons, "stranded seat "+seat)
	}
	for _, l := range r.Landings {
		if l.CIFailed {
			reasons = append(reasons, fmt.Sprintf("landing %s failed CI", l.Bead))
		}
		if !l.Landed {
			reasons = append(reasons, fmt.Sprintf("bead %s did not land within the run", l.Bead))
		}
	}
	for _, c := range r.FailedCalls {
		reasons = append(reasons, "failed call: "+c)
	}
	for _, iv := range r.Interventions {
		reasons = append(reasons, "intervention: "+iv)
	}
	if r.Stopped {
		reasons = append(reasons, "run stopped: "+r.StopReason)
	}
	if n := len(r.Slings); n < setSize {
		reasons = append(reasons, fmt.Sprintf("only %d slings, want %d: the run is unmeasured", n, setSize))
	}
	if len(gateSecs) == 0 {
		reasons = append(reasons, "no gate figure: the run is unmeasured")
	}
	if r.Load.Samples <= 0 {
		reasons = append(reasons, "no host load samples: the run is unmeasured")
	}
	if p, ok := nearestRankP90(slingSecs); ok && p > slingP90Limit {
		reasons = append(reasons, fmt.Sprintf("sling p90 %g s is over %d s", p, slingP90Limit))
	}
	if p, ok := nearestRankP90(gateSecs); ok && p > presubmitP90Limit {
		reasons = append(reasons, fmt.Sprintf("presubmit p90 %g s is over %d s", p, presubmitP90Limit))
	}
	return Verdict{Pass: len(reasons) == 0, Reasons: reasons}
}

// nearestRankP90 returns the nearest-rank 90th percentile of xs, leaving xs
// unchanged, and false when xs is empty.
func nearestRankP90(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	rank := (9*len(sorted) + 9) / 10 // ceil(0.9 * n) in integers
	return sorted[rank-1], true
}
