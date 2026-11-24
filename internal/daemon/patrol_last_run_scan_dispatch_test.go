package daemon

import (
	"io"
	"log"
	"testing"
	"time"
)

// TestPatrolLastRunScanDispatchFieldsRecord pins gt-y872pt: Daemon carries a
// last-run memory for patrol_scan and for spec_dispatch, keyed as gt jobs reads
// them, so a completed pass can be recorded there rather than reading never_run.
func TestPatrolLastRunScanDispatchFieldsRecord(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 24, 18, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		patrol string
		mem    func(*Daemon) *patrolLastRunMemory
		// at differs per row, so a run recorded through the wrong key shows as
		// the sibling's time rather than passing on an equal one.
		at time.Time
	}{
		{patrol: "patrol_scan", mem: func(d *Daemon) *patrolLastRunMemory { return &d.patrolScanRun }, at: base},
		{patrol: "spec_dispatch", mem: func(d *Daemon) *patrolLastRunMemory { return &d.specDispatchRun }, at: base.Add(time.Hour)},
	} {
		t.Run(tc.patrol, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{
				config: &Config{TownRoot: t.TempDir()},
				logger: log.New(io.Discard, "", 0),
			}

			d.recordPatrolRun(tc.mem(d), tc.patrol, tc.at)

			if got := tc.mem(d).last(); !got.Equal(tc.at) {
				t.Errorf("%s in-memory last run = %v, want %v", tc.patrol, got, tc.at)
			}
			runs, err := ReadPatrolLastRuns(d.config.TownRoot)
			if err != nil {
				t.Fatalf("ReadPatrolLastRuns: %v", err)
			}
			if got, ok := runs[tc.patrol]; !ok || !got.Equal(tc.at) {
				t.Errorf("%s last run = %v (recorded %v), want %v; runs: %v", tc.patrol, got, ok, tc.at, runs)
			}
			for _, other := range []string{"patrol_scan", "spec_dispatch"} {
				if other == tc.patrol {
					continue
				}
				if got, ok := runs[other]; ok {
					t.Errorf("recording %s also wrote %s = %v; each field keeps its own entry", tc.patrol, other, got)
				}
			}
		})
	}
}
