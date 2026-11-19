package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/metrics"
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
)

// The metrics extract items' keys. gt-mi4cem fixes them beside the rule, and
// each is a different condition, so each keeps its own first_seen and ack.
const (
	metricsExtractStaleKey      = "metrics-extract:stale"
	metricsExtractGapsKey       = "metrics-extract:gaps"
	metricsExtractUnreadableKey = "metrics-extract:unreadable"
)

// metricsExtractRead is one read of the town's metrics extract: the newest
// day and that day's footer gaps. A read with no error carries both; one that
// failed carries the day when the listing found it.
type metricsExtractRead struct {
	// day is midnight of the newest day directory, in the clock's location; the
	// zero time when no day could be found.
	day time.Time
	// gaps is the newest day's footer's gaps, each a (source, reason) pair.
	gaps [][]string
}

// collectMetricsExtract raises the metrics extract items: the newest day and
// its footer, read from the town's .runtime/metrics. A newest day older than
// metricsExtractStaleAfter raises the stale item naming the day; a newest
// footer with any gaps raises the gaps item naming the count and the first gap;
// a directory that cannot be read raises the unreadable item, never nothing.
// An unreadable extract that still names its day raises the stale item beside
// the unreadable one: an old extract is old whether or not its footer reads.
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
// canonical day directory under .runtime/metrics and that day's footer gaps. A
// read that fails still carries the day when the listing found one, so the
// stale item is raised beside the unreadable one.
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
	path := filepath.Join(root, day.Format(time.DateOnly), metricsExtractFooterName)
	data, err := os.ReadFile(path)
	if err != nil {
		return read, fmt.Errorf("reading %s: %w", path, err)
	}
	var footer metrics.Footer
	if err := json.Unmarshal(data, &footer); err != nil {
		return read, fmt.Errorf("%s: %w", path, err)
	}
	read.gaps = footer.Gaps
	return read, nil
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
