package doctor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunBdSQLCSVRunsInDir checks runBdSQLCSV asks the bd of the directory
// it was given. That bd's stdout-only CSV parsing (gt-m7t) is pinned in
// internal/beads (TestSQLCSVParsesStdoutOnly, TestSQLCSVErrorCarriesStderr).
func TestRunBdSQLCSVRunsInDir(t *testing.T) {
	t.Parallel()
	bd := newFakeBD()
	dir := t.TempDir()
	bd.db(dir).OnSQL(csvAnswer([]string{"id"}, []string{"gt-abc"}))
	records, err := runBdSQLCSV(bd.ctx(t.TempDir()), dir, "SELECT id FROM issues")
	if err != nil || len(records) != 2 || records[1][0] != "gt-abc" {
		t.Fatalf("runBdSQLCSV = %v, %v", records, err)
	}
	if got := bd.db(dir).SQLStatements(); len(got) != 1 || got[0] != "SELECT id FROM issues" {
		t.Errorf("statements = %q", got)
	}
	if opens := bd.opened(); len(opens) != 1 || opens[0].dir != filepath.Clean(dir) || opens[0].env != nil {
		t.Errorf("opens = %+v, want one in %s with the inherited environment", opens, dir)
	}
}

// TestCheckStuckWispsDoltReportsStaleWisp exercises the stuck-wisp query
// through the check itself (the gt-m7t symptom was this check failing).
func TestCheckStuckWispsDoltReportsStaleWisp(t *testing.T) {
	t.Parallel()
	bd := newFakeBD()
	rig := t.TempDir()
	bd.db(rig).OnSQL(csvAnswer(
		[]string{"id", "title", "status", "updated_at"},
		[]string{"om-old", "Stale wisp", "in_progress", "2020-01-01 00:00:00"},
	))
	c := NewPatrolNotStuckCheck()
	c.stuckThreshold = time.Hour
	stuck, err := c.checkStuckWispsDolt(bd.ctx(t.TempDir()), rig, "om")
	if err != nil {
		t.Fatalf("checkStuckWispsDolt: %v", err)
	}
	if len(stuck) != 1 || !strings.Contains(stuck[0], "om-old") {
		t.Fatalf("stuck = %v, want one entry for om-old", stuck)
	}
}
