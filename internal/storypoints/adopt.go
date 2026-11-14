package storypoints

import (
	"cmp"
	"fmt"
	"math"
	"slices"
)

// AdoptBar is the rank correlation a proposed model must reach on its own holdout
// before any of its weights can be adopted: the design's bar (gt-5d9k2f).
const AdoptBar = 0.4

// AdoptMargin is how much lower than the checked-in weights' rank correlation the
// proposed model's may be on the same holdout and still be adopted. A retrain that
// ranks the latest beads no better than the weights in use is not worth a new file,
// and one that ranks them far worse is a regression whatever its absolute score.
const AdoptMargin = 0.03

// MinAdoptHoldout is the fewest holdout beads a rank correlation is read from. Below
// it a correlation says too little to adopt or reject on, so the holdout is not
// measurable and the proposal is refused.
const MinAdoptHoldout = 10

// marginTolerance absorbs the float error of the two subtractions that compare a
// correlation with the margin: two near-two-decimal correlations can differ by
// 0.030000000000000027, which is not more than the 0.03 the rule allows.
const marginTolerance = 1e-9

// Adoption is the adoption rule's decision and the one-line reason for it. A
// rejection is a decision, not an error: the caller reports the reason and leaves
// the weights in use unchanged.
type Adoption struct {
	Adopted bool
	Reason  string
}

// Adopt decides whether a retrain's proposed weights replace the checked-in ones.
// The holdout is the retrain's own: the measured minutes of its latest beads, and
// the blend of the shape and embedding predictions that the proposed weights and the
// checked-in weights each make for them.
//
// A proposal is adopted only when its blend's rank correlation against the measured
// minutes reaches AdoptBar and is not more than AdoptMargin below the checked-in
// blend's on the same holdout. Every input the comparison cannot be made from is a
// rejection: a holdout under MinAdoptHoldout beads, a value that is not a positive
// finite number, and a series in which every bead has the same value. None of those
// is read as a correlation of zero, because zero is a measurement and these are not.
func Adopt(holdout []Holdout) Adoption {
	if len(holdout) < MinAdoptHoldout {
		return Adoption{Reason: fmt.Sprintf(
			"rejected: the holdout has %d beads, fewer than the %d a rank correlation is read from, so the holdout is not measurable",
			len(holdout), MinAdoptHoldout)}
	}
	actual := make([]float64, len(holdout))
	proposed := make([]float64, len(holdout))
	checked := make([]float64, len(holdout))
	for i, h := range holdout {
		for _, v := range []struct {
			name  string
			value float64
		}{
			{"measured", h.Actual},
			{"proposed", h.Proposed},
			{"current", h.Checked},
		} {
			if !(v.value > 0) || math.IsInf(v.value, 0) {
				return Adoption{Reason: fmt.Sprintf(
					"rejected: holdout bead %d has %s minutes %v, want a positive finite number, so the holdout is not measurable",
					i, v.name, v.value)}
			}
		}
		actual[i], proposed[i], checked[i] = h.Actual, h.Proposed, h.Checked
	}

	for _, s := range []struct {
		name   string
		values []float64
		reason string
	}{
		{"measured", actual, "the measured minutes are one value on every holdout bead, so no rank correlation is measurable"},
		{"proposed", proposed, "the proposed blend is one value on every holdout bead, so its rank correlation is not measurable"},
		{"current", checked, "the current weights' blend is one value on every holdout bead, so the comparison with it cannot be made"},
	} {
		if !hasSpread(s.values) {
			return Adoption{Reason: "rejected: " + s.reason}
		}
	}

	proposedRho := rankCorrelation(actual, proposed)
	checkedRho := rankCorrelation(actual, checked)
	if proposedRho < AdoptBar {
		return Adoption{Reason: fmt.Sprintf(
			"rejected: holdout rank correlation %.3f is below the %.2f bar", proposedRho, AdoptBar)}
	}
	if worse := checkedRho - proposedRho; worse > AdoptMargin+marginTolerance {
		return Adoption{Reason: fmt.Sprintf(
			"rejected: holdout rank correlation %.3f is %.3f below the current weights' %.3f, more than the %.2f allowed",
			proposedRho, worse, checkedRho, AdoptMargin)}
	}
	return Adoption{Adopted: true, Reason: fmt.Sprintf(
		"adopted: holdout rank correlation %.3f clears the %.2f bar and is not more than %.2f below the current weights' %.3f",
		proposedRho, AdoptBar, AdoptMargin, checkedRho)}
}

// hasSpread reports whether v holds more than one value. Adopt calls it only after
// rejecting values that are not positive finite numbers, so a NaN is not read here
// as a value that spreads.
func hasSpread(v []float64) bool {
	for _, x := range v {
		if x != v[0] {
			return true
		}
	}
	return false
}

// rankCorrelation is the Pearson correlation of the average ranks of x and y: the
// rank correlation. Both series must have spread, which Adopt checks.
func rankCorrelation(x, y []float64) float64 {
	rx, ry := averageRanks(x), averageRanks(y)
	var mx, my float64
	for i := range rx {
		mx += rx[i] / float64(len(rx))
		my += ry[i] / float64(len(ry))
	}
	var cov, vx, vy float64
	for i := range rx {
		dx, dy := rx[i]-mx, ry[i]-my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	// Rounded to the interval's ends, so a correlation of exactly one is not reported
	// as 1.0000000000000002 by the arithmetic.
	return math.Max(-1, math.Min(1, cov/math.Sqrt(vx*vy)))
}

// averageRanks is each value's 1-based rank, tied values sharing the mean of the
// ranks they span.
func averageRanks(v []float64) []float64 {
	order := make([]int, len(v))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return cmp.Compare(v[a], v[b]) })
	ranks := make([]float64, len(v))
	for i := 0; i < len(order); {
		j := i
		for j+1 < len(order) && v[order[j+1]] == v[order[i]] {
			j++
		}
		mean := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			ranks[order[k]] = mean
		}
		i = j + 1
	}
	return ranks
}
