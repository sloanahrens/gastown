package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
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

// TestTierSweepCycleRecordsItsJobRun drives a cycle through its own trigger: a
// cycle that reached a verdict records its run from the cycle's start to its
// completion, the same completion the patrol record carries, and a cycle that
// did not complete records nothing.
func TestTierSweepCycleRecordsItsJobRun(t *testing.T) {
	t.Parallel()

	// sweepDaemon is a daemon whose one rig can sweep: origin/main reads a fixed
	// sha and the checkout is a no-op, with the fake clock its cycle runs on.
	// The recorder answers RED until a row makes it green.
	sweepDaemon := func(t *testing.T, now time.Time) (*Daemon, *tierSweepRunRecorder, *clockwork.FakeClock, *bytes.Buffer) {
		t.Helper()
		d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
		d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
		d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
		return d, rec, d.clock.(*clockwork.FakeClock), logs
	}

	// unwritableJobLedger makes every append to the town's job ledger fail, the
	// way a town mount that cannot be written to would.
	unwritableJobLedger := func(t *testing.T, townRoot string) {
		t.Helper()
		jobs := filepath.Join(townRoot, ".runtime", "jobs")
		if err := os.MkdirAll(filepath.Dir(jobs), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(jobs), err)
		}
		if err := os.WriteFile(jobs, []byte("not a directory\n"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", jobs, err)
		}
	}

	t.Run("a completed cycle records one run from the cycle's start to the patrol record's time", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, rec, clk, _ := sweepDaemon(t, now)
		rec.green()
		// The cycle's work takes 17 minutes, so the run's end can only be the
		// completion time and its start only the cycle's start.
		cycleWork := 17 * time.Minute
		d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
			clk.Advance(cycleWork)
			return "abc123", nil
		}

		d.triggerTierSweep()
		waitTierSweepIdle(t, d)

		runs := tierSweepJobRuns(t, d.config.TownRoot)
		if len(runs) != 1 {
			t.Fatalf("the ledger holds %d run(s) for a completed cycle, want one: %+v", len(runs), runs)
		}
		if got := runs[0].StartedAt; !got.Equal(now) {
			t.Errorf("run started = %v, want the cycle's start %v", got, now)
		}
		ended := now.Add(cycleWork)
		if got := runs[0].EndedAt; !got.Equal(ended) {
			t.Errorf("run ended = %v, want the cycle's completion %v", got, ended)
		}
		lastRun, found, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep")
		if err != nil || !found {
			t.Fatalf("patrol last run: found=%v err=%v, want it recorded", found, err)
		}
		if !lastRun.Equal(ended) {
			t.Errorf("patrol last run = %v, want the same completion as the run, %v", lastRun, ended)
		}
	})

	t.Run("a red sweep records outcome ok", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, _, _, _ := sweepDaemon(t, now)
		// The default recorder answers a stage with no output, which the cycle
		// judges RED.
		d.triggerTierSweep()
		waitTierSweepIdle(t, d)

		if got := tierSweepTestState(t, d, "gastown").Tiers["shell"].Verdict; got != tierSweepRed {
			t.Fatalf("the cycle's shell verdict = %q, want %q: the row needs a RED sweep", got, tierSweepRed)
		}
		runs := tierSweepJobRuns(t, d.config.TownRoot)
		if len(runs) != 1 {
			t.Fatalf("the ledger holds %d run(s) for a red cycle, want one: %+v", len(runs), runs)
		}
		if got := runs[0].Outcome; got != "ok" {
			t.Errorf("run outcome = %q, want %q: a red sweep is a result, not a failed job", got, "ok")
		}
	})

	t.Run("a cycle that did not complete records no run and no patrol record", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, rec, _, _ := sweepDaemon(t, now)
		rec.green()
		// origin/main cannot be read, so the cycle defers with nothing judged.
		d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
			return "", errors.New("origin/main unreachable")
		}

		d.triggerTierSweep()
		waitTierSweepIdle(t, d)

		if runs := tierSweepJobRuns(t, d.config.TownRoot); len(runs) != 0 {
			t.Errorf("the ledger holds %d run(s) for a cycle that did not complete, want none: %+v", len(runs), runs)
		}
		if _, found, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep"); err != nil || found {
			t.Errorf("patrol last run: found=%v err=%v, want none recorded", found, err)
		}
	})

	t.Run("a failed append is logged and does not cost the patrol record", func(t *testing.T) {
		t.Parallel()
		now := atHour(time.Now(), 15)
		d, rec, _, logs := sweepDaemon(t, now)
		rec.green()
		unwritableJobLedger(t, d.config.TownRoot)

		d.triggerTierSweep()
		waitTierSweepIdle(t, d)

		if !strings.Contains(logs.String(), "the job run was not recorded") {
			t.Fatalf("the failed append logged nothing:\n%s", logs.String())
		}
		lastRun, found, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep")
		if err != nil || !found {
			t.Fatalf("patrol last run: found=%v err=%v, want it recorded despite the failed append", found, err)
		}
		if !lastRun.Equal(now) {
			t.Errorf("patrol last run = %v, want the cycle's completion %v", lastRun, now)
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
