package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tierSweepTestReader is a reader over fixture daemon-log lines, with no clock
// and no real log: read() takes its now, so a test never waits.
func tierSweepTestReader(t *testing.T, lines ...string) (*tierSweepReader, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &tierSweepReader{logPath: path, stateDir: filepath.Join(dir, "tier-sweep"), last: map[string]tierSweepEvent{}}, path
}

func TestTierSweepReadsACycleFromTheLog(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t,
		"2026/10/03 22:51:48 tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:",
		"tier-sweep: shell GREEN passed=6 failed=0 skipped=0 (logs /tmp/tier-sweep.x) in 1m4s",
		"2026/10/03 23:00:19 tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration GREEN, race GREEN) in 9m36s",
		`2026/10/03 23:00:19 dog_cycle: tier_sweep outcome=ran steps=[gastown=done] reason=""`,
	)
	ts := r.read(time.Date(2026, 10, 3, 23, 5, 0, 0, time.Local))
	if ts.Unavailable || ts.Running != nil {
		t.Fatalf("a closed cycle reads idle, not unavailable: %+v", ts)
	}
	if len(ts.Sweeps) != 1 {
		t.Fatalf("sweeps = %+v", ts.Sweeps)
	}
	row := ts.Sweeps[0]
	if row.Rig != "gastown" || row.SHA != "a9be03e4" || row.Secs == nil || *row.Secs != 576 {
		t.Fatalf("row = %+v", row)
	}
	if len(row.Stages) != 3 || row.Stages[1].Tier != "integration" || row.Stages[2].Verdict != "GREEN" {
		t.Errorf("stages = %+v", row.Stages)
	}
}

// The daemon logs no cycle start, so a sweep reads as running from the shell
// stage's finished line until the swept line closes the cycle.
func TestTierSweepRunningWhileASweepIsInFlight(t *testing.T) {
	t.Parallel()
	shellDone := time.Date(2026, 10, 3, 22, 51, 48, 0, time.Local)
	r, _ := tierSweepTestReader(t,
		"2026/10/03 22:51:48 tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:",
		"tier-sweep: shell GREEN passed=6 failed=0 skipped=0 (logs /tmp/tier-sweep.x) in 1m4s",
	)
	ts := r.read(shellDone.Add(3 * time.Minute))
	if ts.Running == nil {
		t.Fatalf("a finished shell stage with no swept line is a sweep in flight: %+v", ts)
	}
	if run := ts.Running; run.Rig != "gastown" || run.Tier != "integration" || run.ElapsedSec != 180 {
		t.Errorf("running = %+v", run)
	}
	if ts := r.read(shellDone.Add(tierSweepStaleAfter + time.Minute)); ts.Running != nil {
		t.Errorf("a shell line older than the stage budget is not a live sweep: %+v", ts.Running)
	}
	r.parseLogLine("2026/10/03 23:00:19 tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration GREEN, race GREEN) in 9m36s")
	if ts := r.read(shellDone.Add(9 * time.Minute)); ts.Running != nil {
		t.Errorf("a swept cycle still reads running: %+v", ts.Running)
	}
}

func TestTierSweepLineWithoutADuration(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t, "2026/10/01 09:00:00 tier_sweep: gastown: swept deadbeef (shell GREEN)")
	ts := r.read(time.Date(2026, 10, 3, 0, 0, 0, 0, time.Local))
	if len(ts.Sweeps) != 1 || ts.Sweeps[0].Secs != nil {
		t.Fatalf("a swept line from before gt-iqzr0 has no duration: %+v", ts.Sweeps)
	}
	if s := ts.Sweeps[0].Stages; len(s) != 1 || s[0].Verdict != "GREEN" {
		t.Errorf("stages = %+v", s)
	}
}

func TestTierSweepRedNamesComeFromTheRecord(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t, "2026/10/03 23:00:19 tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration RED, race GREEN) in 9m36s")
	writeTierSweepRecord(t, r.stateDir, "gastown", `{"last_sha":"a9be03e4ffff","tiers":{"integration":{"verdict":"RED","passed":1,"failed":2,"failed_names":["./internal/cmd","./internal/daemon"]}}}`)
	ts := r.read(time.Date(2026, 10, 3, 23, 5, 0, 0, time.Local))
	got := ts.Sweeps[0].Stages[1]
	if got.Verdict != "RED" || len(got.Failed) != 2 || got.Failed[0] != "./internal/cmd" {
		t.Fatalf("red tier = %+v", got)
	}

	// The record holds one run per rig, so a record for another sha names nothing.
	other, _ := tierSweepTestReader(t, "2026/10/03 23:00:19 tier_sweep: gastown: swept a9be03e4 (integration RED) in 1m")
	writeTierSweepRecord(t, other.stateDir, "gastown", `{"last_sha":"0000ffff","tiers":{"integration":{"verdict":"RED","failed_names":["x"]}}}`)
	if got := other.read(time.Date(2026, 10, 3, 23, 5, 0, 0, time.Local)).Sweeps[0].Stages[0]; len(got.Failed) != 0 {
		t.Errorf("another sweep's record named this row's failures: %+v", got)
	}
}

func writeTierSweepRecord(t *testing.T, dir, rig, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rig+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTierSweepUnreadableLog(t *testing.T) {
	t.Parallel()
	r := &tierSweepReader{logPath: filepath.Join(t.TempDir(), "missing.log"), stateDir: t.TempDir(), last: map[string]tierSweepEvent{}}
	ts := r.read(time.Now())
	if !ts.Unavailable {
		t.Fatalf("an unreadable log must not read as a log with no sweeps: %+v", ts)
	}
}

func TestTierSweepScanIsIncremental(t *testing.T) {
	t.Parallel()
	r, path := tierSweepTestReader(t, "2026/10/03 22:00:00 tier_sweep: gastown: swept 11111111 (shell GREEN) in 1m")
	now := time.Date(2026, 10, 3, 23, 0, 0, 0, time.Local)
	if ts := r.read(now); len(ts.Sweeps) != 1 {
		t.Fatalf("sweeps = %+v", ts.Sweeps)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("2026/10/03 23:00:00 tier_sweep: gastown: swept 22222222 (shell GREEN, race GREEN) in 2m\n")
	_ = f.Close()
	ts := r.read(now)
	if len(ts.Sweeps) != 2 || ts.Sweeps[0].SHA != "22222222" {
		t.Fatalf("an appended line is read once, newest first: %+v", ts.Sweeps)
	}
	if ts := r.read(now); len(ts.Sweeps) != 2 {
		t.Fatalf("a rescan adds nothing: %+v", ts.Sweeps)
	}
}
