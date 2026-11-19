package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
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
// as its footer.json; an empty footer writes no footer file.
func writeExtractDay(t *testing.T, town, day, footer string) {
	t.Helper()
	dir := filepath.Join(town, ".runtime", "metrics", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if footer == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, metricsExtractFooterName), []byte(footer), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cleanFooter is a footer with no gaps, as the extract writes one.
const cleanFooter = `{"gaps":[]}`

// TestMetricsExtractAttentionStale: the newest day directory older than the
// stale window raises one item naming that day, and a day within the window
// raises none. The age is measured from the day's own midnight, so the bound
// is four rows: old, within, exactly at it, and a minute past it.
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
				metricsExtractFooterName,
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
