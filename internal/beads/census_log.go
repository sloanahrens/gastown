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

// censusLogMarker opens each census record in daemon.log. A reader that
// windows the log looks for it, so it is the one place the record's shape is
// spelled for readers as well as writers.
const censusLogMarker = "bd census:"

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
// the process that started this one to log (gt-139dp). The rows are written to
// a temp file beside path and renamed over it, so a reader racing the writer
// reads one whole snapshot or the other, never a line torn in half.
func WriteCensusSink(path string, rows []CensusRow) error {
	var b strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&b, "%s@%s=%d\n", row.Verb, row.Caller, row.Count)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("writing the census sink: %w", err)
	}
	// One writer per sink, so the temp name is fixed: a writer killed mid-write
	// leaves the temp file, never a half-written census, and the next tick's
	// write replaces it.
	tmpPath := path + ".tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("writing the census sink: %w", err)
	}
	_, err = tmp.WriteString(b.String())
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpPath, path)
	}
	if err != nil {
		os.Remove(tmpPath)
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

// censusMaxWindowSeconds is the longest window a census record may claim: the
// daemon logs a record a minute, so anything above an hour is a line the
// writer never produced.
const censusMaxWindowSeconds = 3600

// CensusWindow is one census record read back from the log: the calls the
// daemon counted in the window, how long that window ran, and the rows the
// record lists (its busiest censusLogRows, not every caller).
type CensusWindow struct {
	Total   int
	Seconds int
	Rows    []CensusRow
}

// IsCensusLine reports whether a log line's text is a census record by its
// marker, whether or not it parses: a record that carries the marker and does
// not parse is torn, and a reader counts it instead of passing over it.
func IsCensusLine(text string) bool {
	return strings.Contains(text, censusLogMarker)
}

// ParseCensusLine reads the census record in one daemon log line and reports
// false for a line that holds none, or holds a truncated or inconsistent one.
// The record's own fields are checked against the rows it lists: the rows are
// a sample of the window's calls, so a row sum above the total means the line
// is torn, and its rates would be built on a number the daemon never wrote.
func ParseCensusLine(line string) (CensusWindow, bool) {
	_, rest, found := strings.Cut(line, censusLogMarker)
	if !found {
		return CensusWindow{}, false
	}
	fields := strings.Fields(rest)
	if len(fields) < 3 || fields[2] != "top:" {
		return CensusWindow{}, false
	}
	total, ok := censusIntField(fields[0], "total=")
	if !ok {
		return CensusWindow{}, false
	}
	seconds, ok := censusWindowSeconds(fields[1])
	if !ok || seconds > censusMaxWindowSeconds {
		return CensusWindow{}, false
	}

	window := CensusWindow{Total: total, Seconds: seconds, Rows: make([]CensusRow, 0, len(fields)-3)}
	for i := 3; i < len(fields); i++ {
		token := fields[i]
		// The record joins its rows with spaces and a verb can hold one
		// ("comments add"), so a token with no separator is the first half of
		// a two-word verb and belongs to the token after it.
		if !strings.Contains(token, "@") && i+1 < len(fields) {
			i++
			token += " " + fields[i]
		}
		row, ok := censusRow(token)
		if !ok {
			return CensusWindow{}, false
		}
		window.Rows = append(window.Rows, row)
	}
	if CensusRowsTotal(window.Rows) > window.Total {
		return CensusWindow{}, false
	}
	return window, true
}

func censusIntField(field, name string) (int, bool) {
	value, ok := strings.CutPrefix(field, name)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func censusWindowSeconds(field string) (int, bool) {
	value, ok := strings.CutPrefix(field, "window=")
	if !ok {
		return 0, false
	}
	value, ok = strings.CutSuffix(value, "s")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func censusRow(token string) (CensusRow, bool) {
	at, eq := strings.LastIndexByte(token, '@'), strings.LastIndexByte(token, '=')
	if at <= 0 || eq <= at+1 {
		return CensusRow{}, false
	}
	count, err := strconv.Atoi(token[eq+1:])
	if err != nil || count < 0 {
		return CensusRow{}, false
	}
	return CensusRow{Verb: token[:at], Caller: token[at+1 : eq], Count: count}, true
}
