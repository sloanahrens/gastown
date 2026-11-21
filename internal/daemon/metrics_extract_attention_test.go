package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
)

// metricsExtractTick is the time the extract cases tick at.
var metricsExtractTick = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// metricsExtractRig is the fixture with the production read over its temp
// town, so the cases drive readMetricsExtract and not a stand-in for it.
func metricsExtractRig(t *testing.T, now time.Time) *attentionFixture {
	t.Helper()
	f := newAttentionFixture(t, now)
	f.src.metricsExtract = func(at time.Time) (metricsExtractRead, error) {
		return readMetricsExtract(f.d.config.TownRoot, at)
	}
	return f
}

// writeExtractDay writes day's directory under town's metrics root with footer
// as its footer.json and a healthy landings.csv; an empty footer writes no
// footer file. The footer's mtime is the day's own midnight, so a case that
// measures the extract's age does not read the host clock; a case that needs
// another write time sets it with os.Chtimes after this.
func writeExtractDay(t *testing.T, town, day, footer string) {
	t.Helper()
	dir := filepath.Join(town, ".runtime", "metrics", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExtractLandings(t, town, day, healthyLandings)
	if footer == "" {
		return
	}
	stamp, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatalf("day %q: %v", day, err)
	}
	path := filepath.Join(dir, metrics.FooterName)
	if err := os.WriteFile(path, []byte(footer), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// writeExtractLandings overwrites day's landings.csv with content.
func writeExtractLandings(t *testing.T, town, day, content string) {
	t.Helper()
	path := filepath.Join(town, ".runtime", "metrics", day, metricsExtractLandingsName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// paddedEstimateRecord is one estimate record padded with trailing spaces to
// size bytes. JSON ignores the spaces, so the record decodes until it is padded
// past the scan's ceiling, where the ceiling alone makes it unreadable.
func paddedEstimateRecord(at time.Time, size int) string {
	rec := fmt.Sprintf(`{"bead_id":"pad","time":%q}`, at.Format(time.RFC3339Nano))
	return rec + strings.Repeat(" ", size-len(rec))
}

// estimateLogText is one estimate record line per time, as the estimator
// appends them: a bead id and the time, which is all the count decodes.
func estimateLogText(times []time.Time) string {
	var log strings.Builder
	for i, at := range times {
		fmt.Fprintf(&log, `{"bead_id":"b%d","time":%q}`+"\n", i, at.Format(time.RFC3339Nano))
	}
	return log.String()
}

// writeEstimateLog writes one estimate record per time to town's estimate log.
func writeEstimateLog(t *testing.T, town string, times []time.Time) {
	t.Helper()
	writeEstimateLogText(t, town, estimateLogText(times))
}

// writeEstimateLogText writes content verbatim to town's estimate log, so a
// case can plant a torn line or one past the scan's ceiling.
func writeEstimateLogText(t *testing.T, town, content string) {
	t.Helper()
	path := estlog.Path(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// atMinutes returns count times, each one minute after the first, starting at
// start.
func atMinutes(start time.Time, count int) []time.Time {
	times := make([]time.Time, count)
	for i := range times {
		times[i] = start.Add(time.Duration(i) * time.Minute)
	}
	return times
}

// cleanFooter is a footer with no gaps, as the extract writes one.
const cleanFooter = `{"gaps":[]}`

// healthyLandings is a day's landings table with an estimate filled in, the
// shape a working extract writes.
const healthyLandings = "bead,rig,sp_minutes\nb1,gastown,8.0\n"

// TestMetricsExtractAttentionStale: the newest day directory older than the
// stale window raises one item naming that day, and a day within the window
// raises none. writeExtractDay stamps each footer at its day's own midnight, so
// the bound is four rows: old, within, exactly at it, and a minute past it.
func TestMetricsExtractAttentionStale(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		now  time.Time
		days []string
		want string // the day the stale item names, or "" for no item
	}{
		{
			name: "a newest day two days old",
			now:  metricsExtractTick,
			days: []string{"2026-11-15", "2026-11-17"},
			want: "2026-11-17",
		},
		{
			name: "a newest day within the window",
			now:  metricsExtractTick,
			days: []string{"2026-11-19"},
		},
		{
			name: "a newest day exactly twenty-six hours old",
			now:  time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC),
			days: []string{"2026-11-18"},
		},
		{
			name: "a newest day a minute past the window",
			now:  time.Date(2026, 11, 19, 2, 1, 0, 0, time.UTC),
			days: []string{"2026-11-18"},
			want: "2026-11-18",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := metricsExtractRig(t, tc.now)
			for _, day := range tc.days {
				writeExtractDay(t, f.d.config.TownRoot, day, cleanFooter)
			}
			items := f.collect(t, f.src.collectMetricsExtract)
			if tc.want == "" {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != metricsExtractStaleKey {
				t.Fatalf("items = %+v, want the one stale item", items)
			}
			if items[0].Kind != attention.KindMetricsExtract {
				t.Errorf("kind = %q, want %q", items[0].Kind, attention.KindMetricsExtract)
			}
			if !strings.Contains(items[0].Summary, tc.want) {
				t.Errorf("summary = %q, want it to name the day %s", items[0].Summary, tc.want)
			}
		})
	}
}

// TestMetricsExtractStaleIsMeasuredFromTheWrite: the stale item ages the newest
// extract from its footer's write time, not its day's midnight (gt-thzlac). A
// footer written 21:05 on the evening of its day is five hours old at 02:01 the
// next morning and raises nothing; at 23:30 the next night it is 26h25m old and
// raises the item naming that write age.
func TestMetricsExtractStaleIsMeasuredFromTheWrite(t *testing.T) {
	t.Parallel()
	const day = "2026-11-19"
	written := time.Date(2026, 11, 19, 21, 5, 0, 0, time.UTC)
	tests := []struct {
		name string
		now  time.Time
		want bool   // whether the stale item is raised
		age  string // the write age the summary must carry, when wanted
	}{
		{
			name: "five hours after the write",
			now:  time.Date(2026, 11, 20, 2, 1, 0, 0, time.UTC),
		},
		{
			name: "twenty-six hours and twenty-five minutes after the write",
			now:  time.Date(2026, 11, 20, 23, 30, 0, 0, time.UTC),
			want: true,
			age:  "26h25m",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := metricsExtractRig(t, tc.now)
			writeExtractDay(t, f.d.config.TownRoot, day, cleanFooter)
			path := filepath.Join(f.d.config.TownRoot, ".runtime", "metrics", day, metrics.FooterName)
			if err := os.Chtimes(path, written, written); err != nil {
				t.Fatal(err)
			}
			items := f.collect(t, f.src.collectMetricsExtract)
			if !tc.want {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != metricsExtractStaleKey {
				t.Fatalf("items = %+v, want the one stale item", items)
			}
			for _, want := range []string{day, tc.age} {
				if !strings.Contains(items[0].Summary, want) {
					t.Errorf("summary = %q, want it to carry %q", items[0].Summary, want)
				}
			}
		})
	}
}

// TestMetricsExtractStaleIgnoresAFutureWriteTime: a footer mtime after now is
// not a write time the stale rule can trust, so the age falls back to the day's
// midnight, the way an unreadable footer does (gt-qc3wj8). A footer stamped
// thirty days ahead of now does not hide a newest day five and a half days old;
// a footer written in the evening is still aged from its write, so reading it
// the next morning raises nothing.
func TestMetricsExtractStaleIgnoresAFutureWriteTime(t *testing.T) {
	t.Parallel()
	const day = "2026-11-19"
	tests := []struct {
		name    string
		now     time.Time
		written time.Time // the footer mtime, set with os.Chtimes
		want    bool      // whether the stale item is raised
		age     string    // the write age the summary must carry, when wanted
	}{
		{
			name:    "a footer stamped thirty days ahead of now",
			now:     time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
			written: time.Date(2026, 12, 24, 12, 0, 0, 0, time.UTC),
			want:    true,
			age:     "132h0m", // from the day's midnight, not from the future mtime
		},
		{
			name:    "a footer written the evening before",
			now:     time.Date(2026, 11, 20, 2, 1, 0, 0, time.UTC),
			written: time.Date(2026, 11, 19, 21, 5, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := metricsExtractRig(t, tc.now)
			writeExtractDay(t, f.d.config.TownRoot, day, cleanFooter)
			path := filepath.Join(f.d.config.TownRoot, ".runtime", "metrics", day, metrics.FooterName)
			if err := os.Chtimes(path, tc.written, tc.written); err != nil {
				t.Fatal(err)
			}
			items := f.collect(t, f.src.collectMetricsExtract)
			if !tc.want {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != metricsExtractStaleKey {
				t.Fatalf("items = %+v, want the one stale item", items)
			}
			for _, want := range []string{day, tc.age} {
				if !strings.Contains(items[0].Summary, want) {
					t.Errorf("summary = %q, want it to carry %q", items[0].Summary, want)
				}
			}
		})
	}
}

// TestMetricsExtractStaleReadsTheFooterWriteTime: the read carries the newest
// day's footer.json mtime as the extract's write time, and leaves it zero when
// the footer cannot be statted, so the age falls back to the day's midnight.
func TestMetricsExtractStaleReadsTheFooterWriteTime(t *testing.T) {
	t.Parallel()
	const day = "2026-11-19"
	written := time.Date(2026, 11, 19, 21, 5, 0, 0, time.UTC)
	tests := []struct {
		name      string
		footer    bool
		wantWrite time.Time // the write time the read must carry
		wantErr   bool
	}{
		{name: "a footer whose mtime is set", footer: true, wantWrite: written},
		{name: "a footer that cannot be statted leaves the write time zero", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			dir := filepath.Join(town, ".runtime", "metrics", day)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, metricsExtractLandingsName), []byte(healthyLandings), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.footer {
				path := filepath.Join(dir, metrics.FooterName)
				if err := os.WriteFile(path, []byte(cleanFooter), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, written, written); err != nil {
					t.Fatal(err)
				}
			}
			read, err := readMetricsExtract(town, metricsExtractTick)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if !read.written.Equal(tc.wantWrite) {
				t.Errorf("written = %v, want %v", read.written, tc.wantWrite)
			}
		})
	}
}

// TestMetricsExtractAttentionGaps: the newest footer's gaps raise one item
// naming the count and the first gap; a clean footer clears it, and a metrics
// directory that cannot be read raises the unreadable item, never nothing.
func TestMetricsExtractAttentionGaps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// days are written in order; a day with no footer passes "".
		days    [][2]string
		noRoot  bool
		want    []string // item keys
		summary []string // substrings the first wanted item's summary carries
	}{
		{
			name: "a footer with gaps",
			days: [][2]string{{"2026-11-19", `{"gaps":[["ledger","missing reviewer column"],["usage","no rows"]]}`}},
			want: []string{metricsExtractGapsKey},
			summary: []string{
				"2 gaps",
				"ledger: missing reviewer column",
			},
		},
		{
			name: "a clean footer",
			days: [][2]string{{"2026-11-19", cleanFooter}},
		},
		{
			name: "a clean newest day beside an older day with gaps",
			days: [][2]string{
				{"2026-11-18", `{"gaps":[["ledger","missing reviewer column"]]}`},
				{"2026-11-19", cleanFooter},
			},
		},
		{
			name:   "a missing metrics directory",
			noRoot: true,
			want:   []string{metricsExtractUnreadableKey},
		},
		{
			name: "a newest day with no footer",
			days: [][2]string{{"2026-11-19", ""}},
			want: []string{metricsExtractUnreadableKey},
		},
		{
			name: "a footer that does not decode",
			days: [][2]string{{"2026-11-19", "{not json"}},
			want: []string{metricsExtractUnreadableKey},
			summary: []string{
				metrics.FooterName,
			},
		},
		{
			name: "an old day whose footer cannot be read raises both",
			days: [][2]string{{"2026-11-17", ""}},
			want: []string{metricsExtractStaleKey, metricsExtractUnreadableKey},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := metricsExtractRig(t, metricsExtractTick)
			if !tc.noRoot {
				for _, day := range tc.days {
					writeExtractDay(t, f.d.config.TownRoot, day[0], day[1])
				}
			}
			items := f.collect(t, f.src.collectMetricsExtract)
			got := make([]string, 0, len(items))
			for _, it := range items {
				got = append(got, it.Key)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("keys = %v, want %v", got, tc.want)
			}
			if len(tc.summary) == 0 {
				return
			}
			for _, want := range tc.summary {
				if !strings.Contains(items[0].Summary, want) {
					t.Errorf("summary = %q, want it to carry %q", items[0].Summary, want)
				}
			}
		})
	}
}

// TestMetricsExtractAttentionUnreadableNamesTheRoot: the unreadable item names
// the directory the read walked, so the reader knows which town to look at.
func TestMetricsExtractAttentionUnreadableNamesTheRoot(t *testing.T) {
	t.Parallel()
	f := metricsExtractRig(t, metricsExtractTick)

	items := f.collect(t, f.src.collectMetricsExtract)
	if len(items) != 1 || items[0].Key != metricsExtractUnreadableKey {
		t.Fatalf("items = %+v, want the one unreadable item", items)
	}
	root := filepath.Join(f.d.config.TownRoot, ".runtime", "metrics")
	if !strings.Contains(items[0].Summary, root) {
		t.Errorf("summary = %q, want it to name %s", items[0].Summary, root)
	}
}

// TestMetricsExtractAttentionNoDayDirectory: a metrics root holding no
// canonical day directory — only the extract's cache beside its sibling files
// — is an extract that cannot be read, not an empty answer.
func TestMetricsExtractAttentionNoDayDirectory(t *testing.T) {
	t.Parallel()
	f := metricsExtractRig(t, metricsExtractTick)
	root := filepath.Join(f.d.config.TownRoot, ".runtime", "metrics")
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gate-runs.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	items := f.collect(t, f.src.collectMetricsExtract)
	if len(items) != 1 || items[0].Key != metricsExtractUnreadableKey {
		t.Fatalf("items = %+v, want the one unreadable item", items)
	}
	if !strings.Contains(items[0].Summary, root) {
		t.Errorf("summary = %q, want it to name %s", items[0].Summary, root)
	}
}

// TestMetricsExtractAttentionIsWiredAndRegistered: the production sources
// carry the read, the read walks the town's own metrics root, and the
// collector is one of the queue's.
func TestMetricsExtractAttentionIsWiredAndRegistered(t *testing.T) {
	t.Parallel()
	d, _, _ := patrolReloadTown(t)
	s := d.attentionSources(metricsExtractTick)
	if s.metricsExtract == nil {
		t.Fatal("the production sources carry no metrics extract read")
	}
	read, err := s.metricsExtract(metricsExtractTick)
	if err == nil {
		t.Fatalf("the read of an empty town answered %+v, want an error", read)
	}
	if root := filepath.Join(d.config.TownRoot, ".runtime", "metrics"); !strings.Contains(err.Error(), root) {
		t.Errorf("err = %q, want it to name %s", err, root)
	}
	c := collectorFor(t, s, attention.KindMetricsExtract)
	if c.collect == nil {
		t.Fatal("the extract collector is registered with no collect")
	}
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collector: %v", err)
	}
	if len(items) != 1 || items[0].Key != metricsExtractUnreadableKey {
		t.Fatalf("items = %+v, want the production sources to raise the unreadable item", items)
	}
}

// extractNoEstimatesDay is the day the estimate cases write, inside the stale
// window at metricsExtractTick so only the item under test is raised.
const extractNoEstimatesDay = "2026-11-19"

// emptyEstimateLandings is a landings table whose estimate cells are all empty.
const emptyEstimateLandings = "bead,rig,sp_minutes\nb1,gastown,\nb2,gastown,\n"

// TestMetricsExtractNoEstimatesRaises: a newest table with an empty estimate
// column and twenty-five estimates made since that day raises one item naming
// the day and both counts; the same estimates beside a filled cell raise none
// (gt-4f1ocn).
func TestMetricsExtractNoEstimatesRaises(t *testing.T) {
	t.Parallel()
	dayStart := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		landings string
		want     bool
	}{
		{name: "an empty estimate column beside 25 estimates that day", landings: emptyEstimateLandings, want: true},
		{name: "a filled estimate cell beside 25 estimates that day", landings: healthyLandings},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := metricsExtractRig(t, metricsExtractTick)
			writeExtractDay(t, f.d.config.TownRoot, extractNoEstimatesDay, cleanFooter)
			writeExtractLandings(t, f.d.config.TownRoot, extractNoEstimatesDay, tc.landings)
			writeEstimateLog(t, f.d.config.TownRoot, atMinutes(dayStart.Add(9*time.Hour), 25))

			items := f.collect(t, f.src.collectMetricsExtract)
			if !tc.want {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != metricsExtractNoEstimatesKey {
				t.Fatalf("items = %+v, want the one no-estimates item", items)
			}
			if items[0].Kind != attention.KindMetricsExtract {
				t.Errorf("kind = %q, want %q", items[0].Kind, attention.KindMetricsExtract)
			}
			for _, want := range []string{"extract columns empty although 25 estimates were logged today", extractNoEstimatesDay, metricsExtractEstimateColumn, "2 rows"} {
				if !strings.Contains(items[0].Summary, want) {
					t.Errorf("summary = %q, want it to carry %q", items[0].Summary, want)
				}
			}
		})
	}
}

// TestMetricsExtractNoEstimatesEdges: fewer estimates than the floor, estimates
// from before the day, a table with no estimate column, a missing log, a log
// that cannot be read and a landings table that cannot be read each behave as
// the rule states. A log whose lines in the day's window do not decode — every
// line, a torn line beside twenty good ones, a torn line after the window's
// start, or a line over the scan's ceiling — raises the same item with the
// unreadable reason and the two line counts, never a quiet day; a torn line
// before the window's start is an earlier day's and raises none. The scan goes
// on past an overlong line, and a line at the ceiling decodes.
func TestMetricsExtractNoEstimatesEdges(t *testing.T) {
	t.Parallel()
	dayStart := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	good25 := atMinutes(dayStart.Add(9*time.Hour), 25)
	oldBefore := atMinutes(dayStart.Add(-2*time.Hour), 5)
	tests := []struct {
		name     string
		landings string
		// unreadable selects how the estimate log is broken: "scan" is a
		// directory at the log path, whose open succeeds and read fails;
		// "open" is a file where the log's directory should be, whose open
		// fails with an error that is not ErrNotExist.
		noLandings    bool
		estimates     []time.Time
		rawLog        string // written verbatim in place of estimates
		unreadableLog string
		// unreadableNames is the file the unreadable item must name; the log
		// path when empty.
		unreadableNames string
		want            []string // item keys
		summary         []string // substrings the one wanted item's summary carries
	}{
		{
			name:      "nineteen estimates that day is under the floor",
			landings:  emptyEstimateLandings,
			estimates: atMinutes(dayStart.Add(9*time.Hour), 19),
		},
		{
			name:      "twenty-five estimates from the day before",
			landings:  emptyEstimateLandings,
			estimates: atMinutes(dayStart.Add(-time.Hour), 25),
		},
		{
			name:      "a table with no estimate column is the same state",
			landings:  "bead,rig\nb1,gastown\n",
			estimates: atMinutes(dayStart.Add(9*time.Hour), 25),
			want:      []string{metricsExtractNoEstimatesKey},
		},
		{
			name:     "a town with no estimate log yet",
			landings: emptyEstimateLandings,
		},
		{
			name:          "an estimate log whose read fails",
			landings:      emptyEstimateLandings,
			unreadableLog: "scan",
			want:          []string{metricsExtractUnreadableKey},
		},
		{
			name:          "an estimate log whose open fails",
			landings:      emptyEstimateLandings,
			unreadableLog: "open",
			want:          []string{metricsExtractUnreadableKey},
		},
		{
			name:       "a day whose landings table is missing",
			noLandings: true,
			want:       []string{metricsExtractUnreadableKey},

			unreadableNames: metricsExtractLandingsName,
		},
		{
			name:            "a landings table that does not parse",
			landings:        "bead,rig,sp_minutes\nb1,gastown\n",
			estimates:       atMinutes(dayStart.Add(9*time.Hour), 25),
			want:            []string{metricsExtractUnreadableKey},
			unreadableNames: metricsExtractLandingsName,
		},
		{
			name:     "a log whose every line is torn raises, naming them all",
			landings: emptyEstimateLandings,
			rawLog:   "not json\n{also not json\n",
			want:     []string{metricsExtractNoEstimatesKey},
			summary:  []string{"estimate log unreadable (2 of 2 lines do not decode)"},
		},
		{
			name:     "a torn line beside twenty good ones raises the unreadable reason",
			landings: emptyEstimateLandings,
			rawLog:   estimateLogText(atMinutes(dayStart.Add(9*time.Hour), 20)) + "torn\n",
			want:     []string{metricsExtractNoEstimatesKey},
			summary:  []string{"estimate log unreadable (1 of 21 lines do not decode)"},
		},
		{
			name:     "a torn line before the window start and no estimates today is quiet",
			landings: emptyEstimateLandings,
			rawLog:   "torn\n" + estimateLogText(oldBefore),
		},
		{
			name:     "a torn line after the window start raises, counting the window's one line",
			landings: emptyEstimateLandings,
			rawLog:   estimateLogText(oldBefore) + "torn\n",
			want:     []string{metricsExtractNoEstimatesKey},
			summary:  []string{"estimate log unreadable (1 of 1 lines do not decode)"},
		},
		{
			name:     "a line over the ceiling is unreadable even when its head decodes, and the scan goes on",
			landings: emptyEstimateLandings,
			rawLog: paddedEstimateRecord(dayStart.Add(10*time.Hour), metricsExtractEstimateLineMax+1) +
				"\n" + estimateLogText(good25),
			want:    []string{metricsExtractNoEstimatesKey},
			summary: []string{"estimate log unreadable (1 of 26 lines do not decode)"},
		},
		{
			name:     "a record padded to exactly the ceiling still decodes",
			landings: emptyEstimateLandings,
			rawLog:   estimateLogText(good25) + paddedEstimateRecord(dayStart.Add(10*time.Hour), metricsExtractEstimateLineMax),
			want:     []string{metricsExtractNoEstimatesKey},
			summary:  []string{"26 estimates"},
		},
		{
			name:     "a filled table beside a torn log raises none",
			landings: healthyLandings,
			rawLog:   "torn\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := metricsExtractRig(t, metricsExtractTick)
			writeExtractDay(t, f.d.config.TownRoot, extractNoEstimatesDay, cleanFooter)
			if tc.noLandings {
				path := filepath.Join(f.d.config.TownRoot, ".runtime", "metrics", extractNoEstimatesDay, metricsExtractLandingsName)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeExtractLandings(t, f.d.config.TownRoot, extractNoEstimatesDay, tc.landings)
			}
			switch tc.unreadableLog {
			case "scan":
				if err := os.MkdirAll(estlog.Path(f.d.config.TownRoot), 0o755); err != nil {
					t.Fatal(err)
				}
			case "open":
				dir := filepath.Dir(estlog.Path(f.d.config.TownRoot))
				if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dir, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			default:
				switch {
				case tc.rawLog != "":
					writeEstimateLogText(t, f.d.config.TownRoot, tc.rawLog)
				case len(tc.estimates) > 0:
					writeEstimateLog(t, f.d.config.TownRoot, tc.estimates)
				}
			}

			items := f.collect(t, f.src.collectMetricsExtract)
			got := make([]string, 0, len(items))
			for _, it := range items {
				got = append(got, it.Key)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("keys = %v, want %v", got, tc.want)
			}
			if len(tc.want) == 1 {
				for _, want := range tc.summary {
					if !strings.Contains(items[0].Summary, want) {
						t.Errorf("summary = %q, want it to carry %q", items[0].Summary, want)
					}
				}
			}
			if len(tc.want) != 1 || tc.want[0] != metricsExtractUnreadableKey {
				return
			}
			want := tc.unreadableNames
			if want == "" {
				want = estlog.Path(f.d.config.TownRoot)
			}
			if !strings.Contains(items[0].Summary, want) {
				t.Errorf("summary = %q, want it to name %s", items[0].Summary, want)
			}
		})
	}
}

// TestMetricsExtractBrokenLandingsKeepsTheGapsItem: a landings table that
// cannot be read raises the unreadable item beside the footer's gaps item, not
// instead of it.
func TestMetricsExtractBrokenLandingsKeepsTheGapsItem(t *testing.T) {
	t.Parallel()
	f := metricsExtractRig(t, metricsExtractTick)
	writeExtractDay(t, f.d.config.TownRoot, extractNoEstimatesDay, `{"gaps":[["ledger","no rows"]]}`)
	writeExtractLandings(t, f.d.config.TownRoot, extractNoEstimatesDay, "bead,rig,sp_minutes\nb1,gastown\n")

	items := f.collect(t, f.src.collectMetricsExtract)
	got := make([]string, 0, len(items))
	for _, it := range items {
		got = append(got, it.Key)
	}
	sort.Strings(got)
	want := []string{metricsExtractGapsKey, metricsExtractUnreadableKey}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

// TestMetricsExtractNoEstimatesNotRaisedOnAFailedRead: a read that failed
// raises only the unreadable item, even when it carries counts from before the
// failure: a failed read could not confirm them, so they are not evidence.
func TestMetricsExtractNoEstimatesNotRaisedOnAFailedRead(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, metricsExtractTick)
	f.src.metricsExtract = func(time.Time) (metricsExtractRead, error) {
		return metricsExtractRead{
			day:                 metricsExtractTick.Add(-24 * time.Hour),
			gaps:                [][]string{{"ledger", "unreadable diff"}},
			landingsRows:        3,
			estimateColumnEmpty: true,
			estimates:           25,
		}, errors.New("landings.csv: is a directory")
	}

	items := f.collect(t, f.src.collectMetricsExtract)
	if len(items) != 1 || items[0].Key != metricsExtractUnreadableKey {
		t.Fatalf("items = %+v, want only the unreadable item", items)
	}
}
