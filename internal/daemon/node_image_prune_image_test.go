package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// TestNodePruneImageIsThePolecatReference: the prune Job runs the Polecat reference.
func TestNodePruneImageIsThePolecatReference(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	_, polecat := runningRefs()
	got, err := r.d.nodePruneImage()
	require.NoError(t, err)
	assert.Equal(t, polecat, got)
}

// TestNodePruneImageRefuses: no values file, an empty or a tag-only Polecat refuse.
func TestNodePruneImageRefuses(t *testing.T) {
	t.Parallel()
	daemon, _ := runningRefs()
	for _, raw := range []string{"", "daemon: " + daemon + "\npolecat: \ncommit: " + strings.Repeat("c", 40) + "\n",
		"daemon: " + daemon + "\npolecat: registry.test:5000/town/gastown-polecat:latest\ncommit: " + strings.Repeat("c", 40) + "\n"} {
		r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
		require.NoError(t, os.Remove(imagevalues.PathIn(filepath.Join(r.town, "gastown"))))
		if raw != "" {
			require.NoError(t, os.WriteFile(imagevalues.PathIn(filepath.Join(r.town, "gastown")), []byte(raw), 0o600))
		}
		got, err := r.d.nodePruneImage()
		require.Error(t, err)
		assert.Empty(t, got)
	}
}
