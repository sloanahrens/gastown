package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetricsSnapshotPrintsOneReceiptAndAppendsOneLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out bytes.Buffer
	if err := runMetricsSnapshot(context.Background(), &out, town); err != nil {
		t.Fatalf("runMetricsSnapshot: %v", err)
	}
	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Fatalf("receipt has %d lines, want 1:\n%s", got, out.String())
	}
	log := filepath.Join(town, ".runtime", "metrics", "snapshots.jsonl")
	if !strings.Contains(out.String(), log) || !strings.Contains(out.String(), "(8 groups, 0 errors)") {
		t.Fatalf("receipt = %q, want the log path and 8 groups, 0 errors", out.String())
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != 1 {
		t.Fatalf("log has %d lines, want 1:\n%s", got, data)
	}
}
