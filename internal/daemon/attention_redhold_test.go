package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// TestRedHoldAttentionRaisesOneHighItemNamingBothKinds: two reasons raise one
// high item, keyed hold:red, naming both kinds, each detail and its cure
// beads; an absent file raises none.
func TestRedHoldAttentionRaisesOneHighItemNamingBothKinds(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: DefaultConfig(t.TempDir())}
	require.NoError(t, dispatch.WriteRedHold(d.config.TownRoot, dispatch.RedHoldRecord{
		Reasons: []dispatch.RedHoldReason{
			{Kind: dispatch.RedHoldMainRed, Detail: "main is red", CureBeads: []string{"gt-a", "gt-b"}},
			{Kind: dispatch.RedHoldSweepRed, Detail: "the sweep failed"},
		},
	}))

	items, err := (&attentionSources{d: d}).collectRedHold(context.Background())
	require.NoError(t, err)
	require.Len(t, items, 1, "one item however many reasons")
	assert.Equal(t, "hold:red", items[0].Key)
	assert.Equal(t, attention.KindRedHoldHeld, items[0].Kind)
	assert.Equal(t, attention.SeverityHigh, items[0].Severity)
	assert.Equal(t, "dispatch held by the red hold: main-red, sweep-red"+
		"; main-red: main is red (cure: gt-a, gt-b)"+
		"; sweep-red: the sweep failed", items[0].Summary)

	items, err = (&attentionSources{d: &Daemon{config: DefaultConfig(t.TempDir())}}).
		collectRedHold(context.Background())
	require.NoError(t, err)
	assert.Empty(t, items, "no hold file raises none")
}

// TestRedHoldAttentionReportsAnUnreadableFile: a file that cannot be parsed
// raises one high item keyed hold:red whose summary says the record is
// unreadable, rather than passing as no hold.
func TestRedHoldAttentionReportsAnUnreadableFile(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: DefaultConfig(t.TempDir())}
	path := dispatch.RedHoldPath(d.config.TownRoot)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	items, err := (&attentionSources{d: d}).collectRedHold(context.Background())
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "hold:red", items[0].Key)
	assert.Equal(t, attention.SeverityHigh, items[0].Severity)
	assert.Contains(t, items[0].Summary, "red hold unreadable")
}

// TestRedHoldAttentionIsCollected: the collectors table has an entry for the
// kind, so the item the collector builds is raised at all.
func TestRedHoldAttentionIsCollected(t *testing.T) {
	t.Parallel()
	var found int
	for _, c := range (&attentionSources{d: &Daemon{config: DefaultConfig(t.TempDir())}}).collectors() {
		if c.kind == attention.KindRedHoldHeld {
			found++
			assert.NotNil(t, c.collect)
		}
	}
	assert.Equal(t, 1, found, "the red hold kind has one collector")
}
