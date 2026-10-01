//go:build integration

package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegrationOverlayHealthCheck_CookedSteps guards gt-0z1lt through the
// real bd cook: mol-doc-audit carries only its delta, so load-context
// (inherited from mol-polecat-work) and audit (from its doc-audit-slice
// expansion) are not in its file. Overlays apply to the cooked formula, so
// both are valid targets; implement is replaced by the expansion, so it is
// stale.
func TestIntegrationOverlayHealthCheck_CookedSteps(t *testing.T) {
	town := t.TempDir()
	setupRigsJSON(t, town, []string{"testrig"})
	_, err := formula.ProvisionFormulas(town)
	require.NoError(t, err)
	overlayDir := formula.OverlayDir(town)
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	valid := "[[step-overrides]]\nstep_id = \"load-context\"\nmode = \"append\"\ndescription = \"Extra context\"\n\n" +
		"[[step-overrides]]\nstep_id = \"audit\"\nmode = \"append\"\ndescription = \"Extra audit rule\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "mol-doc-audit.toml"), []byte(valid), 0o644))

	check := NewOverlayHealthCheck()
	result := check.Run(&CheckContext{TownRoot: town})
	assert.Equal(t, StatusOK, result.Status, "details: %v", result.Details)

	stale := "[[step-overrides]]\nstep_id = \"implement\"\nmode = \"skip\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "mol-doc-audit.toml"), []byte(stale), 0o644))
	result = check.Run(&CheckContext{TownRoot: town})
	assert.Equal(t, StatusWarning, result.Status)
	require.NotEmpty(t, result.Details)
	assert.Contains(t, result.Details[0], "implement")
}
