package daemon

import (
	"math"
	"time"
)

// jsonlBudgetMargin doubles the estimate so an export that runs somewhat
// slower than its measured rate still finishes inside its budget.
const jsonlBudgetMargin = 2

// jsonlIssuesBudgetCeiling caps the gt issues export budget, so a very large
// host slowdown cannot let one table's export hold the backup cycle open.
const jsonlIssuesBudgetCeiling = 10 * time.Minute

// jsonlTableBudget returns how long one table's export may run: rows times
// perRow times slowdown times jsonlBudgetMargin, clamped to [floor, ceiling].
// A slowdown below 1, NaN or infinite counts as 1; zero or negative rows or
// perRow give the floor. A floor above the ceiling gives the ceiling.
func jsonlTableBudget(rows int, perRow time.Duration, slowdown float64, floor, ceiling time.Duration) time.Duration {
	if math.IsNaN(slowdown) || math.IsInf(slowdown, 0) || slowdown < 1 {
		slowdown = 1
	}
	est := 0.0
	if rows > 0 && perRow > 0 {
		est = float64(rows) * float64(perRow) * slowdown * jsonlBudgetMargin
	}
	// Compared in float before the conversion: converting an out-of-range
	// float to time.Duration is implementation-defined and can wrap negative
	// on some platforms, which the floor clamp would then hide.
	if est >= float64(ceiling) {
		return ceiling
	}
	return min(max(time.Duration(est), floor), ceiling)
}
