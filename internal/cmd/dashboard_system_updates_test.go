package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/update"
)

// writeEmptyDaemonLog makes the daemon log readable with no sweeps in it, so a
// test's unavailable flag can only come from the history it is checking.
func writeEmptyDaemonLog(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "daemon")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "daemon.log"), nil, 0o600))
}

// writeDaemonLog makes the daemon log readable with one sweep line, so the
// section has a swept commit to join.
func writeDaemonLog(t *testing.T, root, line string) {
	t.Helper()
	writeEmptyDaemonLog(t, root)
	path := filepath.Join(root, "daemon", "daemon.log")
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o600))
}

// The section's rolls come from the update history: a finished run is a roll to
// its target commit, from the build the run probed before it.
func TestSystemUpdatesNowSupplyJoinsRollsFromHistory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeEmptyDaemonLog(t, root)
	const target = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	const before = "1234567890abcdef1234567890abcdef12345678"
	started := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	store := &update.Store{Dir: update.Dir(root)}
	require.NoError(t, store.AppendHistory(update.State{
		RunID:     "run-1",
		Source:    "daemon",
		StartedAt: started,
		EndedAt:   started.Add(4 * time.Minute),
		Target:    update.Target{SHA: target, InstalledSHA: before},
		Current:   "verify",
		Outcome:   update.OutcomeOK,
	}))

	got := systemUpdates(root, newTierSweepReader(root), nil, started.Add(time.Hour))

	require.Len(t, got.Rows, 1, "the roll names one commit row")
	row := got.Rows[0]
	assert.Equal(t, target[:12], row.SHA)
	require.NotNil(t, row.Roll)
	assert.Equal(t, before, row.Roll.From)
	assert.Equal(t, target, row.Roll.To)
	assert.Equal(t, "ok", row.Roll.Outcome)
	assert.Equal(t, "verify", row.Roll.Current)
	assert.True(t, row.Roll.EndedAt.Equal(started.Add(4*time.Minute)))
	assert.False(t, got.Unavailable, "both sources read")
}

// A partial roll carries the step it did not pass, so the pane can say why it
// is not ok. A roll that ended any other way, or a partial one whose steps name
// no miss, carries none and the pane falls back to the outcome word (gt-c8yqh9).
func TestSystemUpdatesRollNamesTheStepAPartialRollMissed(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	const target = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	for _, tc := range []struct {
		name    string
		outcome update.Outcome
		steps   []update.StepRecord
		want    string
	}{
		{"verify failed", update.OutcomePartial, []update.StepRecord{
			{Name: update.StepInstall, Result: update.ResultOK},
			{Name: update.StepRestart, Result: update.ResultOK},
			{Name: update.StepVerify, Result: update.ResultFailed},
		}, "verify"},
		{"restart deferred", update.OutcomePartial, []update.StepRecord{
			{Name: update.StepRestart, Result: update.ResultDeferred},
			{Name: update.StepVerify, Result: update.ResultSkipped},
		}, "restart"},
		{"partial with no miss recorded", update.OutcomePartial, nil, ""},
		{"failed roll names no step", update.OutcomeFailed, []update.StepRecord{
			{Name: update.StepInstall, Result: update.ResultFailed},
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeEmptyDaemonLog(t, root)
			store := &update.Store{Dir: update.Dir(root)}
			require.NoError(t, store.AppendHistory(update.State{
				RunID:     "run-under-test",
				StartedAt: started,
				EndedAt:   started.Add(time.Minute),
				Target:    update.Target{SHA: target},
				Outcome:   tc.outcome,
				Steps:     tc.steps,
			}))

			got := systemUpdates(root, newTierSweepReader(root), nil, started)

			require.Len(t, got.Rows, 1, "the roll names one commit row")
			require.NotNil(t, got.Rows[0].Roll)
			assert.Equal(t, tc.want, got.Rows[0].Roll.Step)
		})
	}
}

// A history that cannot be read is unavailable with no rows, never an empty
// history that reads as no roll has run. The daemon log is readable here, so the
// flag is the history's alone.
func TestSystemUpdatesNowSupplyFlagsAnUnreadableHistory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeEmptyDaemonLog(t, root)
	// A directory where the history file belongs: the file cannot be read as one.
	require.NoError(t, os.MkdirAll(filepath.Join(update.Dir(root), "history.jsonl"), 0o700))

	got := systemUpdates(root, newTierSweepReader(root), nil, time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC))

	assert.True(t, got.Unavailable)
	assert.Empty(t, got.Rows)
	assert.False(t, got.RollUnknown, "no state record is the answer for a town that never ran the job")
}

// An update record that exists but cannot be read marks the roll unknown: a
// section with no Now.Roll is then a reading that failed, never a town with no
// roll running.
func TestSystemUpdatesNowSupplyFlagsAnUnreadableUpdateRecord(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeEmptyDaemonLog(t, root)
	// A directory where the state record belongs: it cannot be read as a file.
	require.NoError(t, os.MkdirAll(filepath.Join(update.Dir(root), "state.json"), 0o700))

	got := systemUpdates(root, newTierSweepReader(root), nil, time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC))

	assert.True(t, got.RollUnknown)
}

// A sweep log that cannot be read marks the section unavailable even when the
// history is fine: the rows then lack the sweep stage they should carry.
func TestSystemUpdatesNowSupplyFlagsAnUnreadableSweepLog(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	started := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	store := &update.Store{Dir: update.Dir(root)}
	require.NoError(t, store.AppendHistory(update.State{
		RunID:     "run-2",
		StartedAt: started,
		EndedAt:   started.Add(time.Minute),
		Target:    update.Target{SHA: "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"},
		Outcome:   update.OutcomeOK,
	}))

	got := systemUpdates(root, newTierSweepReader(root), nil, started)

	assert.True(t, got.Unavailable, "no daemon log means no sweep stage was read")
	require.Len(t, got.Rows, 1, "the roll still joins")
}

// The post-land and images runs of a swept commit fill that row's Runs, so the
// pane's stage chips have what they draw (gt-nhdxkq).
func TestSystemUpdatesPipelineJoinsRunsForASweptCommit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeDaemonLog(t, root, "2026/11/18 09:00:00 tier_sweep: gastown: swept 9f8e7d6c (shell GREEN, integration GREEN) in 9m36s")
	runs := []dashboard.PipelineRun{
		{Workflow: "images.yml", SHA: "9f8e7d6c5b4a", Status: "success"},
		{Workflow: "post-land.yml", SHA: "9f8e7d6c5b4a", Status: "success"},
	}
	other := dashboard.PipelineRun{Workflow: "images.yml", SHA: "1a2b3c4d5e6f", Status: "success"}
	read := func() *dashboard.GastownPipeline {
		return &dashboard.GastownPipeline{Runs: append([]dashboard.PipelineRun{other}, runs...)}
	}

	got := systemUpdates(root, newTierSweepReader(root), read, time.Date(2026, 11, 18, 9, 5, 0, 0, time.Local))

	byCommit := map[string][]dashboard.PipelineRun{}
	for _, row := range got.Rows {
		byCommit[row.SHA] = row.Runs
	}
	assert.Equal(t, runs, byCommit["9f8e7d6c5b4a"], "the swept commit's row holds its own two runs and no other commit's")
	assert.Equal(t, []dashboard.PipelineRun{other}, byCommit["1a2b3c4d5e6f"], "another commit's run sits in that commit's own row")
}

// A nil pipeline read leaves every row without runs, the section a town with no
// viewer client draws today (gt-nhdxkq).
func TestSystemUpdatesPipelineNilLeavesRowsWithoutRuns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeDaemonLog(t, root, "2026/11/18 09:00:00 tier_sweep: gastown: swept 9f8e7d6c (shell GREEN, integration GREEN) in 9m36s")

	got := systemUpdates(root, newTierSweepReader(root), nil, time.Date(2026, 11, 18, 9, 5, 0, 0, time.Local))

	require.Len(t, got.Rows, 1, "the swept commit names one row")
	assert.Empty(t, got.Rows[0].Runs)
}

// The pipeline read is two Forgejo list calls, so it runs once per cadence: a
// build inside the window reuses the last read, a nil result included, and one
// at or past the window reads again (gt-nhdxkq, om finding de02a01e).
func TestSystemUpdatesPipelineThrottle(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	first := &dashboard.GastownPipeline{At: base}
	second := &dashboard.GastownPipeline{At: base.Add(time.Minute)}
	for _, tc := range []struct {
		name      string
		first     *dashboard.GastownPipeline
		advance   time.Duration
		wantReads int
		want      *dashboard.GastownPipeline
	}{
		{"inside the window reuses the last read", first, 30 * time.Second, 1, first},
		{"after the window reads again", first, time.Minute, 2, second},
		{"a nil read is cached inside the window", nil, 30 * time.Second, 1, nil},
		{"a nil read after the window reads again", nil, time.Minute, 2, second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := base
			reads := 0
			read := func() *dashboard.GastownPipeline {
				reads++
				if reads == 1 {
					return tc.first
				}
				return second
			}
			poll := newSystemUpdatesPipeline(read, func() time.Time { return clock }, time.Minute)

			got := poll()
			assert.Equal(t, 1, reads, "the first call reads")
			if tc.first == nil {
				assert.Nil(t, got)
			} else {
				assert.Same(t, tc.first, got)
			}
			clock = clock.Add(tc.advance)
			got = poll()

			assert.Equal(t, tc.wantReads, reads, "the read count")
			if tc.want == nil {
				assert.Nil(t, got)
			} else {
				assert.Same(t, tc.want, got)
			}
		})
	}
}

// TestSystemUpdatesPipelineUnknown: a pipeline read that failed with no good runs
// marks the section RunsUnknown; one that failed but holds last good runs keeps
// them and marks nothing; a healthy empty read marks nothing.
func TestSystemUpdatesPipelineUnknown(t *testing.T) {
	t.Parallel()
	good := []dashboard.PipelineRun{{Workflow: "images.yml", SHA: "9f8e7d6c5b4a", Status: "success"}}
	tests := []struct {
		name string
		read *dashboard.GastownPipeline
		want bool
	}{
		{"a failed read with no runs", &dashboard.GastownPipeline{Runs: []dashboard.PipelineRun{}, Error: "timeout"}, true},
		{"a failed read with last good runs", &dashboard.GastownPipeline{Runs: good, Error: "timeout"}, false},
		{"a healthy read with no runs", &dashboard.GastownPipeline{Runs: []dashboard.PipelineRun{}}, false},
		{"no pipeline", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			read := func() *dashboard.GastownPipeline { return tt.read }
			got := systemUpdates(root, newTierSweepReader(root), read, time.Date(2026, 11, 18, 9, 5, 0, 0, time.Local))
			assert.Equal(t, tt.want, got.RunsUnknown)
		})
	}
}
