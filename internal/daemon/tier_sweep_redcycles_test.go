package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTierSweepRedCyclesCountsFinishedCycles(t *testing.T) {
	t.Parallel()
	green := map[string]tierSweepTierResult{"shell": {Verdict: tierSweepGreen}}
	tests := []struct {
		name    string
		prior   int
		results map[string]tierSweepTierResult
		green   bool
		want    int
	}{
		{"a fully green cycle resets", 2, green, true, 0},
		{"a cycle that judged no RED tier leaves the count", 1, green, false, 1},
		{"a cycle that judged nothing new leaves the count", 1, nil, false, 1},
	}
	for _, tc := range tests {
		if got := tierSweepRedCycles(tc.prior, tc.results, tc.green); got != tc.want {
			t.Errorf("%s: RedCycles = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestTierSweepRedCyclesCountsACycleThatDidNotStart runs a second red cycle
// through the daemon: a stage whose run never started folds to RED and counts.
func TestTierSweepRedCyclesCountsACycleThatDidNotStart(t *testing.T) {
	t.Parallel()
	d, rec, _, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{LastSHA: "old", RedCycles: 1}); err != nil {
		t.Fatal(err)
	}
	rec.result = func(tierSweepStage) tierSweepStageResult {
		return tierSweepStageResult{ran: true, exitCode: -1, reason: tierSweepRunnerNoResult, output: "sweep-run: the run did not start (exit 1)\n"}
	}
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "aaaa", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.runTierSweep()
	if st := tierSweepTestState(t, d, "gastown"); st.RedCycles != 2 {
		t.Fatalf("RedCycles = %d after a second red cycle that never started, want 2", st.RedCycles)
	}
}

func TestTierSweepRedCyclesOldRecordLoadsAsZero(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := tierSweepStatePath(town, "gastown")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A record written before the field existed carries no red_cycles key.
	if err := os.WriteFile(path, []byte(`{"last_sha":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := readTierSweepState(town, "gastown"); err != nil || st.RedCycles != 0 {
		t.Errorf("RedCycles = %d, err = %v for a record with no red_cycles key, want 0", st.RedCycles, err)
	}
}
