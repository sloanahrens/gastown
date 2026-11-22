package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/landworker"
)

// main-unknown (gt-8ceklp): a rig whose red-main check could not judge main
// for three runs in a row raises one attention item naming the count, the last
// cause and the commit. The state is the file the landing worker writes, read
// through the same path.

var mainUnknownNow = time.Date(2026, 11, 22, 5, 0, 0, 0, time.UTC)

const mainUnknownCommit = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"

// A streak of three UNKNOWN runs raises one item keyed by rig, with the count,
// the cause and the short sha in its summary.
func TestCollectMainUnknownRaisesAtThree(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, mainUnknownNow)
	writeJSONFile(t, RedMainStatePath(f.d.config.TownRoot, attentionRig), landworker.MainState{
		UnknownRuns:   3,
		UnknownCommit: mainUnknownCommit,
		UnknownCause:  "go: module lookup disabled",
	})
	c := collectorFor(t, f.src, attention.KindMainUnknown)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one main-unknown item", items)
	}
	it := items[0]
	if it.Key != "main-unknown:gastown" || it.Kind != attention.KindMainUnknown ||
		it.Severity != attention.SeverityHigh || it.Rig != attentionRig || it.SHA != mainUnknownCommit {
		t.Errorf("item = %+v, want a high main-unknown item on gastown at %s", it, mainUnknownCommit)
	}
	want := "main UNKNOWN for 3 runs at 1a2b3c4d: go: module lookup disabled; the rerun could not run"
	if it.Summary != want {
		t.Errorf("summary = %q, want %q", it.Summary, want)
	}
}

// A streak with no recorded cause still names the count and the commit, and
// does not print an empty cause between the colons.
func TestMainUnknownSummaryWithoutCause(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, mainUnknownNow)
	writeJSONFile(t, RedMainStatePath(f.d.config.TownRoot, attentionRig), landworker.MainState{
		UnknownRuns:   4,
		UnknownCommit: mainUnknownCommit,
	})
	c := collectorFor(t, f.src, attention.KindMainUnknown)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one main-unknown item", items)
	}
	want := "main UNKNOWN for 4 runs at 1a2b3c4d: no cause recorded; the rerun could not run"
	if items[0].Summary != want {
		t.Errorf("summary = %q, want %q", items[0].Summary, want)
	}
}

// No streak, or a streak cleared back to zero, raises nothing: the item clears.
func TestMainUnknownNoItemWithoutAStreak(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, mainUnknownNow)
	writeJSONFile(t, RedMainStatePath(f.d.config.TownRoot, attentionRig), landworker.MainState{
		LastGreen: mainUnknownCommit,
		LastRun:   mainUnknownCommit,
	})
	c := collectorFor(t, f.src, attention.KindMainUnknown)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none for a rig with no UNKNOWN streak", items)
	}
}

// An unreadable main state is a collector failure, not a clean rig: the
// collector returns the error so the queue keeps the previous tick's items.
func TestMainUnknownUnreadableStateIsAnError(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, mainUnknownNow)
	path := RedMainStatePath(f.d.config.TownRoot, attentionRig)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := collectorFor(t, f.src, attention.KindMainUnknown)

	items, err := c.collect(context.Background())
	if err == nil {
		t.Fatalf("collect() items = %+v, nil error; want the unreadable state refused", items)
	}
	if len(items) != 0 {
		t.Errorf("items = %+v on a failed read, want none", items)
	}
}

// gt-m0ohe7 pins gt-8ceklp's collector below the streak: two UNKNOWN runs raise
// nothing, and a previous item clears when the landing worker's count is back at
// zero, because the collector rebuilds the queue from the state each tick rather
// than keeping what it raised before.
func TestCollectMainUnknownBelowThresholdAndClearing(t *testing.T) {
	t.Parallel()
	prevItem := attention.Item{
		Key: "main-unknown:" + attentionRig, Kind: attention.KindMainUnknown,
		Severity: attention.SeverityHigh, Rig: attentionRig, SHA: mainUnknownCommit,
		Summary: "main UNKNOWN for 3 runs at 1a2b3c4d: go: module lookup disabled; the rerun could not run",
	}
	cases := []struct {
		name  string
		state landworker.MainState
		prev  []attention.Item
	}{
		{
			name: "two UNKNOWN runs raise nothing",
			state: landworker.MainState{
				UnknownRuns:   2,
				UnknownCommit: mainUnknownCommit,
				UnknownCause:  "go: module lookup disabled",
			},
		},
		{
			name: "a count back at zero clears the item",
			state: landworker.MainState{
				LastGreen: mainUnknownCommit,
				LastRun:   mainUnknownCommit,
			},
			prev: []attention.Item{prevItem},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAttentionFixture(t, mainUnknownNow)
			writeJSONFile(t, RedMainStatePath(f.d.config.TownRoot, attentionRig), tc.state)
			prev := attention.State{Updated: mainUnknownNow, Items: tc.prev}
			collectors := []attentionCollector{collectorFor(t, f.src, attention.KindMainUnknown)}

			observed := collectAttention(context.Background(), prev, collectors, func(string, ...any) {})
			if len(observed) != 0 {
				t.Errorf("observed = %+v, want no main-unknown item", observed)
			}
		})
	}
}

// gt-gcp5sp: through the queue's own collectAttention, an unreadable main state
// keeps the previous tick's main-unknown item, and a missing state file raises
// nothing, so the item clears.
func TestCollectMainUnknownStateErrors(t *testing.T) {
	t.Parallel()
	prevItem := attention.Item{
		Key: "main-unknown:" + attentionRig, Kind: attention.KindMainUnknown,
		Severity: attention.SeverityHigh, Rig: attentionRig, SHA: mainUnknownCommit,
		Summary: "main UNKNOWN for 3 runs at 1a2b3c4d: go: module lookup disabled; the rerun could not run",
	}
	cases := []struct {
		name  string
		write func(t *testing.T, path string)
		want  []attention.Item
	}{
		{
			name: "unreadable state keeps the previous item",
			write: func(t *testing.T, path string) {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: []attention.Item{prevItem},
		},
		{
			name:  "missing state raises nothing",
			write: func(*testing.T, string) {},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAttentionFixture(t, mainUnknownNow)
			tc.write(t, RedMainStatePath(f.d.config.TownRoot, attentionRig))
			prev := attention.State{Updated: mainUnknownNow, Items: []attention.Item{prevItem}}
			collectors := []attentionCollector{collectorFor(t, f.src, attention.KindMainUnknown)}

			observed := collectAttention(context.Background(), prev, collectors, func(string, ...any) {})
			if !reflect.DeepEqual(observed, tc.want) {
				t.Errorf("observed = %+v, want %+v", observed, tc.want)
			}
		})
	}
}
