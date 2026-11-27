//go:build !race

package scrub

// 10 MB and a twentieth of the time left: only this run exposes a CPU-only quadratic.
const (
	safeInputSize     = 10 << 20
	safeBudgetDivisor = 20
)
