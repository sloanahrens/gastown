//go:build race

package scrub

// 1 MB and half the time left: under race speed only a memory quadratic shows.
const (
	safeInputSize     = 1 << 20
	safeBudgetDivisor = 2
)
