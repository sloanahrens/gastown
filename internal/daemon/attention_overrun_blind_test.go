package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
)

// TestOverrunBlindAttention: the collector list's KindOverrunBlind entry raises
// the blind guard's one item at a streak of three, names its seats and first
// error, and raises none at zero or with no daemon (gt-0vl1d4).
func TestOverrunBlindAttention(t *testing.T) {
	t.Parallel()
	collect := func(t *testing.T, s *attentionSources) []attention.Item {
		t.Helper()
		for _, c := range s.collectors() {
			if c.kind == attention.KindOverrunBlind {
				items, err := c.collect(context.Background())
				require.NoError(t, err)
				return items
			}
		}
		t.Fatalf("collectors have no KindOverrunBlind entry")
		return nil
	}

	t.Run("three blind ticks raise the one item", func(t *testing.T) {
		d := &Daemon{}
		for i := 0; i < 3; i++ {
			d.overrunKeptReads.setBlind(2, "bd show: gt-aaa11 unavailable")
		}
		items := collect(t, &attentionSources{d: d})
		require.Len(t, items, 1)
		assert.Equal(t, "overrun-guard:blind", items[0].Key)
		assert.Equal(t, attention.KindOverrunBlind, items[0].Kind)
		assert.Contains(t, items[0].Summary, "3 ticks in a row")
		assert.Contains(t, items[0].Summary, "bd show: gt-aaa11 unavailable")
	})

	t.Run("a streak below three raises none", func(t *testing.T) {
		assert.Empty(t, collect(t, &attentionSources{d: &Daemon{}}))
	})

	t.Run("no daemon raises nothing", func(t *testing.T) {
		assert.Empty(t, collect(t, &attentionSources{}))
	})
}
