package beads

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// censusLogRows is how many of the busiest verb@caller rows a census line
// names; the rest stay counted in the total (gt-7h3vm).
const censusLogRows = 8

// StartCensusLog blocks while it logs one line per receive from every naming
// the bd subprocesses started since the previous one, most frequent first
// (gt-7h3vm); it returns when ctx is done or every closes.
func StartCensusLog(ctx context.Context, every <-chan time.Time, logf func(format string, args ...any)) {
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case tick, ok := <-every:
			if !ok {
				return
			}
			last = censusLogWindow(tick, last, logf)
		}
	}
}

// censusLogWindow logs and clears the calls counted since the previous tick,
// and returns the tick that opens the next window. The window comes from the
// tick times rather than the wall clock, so a caller that fabricates ticks
// gets the frame it asked for.
func censusLogWindow(tick, last time.Time, logf func(format string, args ...any)) time.Time {
	seconds := int(tick.Sub(last).Seconds())
	if seconds < 0 {
		seconds = 0
	}

	rows := CensusSnapshotAndReset()
	total := 0
	for _, row := range rows {
		total += row.Count
	}
	if total == 0 {
		return tick
	}
	if len(rows) > censusLogRows {
		rows = rows[:censusLogRows]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "bd census: total=%d window=%ds top:", total, seconds)
	for _, row := range rows {
		fmt.Fprintf(&b, " %s@%s=%d", row.Verb, row.Caller, row.Count)
	}
	logf("%s", b.String())
	return tick
}
