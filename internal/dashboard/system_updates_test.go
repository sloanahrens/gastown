package dashboard

import (
	"encoding/json"
	"fmt"
	"slices"
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

// TestSystemUpdateRowsKeep holds the commits the section draws beside its newest
// rows: the commit under a sweep in flight and the last green sweep's. A kept
// commit the cap cut is drawn after the newest rows, a kept sha no joined commit
// names is drawn with no stages, and a limit of zero or less draws no keep
// (gt-hueeyb).
func TestSystemUpdateRowsKeep(t *testing.T) {
	t.Parallel()

	// sha is the eight-character sha of commit i; commit 7 is the newest of the
	// eight joined commits, commit 0 the oldest.
	sha := func(i int) string { return sysTestSHA(i)[:8] }
	var sweeps []TierSweepRow
	for i := range 8 {
		sweeps = append(sweeps, TierSweepRow{At: sysTestClock.Add(time.Duration(i) * time.Minute), Rig: "gastown", SHA: sha(i)})
	}
	newest := []string{sha(7), sha(6), sha(5), sha(4), sha(3), sha(2)}

	cases := []struct {
		name     string
		limit    int
		keeps    []string
		want     []string
		wantMore int
	}{
		{
			name:     "no keep draws the newest six",
			limit:    systemUpdatesRowCap,
			want:     newest,
			wantMore: 2,
		},
		{
			name:     "the swept commit the cap cut is drawn after the newest",
			limit:    systemUpdatesRowCap,
			keeps:    []string{sha(0)},
			want:     append(slices.Clone(newest), sha(0)),
			wantMore: 1,
		},
		{
			name:     "the swept and green shas naming one commit draw one row",
			limit:    systemUpdatesRowCap,
			keeps:    []string{sha(0), sysTestSHA(0)[:12]},
			want:     append(slices.Clone(newest), sha(0)),
			wantMore: 1,
		},
		{
			name:     "a keep the newest rows draw is not repeated",
			limit:    systemUpdatesRowCap,
			keeps:    []string{sha(7)},
			want:     newest,
			wantMore: 2,
		},
		{
			name:     "older kept rows are newest first among themselves",
			limit:    systemUpdatesRowCap,
			keeps:    []string{sha(0), sha(1)},
			want:     append(slices.Clone(newest), sha(1), sha(0)),
			wantMore: 0,
		},
		{
			name:     "a sha no joined commit names draws a row after the joined ones",
			limit:    systemUpdatesRowCap,
			keeps:    []string{sha(9)},
			want:     append(slices.Clone(newest), sha(9)),
			wantMore: 2,
		},
		{
			name:     "a limit of zero adds no keep",
			limit:    0,
			keeps:    []string{sha(0)},
			want:     []string{},
			wantMore: len(sweeps),
		},
		{
			name:     "a blank or short keep names no commit",
			limit:    systemUpdatesRowCap,
			keeps:    []string{"", "abc"},
			want:     newest,
			wantMore: 2,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rows, more := SystemUpdateRowsWithMore(sweeps, nil, nil, tt.limit, tt.keeps...)
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.SHA)
			}
			assert.Equal(t, tt.want, got, "rows drawn")
			assert.Equal(t, tt.wantMore, more, "commits reported left out")
		})
	}

	t.Run("a sha no joined commit names has no stages and no time", func(t *testing.T) {
		t.Parallel()
		rows, _ := SystemUpdateRowsWithMore(sweeps[:3], nil, nil, systemUpdatesRowCap, sysTestSHA(9)[:8])
		require.Len(t, rows, 4)
		last := rows[3]
		assert.Equal(t, sysTestSHA(9)[:8], last.SHA)
		assert.True(t, last.At.IsZero(), "a row with no join has no time")
		assert.Nil(t, last.Sweep)
		assert.Empty(t, last.Runs)
		assert.Nil(t, last.Roll)
	})

	t.Run("NewSystemUpdates draws the swept and last green commits", func(t *testing.T) {
		t.Parallel()
		// Commit 0 is the only green sweep, so it is the last green one, and the
		// swept commit 1 is the second oldest, which the cap cuts.
		var judged []TierSweepRow
		for i := range 8 {
			verdict := "RED"
			if i == 0 {
				verdict = "GREEN"
			}
			judged = append(judged, sweepAt(i, "gastown", sha(i), verdict))
		}
		su := NewSystemUpdates(judged, nil, nil, false, sha(1))
		require.NotNil(t, su.LastGreen)
		assert.Equal(t, sha(0), su.LastGreen.SHA)
		var got []string
		for _, row := range su.Rows {
			got = append(got, row.SHA)
		}
		assert.Equal(t, append(slices.Clone(newest), sha(1), sha(0)), got, "rows drawn")
		assert.Equal(t, 0, su.More, "commits reported left out")
	})
}

// sweepAt is a sweep of the rig at minute min of the test clock, one stage per
// verdict, so a test reads its verdicts in order.
func sweepAt(min int, rig, sha string, verdicts ...string) TierSweepRow {
	stages := make([]TierSweepStage, 0, len(verdicts))
	for i, v := range verdicts {
		stages = append(stages, TierSweepStage{Tier: fmt.Sprintf("tier-%d", i), Verdict: v})
	}
	return TierSweepRow{At: sysTestClock.Add(time.Duration(min) * time.Minute), Rig: rig, SHA: sha, Stages: stages}
}

// The section draws the gastown rig's newest five sweeps, newest first, and
// leaves out the other rigs' sweeps.
func TestSystemUpdatesSweepsKeepsTheNewestFiveOfTheRig(t *testing.T) {
	t.Parallel()

	sweeps := []TierSweepRow{
		sweepAt(3, "gastown", sysTestSHA(3)[:8], "GREEN"),
		sweepAt(7, "gastown", sysTestSHA(7)[:8], "GREEN"),
		sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN"),
		sweepAt(9, "other", sysTestSHA(9)[:8], "GREEN"),
		sweepAt(5, "gastown", sysTestSHA(5)[:8], "RED"),
		sweepAt(2, "gastown", sysTestSHA(2)[:8], "GREEN"),
		sweepAt(6, "gastown", sysTestSHA(6)[:8], "GREEN"),
		sweepAt(4, "gastown", sysTestSHA(4)[:8], "GREEN"),
	}
	su := NewSystemUpdates(sweeps, nil, nil, false)

	var got []string
	for _, s := range su.Sweeps {
		got = append(got, s.SHA)
	}
	assert.Equal(t, []string{sysTestSHA(7)[:8], sysTestSHA(6)[:8], sysTestSHA(5)[:8], sysTestSHA(4)[:8], sysTestSHA(3)[:8]}, got)
}

// The section's sweeps are a copy: the caller's slice keeps the order it came in.
func TestSystemUpdatesSweepsLeaveTheCallersSliceInItsOrder(t *testing.T) {
	t.Parallel()

	sweeps := []TierSweepRow{
		sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN"),
		sweepAt(2, "gastown", sysTestSHA(2)[:8], "GREEN"),
	}
	NewSystemUpdates(sweeps, nil, nil, false)

	assert.Equal(t, sysTestSHA(1)[:8], sweeps[0].SHA)
	assert.Equal(t, sysTestSHA(2)[:8], sweeps[1].SHA)
}

// Each sweep passes its wall time through as written, nil for an old line.
func TestSystemUpdatesSweepsPassSecsThrough(t *testing.T) {
	t.Parallel()

	took := 12.5
	timed := sweepAt(2, "gastown", sysTestSHA(2)[:8], "GREEN")
	timed.Secs = &took
	untimed := sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN")
	su := NewSystemUpdates([]TierSweepRow{untimed, timed}, nil, nil, false)

	require.Len(t, su.Sweeps, 2)
	require.NotNil(t, su.Sweeps[0].Secs)
	assert.Equal(t, 12.5, *su.Sweeps[0].Secs)
	assert.Nil(t, su.Sweeps[1].Secs)
}

// LastGreen is the newest gastown sweep whose every stage is GREEN, so a newer
// sweep that ran red does not hide the green one behind it.
func TestSystemUpdatesSweepsLastGreenIsTheNewestAllGreenSweep(t *testing.T) {
	t.Parallel()

	sweeps := []TierSweepRow{
		sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN"),
		sweepAt(2, "gastown", sysTestSHA(2)[:8], "GREEN", "GREEN"),
		sweepAt(3, "gastown", sysTestSHA(3)[:8], "GREEN", "RED"),
	}
	su := NewSystemUpdates(sweeps, nil, nil, false)

	require.NotNil(t, su.LastGreen)
	assert.Equal(t, sysTestSHA(2)[:8], su.LastGreen.SHA)
}

// LastGreen carries the green sweep's sha as written and the time it ran.
func TestSystemUpdatesSweepsLastGreenCarriesItsShaAndTime(t *testing.T) {
	t.Parallel()

	green := sweepAt(4, "gastown", sysTestSHA(4)[:8], "GREEN")
	su := NewSystemUpdates([]TierSweepRow{green}, nil, nil, false)

	require.NotNil(t, su.LastGreen)
	assert.Equal(t, green.SHA, su.LastGreen.SHA)
	assert.Equal(t, green.At, su.LastGreen.At)
}

// LastGreen is searched over every gastown sweep the reader holds, not only the
// five the section draws.
func TestSystemUpdatesSweepsLastGreenReadsEveryRowNotOnlyTheFive(t *testing.T) {
	t.Parallel()

	var sweeps []TierSweepRow
	sweeps = append(sweeps, sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN"))
	for i := 2; i <= 7; i++ {
		sweeps = append(sweeps, sweepAt(i, "gastown", sysTestSHA(i)[:8], "RED"))
	}
	su := NewSystemUpdates(sweeps, nil, nil, false)

	require.Len(t, su.Sweeps, 5)
	require.NotNil(t, su.LastGreen)
	assert.Equal(t, sysTestSHA(1)[:8], su.LastGreen.SHA)
}

// A newer GREEN sweep of another rig is ignored: LastGreen stays with the gastown
// sweep, and the other rig's row does not draw in Sweeps.
func TestSystemUpdatesSweepsLastGreenIgnoresOtherRigs(t *testing.T) {
	t.Parallel()

	sweeps := []TierSweepRow{
		sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN"),
		sweepAt(5, "other", sysTestSHA(5)[:8], "GREEN", "GREEN"),
	}
	su := NewSystemUpdates(sweeps, nil, nil, false)

	require.NotNil(t, su.LastGreen)
	assert.Equal(t, sysTestSHA(1)[:8], su.LastGreen.SHA)
	require.Len(t, su.Sweeps, 1)
	assert.Equal(t, sysTestSHA(1)[:8], su.Sweeps[0].SHA)
}

// A RED newest sweep leaves LastGreen at the older green one.
func TestSystemUpdatesSweepsARedNewestLeavesLastGreenAtTheOlderGreen(t *testing.T) {
	t.Parallel()

	sweeps := []TierSweepRow{
		sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN"),
		sweepAt(2, "gastown", sysTestSHA(2)[:8], "RED"),
	}
	su := NewSystemUpdates(sweeps, nil, nil, false)

	require.NotNil(t, su.LastGreen)
	assert.Equal(t, sysTestSHA(1)[:8], su.LastGreen.SHA)
}

// No green sweep in the rig leaves LastGreen nil, so the page draws no last-green line.
func TestSystemUpdatesSweepsNoGreenSweepLeavesLastGreenNil(t *testing.T) {
	t.Parallel()

	sweeps := []TierSweepRow{
		sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN", "RED"),
		sweepAt(2, "gastown", sysTestSHA(2)[:8], "RED"),
	}
	su := NewSystemUpdates(sweeps, nil, nil, false)

	assert.Nil(t, su.LastGreen)
}

// A sweep whose stage list is empty is not green, so the older green one stands.
func TestSystemUpdatesSweepsAnEmptyStageListIsNotGreen(t *testing.T) {
	t.Parallel()

	sweeps := []TierSweepRow{
		sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN"),
		sweepAt(2, "gastown", sysTestSHA(2)[:8]),
	}
	su := NewSystemUpdates(sweeps, nil, nil, false)

	require.NotNil(t, su.LastGreen)
	assert.Equal(t, sysTestSHA(1)[:8], su.LastGreen.SHA)
}

// A stage whose verdict is blank, or anything but GREEN exactly, is not green.
func TestSystemUpdatesSweepsANonGreenVerdictIsNotGreen(t *testing.T) {
	t.Parallel()

	for _, verdict := range []string{"", "green", "SKIPPED"} {
		t.Run("verdict "+verdict, func(t *testing.T) {
			t.Parallel()
			sweeps := []TierSweepRow{
				sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN"),
				sweepAt(2, "gastown", sysTestSHA(2)[:8], "GREEN", verdict),
			}
			su := NewSystemUpdates(sweeps, nil, nil, false)

			require.NotNil(t, su.LastGreen)
			assert.Equal(t, sysTestSHA(1)[:8], su.LastGreen.SHA)
		})
	}
}

// A commit's newest sweep decides it: a newer red sweep of a commit hides the
// green sweep that commit had earlier, so the older green commit stands.
func TestSystemUpdatesSweepsANewerRedSweepOfTheCommitHidesItsGreen(t *testing.T) {
	t.Parallel()

	sweeps := []TierSweepRow{
		sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN"),
		sweepAt(2, "gastown", sysTestSHA(2)[:8], "GREEN"),
		sweepAt(3, "gastown", sysTestSHA(2)[:8], "RED"),
	}
	su := NewSystemUpdates(sweeps, nil, nil, false)

	require.NotNil(t, su.LastGreen)
	assert.Equal(t, sysTestSHA(1)[:8], su.LastGreen.SHA)
}

// Ahead counts the joined commits, other than the green one, whose first stage is
// after the last green sweep, before the row cap. An older commit whose roll ended
// after the green sweep does not count: it started before it.
func TestSystemUpdatesAheadCounts(t *testing.T) {
	t.Parallel()

	green := sweepAt(0, "gastown", sysTestSHA(0)[:8], "GREEN")
	red := func(min int, rig string, i int) TierSweepRow { return sweepAt(min, rig, sysTestSHA(i)[:8], "RED") }
	eight := []TierSweepRow{green}
	for i := 1; i <= 7; i++ {
		eight = append(eight, red(i, "gastown", i))
	}
	cases := []struct {
		name     string
		sweeps   []TierSweepRow
		pipeline *GastownPipeline
		rolls    []SystemUpdateRoll
		want     int
		wantCut  int
	}{
		// The green commit is the oldest, so the cap cuts it; it is drawn beside the
		// rows as the last green sweep, so one commit is left out (gt-hueeyb).
		{name: "after the green sweep, the cap cut ones too", sweeps: eight, want: 7, wantCut: 1},
		{
			name:   "the green commit's own later roll does not count",
			sweeps: []TierSweepRow{sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN")},
			rolls:  []SystemUpdateRoll{sysTestRoll(sysTestSHA(1), sysTestClock.Add(5*time.Minute), sysTestClock.Add(6*time.Minute))},
		},
		{
			name:   "an older commit whose roll ended after the green sweep",
			sweeps: []TierSweepRow{red(1, "gastown", 1), sweepAt(3, "gastown", sysTestSHA(2)[:8], "GREEN")},
			rolls:  []SystemUpdateRoll{sysTestRoll(sysTestSHA(1), sysTestClock.Add(2*time.Minute), sysTestClock.Add(5*time.Minute))},
		},
		{
			name:     "a commit first reported by a run or a roll",
			sweeps:   []TierSweepRow{green},
			pipeline: &GastownPipeline{Runs: []PipelineRun{{Workflow: postLandWorkflow, SHA: sysTestSHA(2)[:12], Status: pipelineSuccess, Started: sysTestClock.Add(2 * time.Minute)}}},
			rolls:    []SystemUpdateRoll{sysTestRoll(sysTestSHA(3), sysTestClock.Add(4*time.Minute), sysTestClock.Add(5*time.Minute))},
			want:     2,
		},
		{name: "another rig's sweep", sweeps: []TierSweepRow{green, red(4, "other", 4)}},
		{name: "no green sweep is -1", sweeps: []TierSweepRow{red(1, "gastown", 1)}, want: -1},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			su := NewSystemUpdates(tt.sweeps, tt.pipeline, tt.rolls, false)

			assert.Equal(t, tt.want, su.Ahead, "Ahead")
			assert.Equal(t, tt.wantCut, su.More, "commits the row cap left out")
		})
	}
}

// Ahead serializes a zero count as 0 and no green sweep as -1.
func TestSystemUpdatesAheadSerializes(t *testing.T) {
	t.Parallel()

	zero, err := json.Marshal(NewSystemUpdates([]TierSweepRow{sweepAt(1, "gastown", sysTestSHA(1)[:8], "GREEN")}, nil, nil, false))
	require.NoError(t, err)
	assert.Contains(t, string(zero), `"ahead":0`)
	none, err := json.Marshal(NewSystemUpdates([]TierSweepRow{sweepAt(1, "gastown", sysTestSHA(1)[:8], "RED")}, nil, nil, false))
	require.NoError(t, err)
	assert.Contains(t, string(none), `"ahead":-1`)
}
