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
