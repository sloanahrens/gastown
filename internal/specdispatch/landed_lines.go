package specdispatch

// landedLines is the added lines beads landed with, grouped by the minutes
// their bead was estimated at (gt-ysvmfw). It is read from the landings table
// on 2026-11-22 and counts only landings that carry both an sp_minutes estimate
// and a lines_added diff size. A band holds minutes from MinMinutes up to, not
// including, MaxMinutes. Recalibrate from the landings table when the estimator
// changes.
var landedLines = []LinesBand{
	{0, 5, 70, 2, 4, 9, 27},
	{5, 7, 293, 10, 30, 64, 101},
	{7, 9, 721, 38, 67, 119, 236},
	{9, 10, 647, 58, 109, 216, 454},
	{10, 12, 489, 111, 246, 468, 794},
	{12, 16, 76, 139, 256, 475, 859},
}

// LinesBand is one row of the landed-lines table: the N landings whose
// estimate fell in [MinMinutes, MaxMinutes), and their added lines at the P25,
// Median, P75 and P90 percentiles.
type LinesBand struct {
	MinMinutes, MaxMinutes   float64
	N, P25, Median, P75, P90 int
}

// LandedLines returns the band holding minutes; found is false below 0 and at
// 16 and above, where the table does not reach.
func LandedLines(minutes float64) (LinesBand, bool) {
	for _, band := range landedLines {
		if minutes >= band.MinMinutes && minutes < band.MaxMinutes {
			return band, true
		}
	}
	return LinesBand{}, false
}

// LineFloor is the Median of the band the bead's sp-minutes estimate reads
// (advice.go). found is false when no estimate reads or the minutes fall outside
// the table, so the gate reads no floor rather than a guessed one.
func LineFloor(labels []string) (int, bool) {
	minutes, ok := spMinutes(labels)
	if !ok {
		return 0, false
	}
	band, ok := LandedLines(minutes)
	if !ok {
		return 0, false
	}
	return band.Median, true
}
