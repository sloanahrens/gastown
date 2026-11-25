package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jobledger"
)

// TestTierSweepRecordsItsJobRun: a completed cycle's run reaches the job ledger
// as one tier_sweep run with its start, end, outcome ok and duration, and with
// no next due time, since a recorded next due is judged with no allowance.
func TestTierSweepRecordsItsJobRun(t *testing.T) {
	t.Parallel()

	t.Run("appends one run with its duration and no next due", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		var logs bytes.Buffer
		d := &Daemon{logger: log.New(&logs, "", 0), config: &Config{TownRoot: townRoot}}

		started := time.Date(2026, time.November, 25, 12, 0, 0, 0, time.UTC)
		ended := started.Add(17*time.Minute + 250*time.Millisecond)
		d.recordTierSweepJobRun(started, ended)

		runs := tierSweepJobRuns(t, townRoot)
		if len(runs) != 1 {
			t.Fatalf("the ledger holds %d run(s), want one: %+v", len(runs), runs)
		}
		got := runs[0]
		if got.Job != "tier_sweep" {
			t.Errorf("run job = %q, want %q", got.Job, "tier_sweep")
		}
		if !got.StartedAt.Equal(started) {
			t.Errorf("run started = %v, want %v", got.StartedAt, started)
		}
		if !got.EndedAt.Equal(ended) {
			t.Errorf("run ended = %v, want %v", got.EndedAt, ended)
		}
		if got.Outcome != "ok" {
			t.Errorf("run outcome = %q, want %q: a red sweep is a result, not a failed job", got.Outcome, "ok")
		}
		wantMS := ended.Sub(started).Milliseconds()
		if ms, ok := got.Stats["duration_ms"]; !ok || ms != wantMS {
			t.Errorf("run duration_ms = %d (present %v), want %d", ms, ok, wantMS)
		}
		if got.NextDue != nil {
			t.Errorf("run next due = %v, want none", got.NextDue)
		}
		if logs.Len() != 0 {
			t.Errorf("a recorded run logged:\n%s", logs.String())
		}
	})

	t.Run("a failing append is logged once and the call returns", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		// A file where the ledger's directory belongs makes every append fail, the
		// way a town mount that cannot be written to would.
		jobs := filepath.Join(townRoot, ".runtime", "jobs")
		if err := os.MkdirAll(filepath.Dir(jobs), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(jobs), err)
		}
		if err := os.WriteFile(jobs, []byte("not a directory\n"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", jobs, err)
		}
		var logs bytes.Buffer
		d := &Daemon{logger: log.New(&logs, "", 0), config: &Config{TownRoot: townRoot}}

		started := time.Date(2026, time.November, 25, 12, 0, 0, 0, time.UTC)
		d.recordTierSweepJobRun(started, started.Add(time.Minute))

		lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
		if len(lines) != 1 || !strings.Contains(lines[0], "tier_sweep") {
			t.Errorf("a failing append logged %d line(s), want one naming tier_sweep:\n%s", len(lines), logs.String())
		}
	})
}

// tierSweepJobRuns reads the tier_sweep runs the town's job ledger holds.
func tierSweepJobRuns(t *testing.T, townRoot string) []jobledger.Run {
	t.Helper()
	ledger, err := jobledger.NewLedger(townRoot)
	if err != nil {
		t.Fatalf("jobledger.NewLedger: %v", err)
	}
	runs, err := ledger.Tail("tier_sweep", 10)
	if err != nil {
		t.Fatalf("ledger.Tail(tier_sweep): %v", err)
	}
	return runs
}
