package dashboard

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sysTestClock is the fixed time every row's stamps are built from.
var sysTestClock = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

// sysTestSHA names commit i with forty digits whose first eight differ, so no
// two commits share a prefix and every row is named by its first twelve.
func sysTestSHA(i int) string {
	return fmt.Sprintf("%08d%032d", i, 0)
}

// sysTestRoll is a finished roll to the commit, from an earlier build.
func sysTestRoll(to string, start, end time.Time) SystemUpdateRoll {
	return SystemUpdateRoll{From: "older", To: to, Outcome: "ok", StartedAt: start, EndedAt: end}
}

// A row holds every stage a commit reached and only those it did, joined across
// the sweep's eight-character sha, the run's twelve and the roll's forty. A
// commit swept or rolled more than once keeps the newest, whichever order the
// records arrive in.
func TestSystemUpdatesRowsJoinsStagesBySHA(t *testing.T) {
	t.Parallel()

	all := sysTestSHA(1)
	t0 := sysTestClock
	oldSweep := TierSweepRow{At: t0.Add(-2 * time.Hour), Rig: "gastown", SHA: all[:8]}
	midSweep := TierSweepRow{At: t0.Add(-time.Hour), Rig: "other", SHA: all[:8]}
	newSweep := TierSweepRow{At: t0, Rig: "gastown", SHA: all[:8]}
	postLand := PipelineRun{Workflow: postLandWorkflow, SHA: all[:12], Status: pipelineSuccess, Started: t0.Add(-time.Minute)}
	images := PipelineRun{Workflow: imagesWorkflow, SHA: all[:12], Status: pipelineSuccess, Started: t0.Add(2 * time.Minute)}
	staleRoll := sysTestRoll(all, t0.Add(-3*time.Hour), t0.Add(-3*time.Hour+time.Minute))
	rolled := sysTestRoll(all, t0.Add(5*time.Minute), t0.Add(6*time.Minute))
	midRoll := sysTestRoll(all, t0.Add(-2*time.Hour), t0.Add(-2*time.Hour+time.Minute))
	only := sysTestSHA(2)
	sweepOnly := TierSweepRow{At: t0.Add(-time.Hour), Rig: "gastown", SHA: only[:8]}

	cases := []struct {
		name     string
		sweeps   []TierSweepRow
		pipeline *GastownPipeline
		rolls    []SystemUpdateRoll
		want     []SystemUpdateRow
	}{
		{
			name:     "all three stages",
			sweeps:   []TierSweepRow{oldSweep, newSweep, midSweep},
			pipeline: &GastownPipeline{Runs: []PipelineRun{images, postLand}},
			rolls:    []SystemUpdateRoll{staleRoll, rolled, midRoll},
			want: []SystemUpdateRow{{
				SHA:   all[:12],
				Sweep: &newSweep,
				Runs:  []PipelineRun{images, postLand},
				Roll:  &rolled,
			}},
		},
		{
			name:   "only a sweep",
			sweeps: []TierSweepRow{sweepOnly},
			want:   []SystemUpdateRow{{SHA: only[:8], Sweep: &sweepOnly}},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SystemUpdateRows(tt.sweeps, tt.pipeline, tt.rolls, systemUpdatesRowCap)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Rows are newest first by the latest stage the commit reached and never more
// than the cap, so an extra commit is dropped from the old end.
func TestSystemUpdatesRowsNewestFirstAndCapped(t *testing.T) {
	t.Parallel()

	var sweeps []TierSweepRow
	for i := 0; i < systemUpdatesRowCap+1; i++ {
		sweeps = append(sweeps, TierSweepRow{
			At:  sysTestClock.Add(time.Duration(i) * time.Minute),
			Rig: "gastown",
			SHA: sysTestSHA(i)[:8],
		})
	}
	// Hand the join its commits oldest first, so the newest-first order must
	// come from the stamps, not the input order.
	got := SystemUpdateRows(sweeps, nil, nil, systemUpdatesRowCap)

	require.Len(t, got, systemUpdatesRowCap, "one commit over the cap is dropped")
	for k := range got {
		newest := len(sweeps) - 1 - k
		assert.Equal(t, sysTestSHA(newest)[:8], got[k].SHA, "row %d is not the %dth-newest commit", k, k)
	}
}

// A record with no sha a join trusts names no commit: a blank roll target, a
// blank run sha and a too-short sweep are dropped rather than shown as a row
// with no name. A cap of zero or less draws nothing.
func TestSystemUpdatesRowsSkipsCommitlessRecords(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		sweeps   []TierSweepRow
		pipeline *GastownPipeline
		rolls    []SystemUpdateRoll
		limit    int
	}{
		{
			name:     "blank and short shas",
			sweeps:   []TierSweepRow{{At: sysTestClock, SHA: "abc123"}},
			pipeline: &GastownPipeline{Runs: []PipelineRun{{SHA: "", Status: pipelineQueued}}},
			rolls:    []SystemUpdateRoll{{StartedAt: sysTestClock}},
			limit:    systemUpdatesRowCap,
		},
		{
			name:   "zero cap",
			sweeps: []TierSweepRow{{At: sysTestClock, SHA: sysTestSHA(3)[:8]}},
			limit:  0,
		},
		{
			name:   "negative cap",
			sweeps: []TierSweepRow{{At: sysTestClock, SHA: sysTestSHA(4)[:8]}},
			limit:  -1,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, SystemUpdateRows(tt.sweeps, tt.pipeline, tt.rolls, tt.limit))
		})
	}
}
