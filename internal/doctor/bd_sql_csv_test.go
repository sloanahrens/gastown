package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// installFakeBd writes a fake bd script to a temp dir and prepends it to PATH.
func installFakeBd(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd stub is shell-specific")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRunBdSQLCSVIgnoresStderrDiagnostics is the regression test for gt-m7t:
// bd emits non-fatal diagnostics (routing notices, config warnings, version
// skew messages) on stderr. Merging stderr into the CSV stream made the
// stuck-wisp query fail with "csv parse: record on line 2: wrong number of
// fields" — the notice became line 1 and the real header line 2.
func TestRunBdSQLCSVIgnoresStderrDiagnostics(t *testing.T) {
	installFakeBd(t, `#!/usr/bin/env bash
echo "Notice: shared-server mode is enabled but metadata pins embedded mode" >&2
echo "id,title,status,updated_at"
echo "gt-abc,Some title,in_progress,2026-01-01 00:00:00"
`)

	records, err := runBdSQLCSV(t.TempDir(), "SELECT 1")
	if err != nil {
		t.Fatalf("runBdSQLCSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2 (header + row): %v", len(records), records)
	}
	if records[1][0] != "gt-abc" {
		t.Fatalf("row id = %q, want gt-abc", records[1][0])
	}
}

// TestRunBdSQLCSVErrorIncludesStderr verifies command failures surface bd's
// stderr in the returned error for diagnosability.
func TestRunBdSQLCSVErrorIncludesStderr(t *testing.T) {
	installFakeBd(t, `#!/usr/bin/env bash
echo "Error: no beads database found" >&2
exit 1
`)

	_, err := runBdSQLCSV(t.TempDir(), "SELECT 1")
	if err == nil {
		t.Fatal("runBdSQLCSV: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no beads database found") {
		t.Fatalf("error %q missing bd stderr content", err.Error())
	}
}

// TestCheckStuckWispsDoltIgnoresStderrDiagnostics exercises the exact failure
// reported in gt-m7t through the stuck-wisp check itself.
func TestCheckStuckWispsDoltIgnoresStderrDiagnostics(t *testing.T) {
	installFakeBd(t, `#!/usr/bin/env bash
echo "[routing] Preserved source dolt_database across redirect" >&2
echo "id,title,status,updated_at"
echo "om-old,Stale wisp,in_progress,2020-01-01 00:00:00"
`)

	c := NewPatrolNotStuckCheck()
	c.stuckThreshold = time.Hour
	stuck, err := c.checkStuckWispsDolt(t.TempDir(), "om")
	if err != nil {
		t.Fatalf("checkStuckWispsDolt: %v", err)
	}
	if len(stuck) != 1 || !strings.Contains(stuck[0], "om-old") {
		t.Fatalf("stuck = %v, want one entry for om-old", stuck)
	}
}
