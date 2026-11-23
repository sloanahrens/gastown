package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
