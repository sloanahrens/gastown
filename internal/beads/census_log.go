package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	if total := CensusRowsTotal(rows); total > 0 {
		logf("bd census: total=%d window=%ds %s", total, seconds, CensusTopRows(rows))
	}
	return tick
}

// CensusRowsTotal is the calls every row counts together.
func CensusRowsTotal(rows []CensusRow) int {
	total := 0
	for _, row := range rows {
		total += row.Count
	}
	return total
}

// CensusTopRows renders rows as the "top: verb@caller=count ..." text a census
// line carries, naming the censusLogRows busiest; it is empty for no rows.
func CensusTopRows(rows []CensusRow) string {
	if len(rows) == 0 {
		return ""
	}
	if len(rows) > censusLogRows {
		rows = rows[:censusLogRows]
	}
	var b strings.Builder
	b.WriteString("top:")
	for _, row := range rows {
		fmt.Fprintf(&b, " %s@%s=%d", row.Verb, row.Caller, row.Count)
	}
	return b.String()
}

// WriteCensusSink writes rows to path as one "verb@caller=count" line each, for
// the process that started this one to log (gt-139dp).
func WriteCensusSink(path string, rows []CensusRow) error {
	var b strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&b, "%s@%s=%d\n", row.Verb, row.Caller, row.Count)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("writing the census sink: %w", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("writing the census sink: %w", err)
	}
	return nil
}

// ReadCensusSink returns the rows WriteCensusSink wrote to path, failing on a
// line it cannot read rather than dropping that line's calls.
func ReadCensusSink(path string) ([]CensusRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []CensusRow
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		row, ok := parseCensusSinkRow(line)
		if !ok {
			// The row is a verb, a caller and a count, never captured output:
			// naming its position is enough to find it.
			return nil, fmt.Errorf("census sink %s: row %d is not verb@caller=count", path, i+1)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseCensusSinkRow reads one "verb@caller=count" line. The split takes the
// last @ and the last =, so a verb or caller that holds either character still
// parses, and the verb may hold a space ("mol wisp").
func parseCensusSinkRow(line string) (CensusRow, bool) {
	at := strings.LastIndex(line, "@")
	eq := strings.LastIndex(line, "=")
	if at < 1 || eq < at+2 {
		return CensusRow{}, false
	}
	count, err := strconv.Atoi(line[eq+1:])
	if err != nil || count < 0 {
		return CensusRow{}, false
	}
	return CensusRow{Verb: line[:at], Caller: line[at+1 : eq], Count: count}, true
}
