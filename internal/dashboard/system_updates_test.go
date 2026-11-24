package dashboard

import (
	"encoding/json"
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
				At:    rolled.EndedAt,
				Sweep: &newSweep,
				Runs:  []PipelineRun{images, postLand},
				Roll:  &rolled,
			}},
		},
		{
			name:   "only a sweep",
			sweeps: []TierSweepRow{sweepOnly},
			want:   []SystemUpdateRow{{SHA: only[:8], At: sweepOnly.At, Sweep: &sweepOnly}},
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
// than the cap, so an extra commit is dropped from the old end. Each row carries
// that latest stamp as At — the roll ends after its sweep here, so At is the
// roll's end, never the sweep's — and the section's JSON names it 'at'.
func TestSystemUpdatesRowsNewestFirstAndCapped(t *testing.T) {
	t.Parallel()

	var sweeps []TierSweepRow
	var rolls []SystemUpdateRoll
	for i := 0; i < systemUpdatesRowCap+1; i++ {
		sha := sysTestSHA(i)
		swept := sysTestClock.Add(time.Duration(i) * time.Minute)
		sweeps = append(sweeps, TierSweepRow{At: swept, Rig: "gastown", SHA: sha[:8]})
		rolls = append(rolls, sysTestRoll(sha, swept, swept.Add(30*time.Second)))
	}
	// Hand the join its commits oldest first, so the newest-first order must
	// come from the stamps, not the input order.
	got := SystemUpdateRows(sweeps, nil, rolls, systemUpdatesRowCap)

	require.Len(t, got, systemUpdatesRowCap, "one commit over the cap is dropped")
	for k := range got {
		newest := len(sweeps) - 1 - k
		// The roll records the full sha, so the row is named by twelve.
		sha := sysTestSHA(newest)[:12]
		assert.Equal(t, sha, got[k].SHA, "row %d is not the %dth-newest commit", k, k)
		assert.Equal(t, rolls[newest].EndedAt, got[k].At,
			"row %d (%s) does not carry the newest time its stages reported", k, sha)
	}

	section, err := json.Marshal(SystemUpdates{Rows: got})
	require.NoError(t, err)
	var decoded struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(section, &decoded))
	require.Len(t, decoded.Rows, len(got))
	for k := range decoded.Rows {
		raw, ok := decoded.Rows[k]["at"]
		require.True(t, ok, "row %d (%s) JSON has no 'at'", k, got[k].SHA)
		want, err := json.Marshal(got[k].At)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(raw), "row %d (%s) JSON at", k, got[k].SHA)
	}
}

// A row whose stages reported no time has a zero At, which the row's JSON omits
// rather than drawing the year-1 zero instant.
func TestSystemUpdatesRowsAtIsZeroWhenNoStageHasATime(t *testing.T) {
	t.Parallel()

	sha := sysTestSHA(1)
	queued := PipelineRun{Workflow: imagesWorkflow, SHA: sha[:12], Status: pipelineQueued}
	got := SystemUpdateRows(nil, &GastownPipeline{Runs: []PipelineRun{queued}}, nil, systemUpdatesRowCap)

	require.Len(t, got, 1)
	assert.True(t, got[0].At.IsZero(), "a row with no stage time has no At")

	payload, err := json.Marshal(got[0])
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &fields))
	_, ok := fields["at"]
	assert.False(t, ok, "a zero At is omitted from the row's JSON")
}

// A record with no sha a join trusts names no commit: a blank roll target, a
// blank run sha and a too-short sweep are dropped rather than shown as a row
// with no name. A cap of zero or less draws nothing, and every joined commit is
// reported left out.
func TestSystemUpdatesRowsSkipsCommitlessRecords(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		sweeps   []TierSweepRow
		pipeline *GastownPipeline
		rolls    []SystemUpdateRoll
		limit    int
		wantCut  int
	}{
		{
			name:     "blank and short shas",
			sweeps:   []TierSweepRow{{At: sysTestClock, SHA: "abc123"}},
			pipeline: &GastownPipeline{Runs: []PipelineRun{{SHA: "", Status: pipelineQueued}}},
			rolls:    []SystemUpdateRoll{{StartedAt: sysTestClock}},
			limit:    systemUpdatesRowCap,
			wantCut:  0,
		},
		{
			name:    "zero cap",
			sweeps:  []TierSweepRow{{At: sysTestClock, SHA: sysTestSHA(3)[:8]}},
			limit:   0,
			wantCut: 1,
		},
		{
			name:    "negative cap",
			sweeps:  []TierSweepRow{{At: sysTestClock, SHA: sysTestSHA(4)[:8]}},
			limit:   -1,
			wantCut: 1,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rows, cut := SystemUpdateRowsWithMore(tt.sweeps, tt.pipeline, tt.rolls, tt.limit)
			assert.Empty(t, rows, "%s: rows drawn", tt.name)
			assert.Equal(t, tt.wantCut, cut, "%s: commits reported left out", tt.name)
		})
	}
}

// A section whose cap cut joined commits carries how many it left out: nine
// joined under a cap of six cut three, and four joined cut none. The rows are
// still the newest ones, so the cut count names the older commits missing from
// the drawn end.
func TestSystemUpdatesRowsReportsHowManyWereCut(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		joined  int
		limit   int
		wantCut int
	}{
		{name: "nine joined under a cap of six", joined: 9, limit: 6, wantCut: 3},
		{name: "four joined under a cap of six", joined: 4, limit: 6, wantCut: 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var sweeps []TierSweepRow
			for i := 0; i < tt.joined; i++ {
				sweeps = append(sweeps, TierSweepRow{
					At:  sysTestClock.Add(time.Duration(i) * time.Minute),
					Rig: "gastown",
					SHA: sysTestSHA(i)[:8],
				})
			}
			rows, cut := SystemUpdateRowsWithMore(sweeps, nil, nil, tt.limit)

			assert.Equal(t, tt.wantCut, cut, "%s: commits reported left out", tt.name)
			wantShown := min(tt.joined, tt.limit)
			require.Len(t, rows, wantShown, "%s: rows drawn", tt.name)
			for k := range rows {
				newest := tt.joined - 1 - k
				assert.Equal(t, sysTestSHA(newest)[:8], rows[k].SHA,
					"%s: row %d is not the %dth-newest commit", tt.name, k, k)
			}
		})
	}
}
