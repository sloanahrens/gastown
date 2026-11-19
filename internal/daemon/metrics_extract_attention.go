package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
)

const (
	// metricsExtractStaleAfter is how old the newest extract's day may be before
	// the queue says the daily run has stopped. The extract writes one day's
	// directory per day, named for its date, so the age is measured from that
	// date's midnight: 26 hours lets the day's run arrive late the next morning
	// and raises the day once 02:00 has passed with no newer one.
	metricsExtractStaleAfter = 26 * time.Hour

	// metricsExtractFooterName is the file the extract writes last into a day's
	// directory, recording the rows it refused. The metrics package names it
	// metrics.footerName (internal/metrics/load_result.go) and writes it with
	// writeFooter (internal/metrics/extract.go).
	metricsExtractFooterName = "footer.json"

	// metricsExtractLandingsName is the day's landings table, the file the
	// extract writes first. The metrics package names it landingsName
	// (internal/metrics/analysis.go).
	metricsExtractLandingsName = "landings.csv"

	// metricsExtractEstimateColumn is the landings.csv column the extract fills
	// from a bead's sp-minutes label (internal/metrics/landings.go).
	metricsExtractEstimateColumn = "sp_minutes"

	// metricsExtractEstimateFloor is how many estimates the estimate log must
	// hold since a day's start before that day's empty estimate column is a
	// defect: a few estimates are a quiet day, not a join that lost its cells.
	metricsExtractEstimateFloor = 20
)

// The metrics extract items' keys. gt-mi4cem fixes them beside the rule, and
// each is a different condition, so each keeps its own first_seen and ack.
const (
	metricsExtractStaleKey       = "metrics-extract:stale"
	metricsExtractGapsKey        = "metrics-extract:gaps"
	metricsExtractUnreadableKey  = "metrics-extract:unreadable"
	metricsExtractNoEstimatesKey = "metrics-extract:no-estimates"
)

// metricsExtractRead is one read of the town's metrics extract: the newest
// day, that day's footer gaps and its landings table's estimate column, and the
// estimates the log holds since that day began. A read with no error carries
// all of them; one that failed carries what it had read when the failure came,
// so the stale item can be raised beside the unreadable one.
type metricsExtractRead struct {
	// day is midnight of the newest day directory, in the clock's location; the
	// zero time when no day could be found.
	day time.Time
	// gaps is the newest day's footer's gaps, each a (source, reason) pair.
	gaps [][]string
	// landingsRows is the row count of the newest day's landings.csv, for the
	// no-estimates item's counts.
	landingsRows int
	// estimateColumnEmpty is true when no row of the newest day's landings.csv
	// carries an sp_minutes value: every cell is empty, or the header has no
	// such column.
	estimateColumnEmpty bool
	// estimates is how many records the estimate log holds whose time is on or
	// after day.
	estimates int
	// estimateLogLines is how many non-blank lines the scan placed in day's
	// window, and estimateLogUndecodable how many of them did not decode into a
	// record with a time. A line that does not decode names no day, so it is
	// placed by position: it is in the window when it falls after the last
	// record before the day's start.
	estimateLogLines       int
	estimateLogUndecodable int
}

// collectMetricsExtract raises the metrics extract items: the newest day, its
// footer and its landings' estimate column, read from the town's
// .runtime/metrics and its estimate log. A newest day older than
// metricsExtractStaleAfter raises the stale item naming the day; a newest
// footer with any gaps raises the gaps item naming the count and the first gap;
// a newest day whose rows all lack sp_minutes while the log holds at least
// metricsExtractEstimateFloor estimates since that day raises the no-estimates
// item naming both counts, and rows that all lack it beside a log with lines in
// that day's window that do not decode raise that same item with the line
// counts; a directory or log that cannot be read raises the unreadable item,
// never nothing. An unreadable extract that still names its day raises the
// stale item beside the unreadable one: an old extract is old whether or not
// its footer reads.
func (s *attentionSources) collectMetricsExtract(context.Context) ([]attention.Item, error) {
	// The production sources always carry the read (attentionSources, pinned by
	// TestMetricsExtractAttentionIsWiredAndRegistered). A hand-built sources
	// value without it raises nothing: the shared test fixture has no metrics
	// read, and an item raised for it would change every tick test built on it.
	if s.metricsExtract == nil {
		return nil, nil
	}
	read, err := s.metricsExtract(s.now)
	var items []attention.Item
	if !read.day.IsZero() {
		if age := s.now.Sub(read.day); age > metricsExtractStaleAfter {
			items = append(items, attention.Item{
				Key:      metricsExtractStaleKey,
				Kind:     attention.KindMetricsExtract,
				Severity: attention.SeverityMedium,
				Summary: fmt.Sprintf(
					"metrics extract: the newest day %s is %s old, past %s; the daily extract has not run since",
					read.day.Format(time.DateOnly), parkedAge(age), metricsExtractStaleAfter),
			})
		}
	}
	switch {
	case err != nil:
		items = append(items, attention.Item{
			Key:      metricsExtractUnreadableKey,
			Kind:     attention.KindMetricsExtract,
			Severity: attention.SeverityMedium,
			Summary:  "metrics extract cannot be read: " + err.Error(),
		})
	case len(read.gaps) > 0:
		items = append(items, attention.Item{
			Key:      metricsExtractGapsKey,
			Kind:     attention.KindMetricsExtract,
			Severity: attention.SeverityMedium,
			Summary: fmt.Sprintf("metrics extract: the %s footer carries %d %s; first: %s",
				read.day.Format(time.DateOnly), len(read.gaps), extractGapWord(len(read.gaps)),
				strings.Join(read.gaps[0], ": ")),
		})
	}
	// An empty column with no estimates behind it is a quiet day, not a defect:
	// only the log can say the join had cells to fill (gt-4f1ocn). A log with
	// lines in that day's window that do not decode is not a quiet day either:
	// it raises the same item, naming the unreadable lines instead of counting
	// them as zero. That branch wins over the floor, so twenty good records
	// beside one torn line is unreadable, not twenty.
	if err == nil && read.estimateColumnEmpty {
		day := read.day.Format(time.DateOnly)
		emptyColumn := fmt.Sprintf("the %s landings table carries no %s in any of its %d rows",
			day, metricsExtractEstimateColumn, read.landingsRows)
		switch {
		case read.estimateLogUndecodable > 0:
			items = append(items, attention.Item{
				Key:      metricsExtractNoEstimatesKey,
				Kind:     attention.KindMetricsExtract,
				Severity: attention.SeverityMedium,
				Summary: fmt.Sprintf("%s and estimate log unreadable (%d of %d lines do not decode)",
					emptyColumn, read.estimateLogUndecodable, read.estimateLogLines),
			})
		case read.estimates >= metricsExtractEstimateFloor:
			items = append(items, attention.Item{
				Key:      metricsExtractNoEstimatesKey,
				Kind:     attention.KindMetricsExtract,
				Severity: attention.SeverityMedium,
				Summary: fmt.Sprintf("%s although %d estimates were logged since that day",
					emptyColumn, read.estimates),
			})
		}
	}
	return items, nil
}

// extractGapWord is "gap" for one gap and "gaps" for any other count, so the
// summary reads as one sentence for the single-gap case too.
func extractGapWord(n int) string {
	if n == 1 {
		return "gap"
	}
	return "gaps"
}

// readMetricsExtract reads townRoot's metrics extract at now: the newest
// canonical day directory under .runtime/metrics, that day's footer gaps, its
// landings' estimate column, and the estimates townRoot's log holds in the
// day's window, beside how many lines of that window did not decode. A read
// that fails still carries the day when the listing found one, so the stale
// item is raised beside the unreadable one.
func readMetricsExtract(townRoot string, now time.Time) (metricsExtractRead, error) {
	root := filepath.Join(townRoot, ".runtime", "metrics")
	entries, err := os.ReadDir(root)
	if err != nil {
		return metricsExtractRead{}, fmt.Errorf("listing %s: %w", root, err)
	}
	day, ok := newestExtractDay(entries, now.Location())
	if !ok {
		return metricsExtractRead{}, fmt.Errorf("%s holds no day directory", root)
	}
	read := metricsExtractRead{day: day}
	dayDir := filepath.Join(root, day.Format(time.DateOnly))
	path := filepath.Join(dayDir, metricsExtractFooterName)
	data, err := os.ReadFile(path)
	if err != nil {
		return read, fmt.Errorf("reading %s: %w", path, err)
	}
	var footer metrics.Footer
	if err := json.Unmarshal(data, &footer); err != nil {
		return read, fmt.Errorf("%s: %w", path, err)
	}
	read.gaps = footer.Gaps
	landings, err := readExtractLandings(filepath.Join(dayDir, metricsExtractLandingsName))
	if err != nil {
		return read, err
	}
	read.landingsRows = landings.rows
	read.estimateColumnEmpty = landings.estimateColumnEmpty
	counted, err := countExtractEstimates(townRoot, day)
	if err != nil {
		return read, err
	}
	read.estimates = counted.estimates
	read.estimateLogLines = counted.lines
	read.estimateLogUndecodable = counted.undecodable
	return read, nil
}

// extractLandings is a day's landings table as the estimate check reads it: how
// many rows it holds and whether any row carries an estimate.
type extractLandings struct {
	rows                int
	estimateColumnEmpty bool
}

// readExtractLandings reads the landings table at path. The column is empty
// when no row carries a non-blank cell in it; a header without the column is
// that same state, since a Row lacking the key answers "" for it. A cell that
// is not a number is a value, and the report that parses it names the bad cell.
func readExtractLandings(path string) (extractLandings, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path is under the town root
	if err != nil {
		return extractLandings{}, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only: a close error cannot change the table
	_, rows, err := metrics.ReadLandings(f)
	if err != nil {
		return extractLandings{}, fmt.Errorf("reading %s: %w", path, err)
	}
	empty := true
	for _, row := range rows {
		if strings.TrimSpace(row[metricsExtractEstimateColumn]) != "" {
			empty = false
			break
		}
	}
	return extractLandings{rows: len(rows), estimateColumnEmpty: empty}, nil
}

// metricsExtractEstimateLineMax is the longest estimate-log line the scan
// keeps. One record is a line of features and an embedding, well under this
// ceiling; a longer line is a corrupt record, and it is counted as one rather
// than failing the whole read.
const metricsExtractEstimateLineMax = 1 << 20

// extractEstimateCount is what one scan of the estimate log found in day's
// window: how many records fall on or after the day, how many non-blank lines
// the window holds, and how many of those lines did not decode into a record
// with a time.
type extractEstimateCount struct {
	estimates   int
	lines       int
	undecodable int
}

// countExtractEstimates counts the records of townRoot's estimate log that fall
// in day's window. Each line is decoded for its time alone, so the embedding
// vector a record carries is skipped and never held. A line that does not
// decode, or carries no time to place it, is placed by position rather than by
// its time: the log is append-only in time order, so that line is in the window
// when it falls after the last record before the day's start, and every such
// line is in the window when no record before the day's start exists. A torn
// line in the window must not read as a quiet day; one from an earlier day says
// nothing about this one. A log that does not exist yet holds no estimates; any
// other failure to open or read it is an error, never an empty log.
func countExtractEstimates(townRoot string, day time.Time) (extractEstimateCount, error) {
	path := estlog.Path(townRoot)
	f, err := os.Open(path) //nolint:gosec // G304: the path is under the town root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return extractEstimateCount{}, nil
		}
		return extractEstimateCount{}, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only: a close error cannot change the count
	var count extractEstimateCount
	// pending is how many lines since the last record before the day failed to
	// decode. That record is the window's start, so each one resets the count:
	// a torn line before it belongs to an earlier day, one after it is today's.
	pending := 0
	r := bufio.NewReader(f)
	for {
		line, overlong, err := readEstimateLogLine(r, metricsExtractEstimateLineMax)
		if err != nil && !errors.Is(err, io.EOF) {
			return extractEstimateCount{}, fmt.Errorf("reading %s: %w", path, err)
		}
		text := strings.TrimSpace(string(line))
		if text != "" || overlong {
			var rec struct {
				Time time.Time `json:"time"`
			}
			switch {
			case overlong, json.Unmarshal([]byte(text), &rec) != nil, rec.Time.IsZero():
				pending++
			case rec.Time.Before(day):
				pending = 0
			default:
				count.estimates++
			}
		}
		if err != nil {
			break
		}
	}
	count.undecodable = pending
	count.lines = count.estimates + pending
	return count, nil
}

// readEstimateLogLine reads one line from r, keeping at most max bytes of it. A
// longer line reports overlong and its remaining fragments are discarded, so
// the scan goes on to the line after it and no buffer grows past max.
func readEstimateLogLine(r *bufio.Reader, max int) (line []byte, overlong bool, err error) {
	for {
		frag, isPrefix, readErr := r.ReadLine()
		if room := max - len(line); room > 0 {
			if len(frag) > room {
				line = append(line, frag[:room]...)
				overlong = true
			} else {
				line = append(line, frag...)
			}
		} else if len(frag) > 0 {
			overlong = true
		}
		if readErr != nil {
			return line, overlong, readErr
		}
		if !isPrefix {
			return line, overlong, nil
		}
	}
}

// newestExtractDay is the newest canonical YYYY-MM-DD directory among entries,
// as midnight of that day in loc. Only a canonical day is an extract's day: the
// metrics root's cache and latest entries are not. ok is false when entries
// holds no day.
func newestExtractDay(entries []os.DirEntry, loc *time.Location) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		day, err := time.ParseInLocation(time.DateOnly, entry.Name(), loc)
		if err != nil {
			continue
		}
		if !found || day.After(newest) {
			newest, found = day, true
		}
	}
	return newest, found
}
