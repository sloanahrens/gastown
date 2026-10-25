package metrics

import "math"

// wilsonZ is the two-sided 95% z-score of the panel's intervals.
const wilsonZ = 1.96

// wilsonInterval is the 95% Wilson score interval for k positives among n
// trials, and false when n is zero and there is nothing to estimate. The score
// interval keeps its bounds inside 0..1 where the normal approximation
// collapses at k=0 and k=n; the clamp holds that against floating-point
// rounding at the ends.
func wilsonInterval(k, n int) (low, high float64, ok bool) {
	if n == 0 {
		return 0, 0, false
	}
	nf := float64(n)
	p := float64(k) / nf
	z2 := wilsonZ * wilsonZ
	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	half := wilsonZ * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf)) / denom
	return math.Max(0, center-half), math.Min(1, center+half), true
}
