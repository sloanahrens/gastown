package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
)

// rolledBackState is a run that ended failed after its install put the daemon
// back on the previous pair because check failed.
func rolledBackState(check string) update.State {
	return update.State{
		RunID:     "run-1",
		StartedAt: updateTriggerTestTime.Add(-10 * time.Minute),
		EndedAt:   updateTriggerTestTime.Add(-5 * time.Minute),
		Target:    update.Target{SHA: "1111111111111111111111111111111111111111"},
		Outcome:   update.OutcomeFailed,
		Rollback: &update.RollbackRecord{
			Check: check,
			Restored: update.ImagePair{
				Daemon:  "registry.test/gt@sha256:" + strings.Repeat("a", 64),
				Polecat: "registry.test/pc@sha256:" + strings.Repeat("b", 64),
				Commit:  "2222222222222222222222222222222222222222",
			},
		},
	}
}

// TestUpdateRolledBackRaisesAHighItemNamingTheCheck holds the item to what the
// run recorded: a high item keyed by the target sha, whose summary names the
// check that failed and the commit the daemon was restored to.
func TestUpdateRolledBackRaisesAHighItemNamingTheCheck(t *testing.T) {
	t.Parallel()
	d, _, _, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{Enabled: true})
	store := &update.Store{Dir: update.Dir(d.config.TownRoot)}
	require.NoError(t, store.Write(rolledBackState(update.CheckRegistryTag)))

	s := &attentionSources{d: d}
	items, err := s.collectUpdateRolledBack(context.Background())
	require.NoError(t, err)
	require.Len(t, items, 1, "a rolled-back run raises one item")
	it := items[0]
	assert.Equal(t, attention.KindUpdateRolledBack, it.Kind)
	assert.Equal(t, attention.SeverityHigh, it.Severity)
	assert.Equal(t, "update-rolled-back:111111111111", it.Key, "the item is keyed by the target sha")
	assert.Contains(t, it.Summary, "the registry-tag check failed", "the summary names the failed check")
	assert.Contains(t, it.Summary, "restored the daemon to registry.test/gt@sha256:")
	assert.Contains(t, it.Summary, "on commit 222222222222", "the summary names the commit restored")
}

// TestUpdateRolledBackRaisesNothingWhenNoRollbackIsRecorded holds the collector
// to the absence it must read as no item: no run has written state, or the last
// run did not roll back.
func TestUpdateRolledBackRaisesNothingWhenNoRollbackIsRecorded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state *update.State
	}{
		{name: "no run has written state"},
		{name: "the last run installed clean", state: &update.State{
			RunID:     "run-2",
			StartedAt: updateTriggerTestTime.Add(-time.Minute),
			Outcome:   update.OutcomeOK,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{Enabled: true})
			if tt.state != nil {
				require.NoError(t, (&update.Store{Dir: update.Dir(d.config.TownRoot)}).Write(*tt.state))
			}
			items, err := (&attentionSources{d: d}).collectUpdateRolledBack(context.Background())
			require.NoError(t, err)
			assert.Empty(t, items)
		})
	}
}

// TestUpdateRolledBackFailsOnAnUnreadableState holds an unreadable state file to
// a failed collection, so the previous tick's item is kept rather than a read
// failure passing as no rollback.
func TestUpdateRolledBackFailsOnAnUnreadableState(t *testing.T) {
	t.Parallel()
	d, _, _, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{Enabled: true})
	dir := update.Dir(d.config.TownRoot)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600))

	items, err := (&attentionSources{d: d}).collectUpdateRolledBack(context.Background())
	assert.Error(t, err, "an unreadable state must fail the collection")
	assert.Empty(t, items)
}

// TestUpdateRolledBackIsCollected holds the collector to the production table: an
// entry of its kind that keeps only its own items when its read fails. Without
// that entry the item is never raised, whatever the collector would find.
func TestUpdateRolledBackIsCollected(t *testing.T) {
	t.Parallel()
	d, _, _, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{Enabled: true})
	var found []attentionCollector
	for _, c := range (&attentionSources{d: d}).collectors() {
		if c.kind == attention.KindUpdateRolledBack {
			found = append(found, c)
		}
	}
	require.Len(t, found, 1, "the rolled-back kind has one collector")
	assert.NotNil(t, found[0].collect)
	assert.NotNil(t, found[0].owns, "a failed read must keep only this kind's items")
}

// TestUpdateRolledBackOwnsOnlyItsOwnItems holds the owns predicate to the kind it
// collects, so a failed collection keeps the previous tick's items of this kind
// and nothing else.
func TestUpdateRolledBackOwnsOnlyItsOwnItems(t *testing.T) {
	t.Parallel()
	own := updateRolledBackItem(rolledBackState(update.CheckRollout))
	assert.True(t, updateRolledBackOwned(own))
	assert.False(t, updateRolledBackOwned(attention.Item{Kind: attention.KindRedMain, Key: "update-rolled-back:1"}))
	assert.False(t, updateRolledBackOwned(attention.Item{Kind: attention.KindUpdateRolledBack, Key: "other:1"}))
}
