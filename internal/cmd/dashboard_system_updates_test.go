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

	got := systemUpdates(root, newTierSweepReader(root), started.Add(time.Hour))

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

			got := systemUpdates(root, newTierSweepReader(root), started)

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

	got := systemUpdates(root, newTierSweepReader(root), time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC))

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

	got := systemUpdates(root, newTierSweepReader(root), time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC))

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

	got := systemUpdates(root, newTierSweepReader(root), started)

	assert.True(t, got.Unavailable, "no daemon log means no sweep stage was read")
	require.Len(t, got.Rows, 1, "the roll still joins")
}

// The pipeline read is throttled: a build inside the window answers the previous
// result without the read's Forgejo calls, a nil one included, and the first
// build at the caller's window reads again (gt-066cn5).
func TestSystemUpdatesPipelineThrottle(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	first := &dashboard.GastownPipeline{Runs: []dashboard.PipelineRun{{Workflow: "post-land.yml"}}, At: start}
	second := &dashboard.GastownPipeline{Runs: []dashboard.PipelineRun{{Workflow: "images.yml"}}, At: start}

	type step struct {
		at        time.Duration // since start
		wantReads int           // underlying reads made by the end of the step
		want      *dashboard.GastownPipeline
	}
	for _, tc := range []struct {
		name    string
		every   time.Duration
		results []*dashboard.GastownPipeline // each underlying read answers with the next
		steps   []step
	}{
		{
			name:    "first call reads and a second inside the window reuses it",
			every:   systemUpdatesPipelineEvery,
			results: []*dashboard.GastownPipeline{first},
			steps: []step{
				{at: 0, wantReads: 1, want: first},
				{at: 30 * time.Second, wantReads: 1, want: first},
			},
		},
		{
			name:    "a nil read is cached inside the window",
			every:   systemUpdatesPipelineEvery,
			results: []*dashboard.GastownPipeline{nil},
			steps: []step{
				{at: 0, wantReads: 1, want: nil},
				{at: 30 * time.Second, wantReads: 1, want: nil},
			},
		},
		{
			name:    "a call at the caller's window's end reads again",
			every:   10 * time.Second,
			results: []*dashboard.GastownPipeline{first, second},
			steps: []step{
				{at: 0, wantReads: 1, want: first},
				{at: 10 * time.Second, wantReads: 2, want: second},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			at := start
			reads := 0
			read := func() *dashboard.GastownPipeline {
				require.Less(t, reads, len(tc.results), "the throttle read more often than the row allows")
				got := tc.results[reads]
				reads++
				return got
			}
			throttled := newSystemUpdatesPipeline(read, func() time.Time { return at }, tc.every)

			for i, s := range tc.steps {
				at = start.Add(s.at)
				got := throttled()
				assert.Equal(t, s.wantReads, reads, "step %d: underlying reads", i)
				if s.want == nil {
					assert.Nil(t, got, "step %d: result", i)
				} else {
					assert.Same(t, s.want, got, "step %d: result", i)
				}
			}
		})
	}
}
