package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/metrics"
)

// fakeSource is a snapshot source the test controls: it never touches the host.
type fakeSource struct {
	group string
	cols  map[string]any
	err   error
}

func (f fakeSource) Group() string { return f.group }

func (f fakeSource) Collect(context.Context, metrics.Env) (map[string]any, error) {
	return f.cols, f.err
}

// The command test injects its sources: the real registry runs real commands
// (bd, docker, lsof) and reads the live town, so it never belongs in the unit tier.
func TestMetricsSnapshotPrintsOneReceiptAndAppendsOneLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := metrics.Env{Town: town, Now: func() time.Time { return time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC) }}
	sources := []metrics.Source{
		fakeSource{group: "alpha", cols: map[string]any{"n": 3}},
		fakeSource{group: "beta", err: errors.New("beta is unreadable")},
	}
	var out bytes.Buffer
	if err := runMetricsSnapshotWith(context.Background(), &out, env, sources); err != nil {
		t.Fatalf("runMetricsSnapshotWith: %v", err)
	}
	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Fatalf("receipt has %d lines, want 1:\n%s", got, out.String())
	}
	log := filepath.Join(town, ".runtime", "metrics", "snapshots.jsonl")
	if !strings.Contains(out.String(), log) || !strings.Contains(out.String(), "(2 groups, 1 errors)") {
		t.Fatalf("receipt = %q, want the log path and 2 groups, 1 errors", out.String())
	}
	if strings.Contains(out.String(), "no data collected") {
		t.Fatalf("receipt of a snapshot with a filled group says no data was collected: %q", out.String())
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != 1 {
		t.Fatalf("log has %d lines, want 1:\n%s", got, data)
	}
	if !strings.Contains(string(data), `"alpha"`) || !strings.Contains(string(data), "beta is unreadable") {
		t.Fatalf("log line lacks the alpha group or the beta error:\n%s", data)
	}
}

// The receipt says so when no placement is known, and counts the placement
// group when a source supplied one.
func TestMetricsSnapshotReceiptSaysWhetherAPlacementIsKnown(t *testing.T) {
	t.Parallel()
	env := func() metrics.Env {
		return metrics.Env{Town: t.TempDir(), Now: func() time.Time { return time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC) }}
	}
	var none bytes.Buffer
	if err := runMetricsSnapshotWith(context.Background(), &none, env(), []metrics.Source{fakeSource{group: "alpha", cols: map[string]any{"n": 1}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none.String(), "(1 groups, 0 errors); placement: none known\n") || strings.Count(none.String(), "\n") != 1 {
		t.Fatalf("receipt without a placement = %q", none.String())
	}
	var known bytes.Buffer
	sources := []metrics.Source{
		fakeSource{group: "alpha", cols: map[string]any{"n": 1}},
		fakeSource{group: metrics.PlacementGroup, cols: map[string]any{"dolt": "mac-host"}},
	}
	if err := runMetricsSnapshotWith(context.Background(), &known, env(), sources); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(known.String(), "(2 groups, 0 errors)\n") || strings.Contains(known.String(), "none known") {
		t.Fatalf("receipt with a placement = %q", known.String())
	}
}

// A collection that carries no columns — every source a stub's empty map, a
// source's nil, or an empty placement — must not print an unqualified success
// receipt (gt-pmovc).
func TestMetricsSnapshotReceiptSaysWhenNothingWasCollected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		sources []metrics.Source
		want    string
	}{
		{
			name: "stubs returning empty maps",
			sources: []metrics.Source{
				fakeSource{group: "alpha", cols: map[string]any{}},
				fakeSource{group: "beta", cols: map[string]any{}},
			},
			want: "(2 groups, 0 errors); no data collected; placement: none known\n",
		},
		{
			name: "sources returning nil",
			sources: []metrics.Source{
				fakeSource{group: "alpha"},
				fakeSource{group: "beta"},
			},
			want: "(2 groups, 0 errors); no data collected; placement: none known\n",
		},
		{
			name: "an empty placement carries nothing either",
			sources: []metrics.Source{
				fakeSource{group: "alpha", cols: map[string]any{}},
				fakeSource{group: metrics.PlacementGroup, cols: map[string]any{}},
			},
			want: "(2 groups, 0 errors); no data collected\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := metrics.Env{Town: t.TempDir(), Now: func() time.Time { return time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC) }}
			var out bytes.Buffer
			if err := runMetricsSnapshotWith(context.Background(), &out, env, tc.sources); err != nil {
				t.Fatalf("runMetricsSnapshotWith: %v", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("receipt = %q, want it to contain %q", out.String(), tc.want)
			}
		})
	}
}
