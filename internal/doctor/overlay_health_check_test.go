package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewOverlayHealthCheck(t *testing.T) {
	t.Parallel()
	check := NewOverlayHealthCheck()
	assert.Equal(t, "overlay-health", check.Name())
	assert.True(t, check.CanFix())
	assert.Equal(t, CategoryConfig, check.Category())
}

func TestOverlayHealthCheck_NoOverlays(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	check := overlayCheckWithSteps(polecatWorkSteps)
	result := check.Run(&CheckContext{TownRoot: tmpDir})

	assert.Equal(t, StatusOK, result.Status)
	assert.Contains(t, result.Message, "no overlay files")
}

func TestOverlayHealthCheck_HealthyOverlay(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	// Create a town-level overlay referencing real step IDs from mol-polecat-work.
	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	content := "[[step-overrides]]\nstep_id = \"load-context\"" + "\nmode = \"append\"\ndescription = \"Extra instructions\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "mol-polecat-work.toml"), []byte(content), 0o644))

	check := overlayCheckWithSteps(polecatWorkSteps)
	result := check.Run(&CheckContext{TownRoot: tmpDir})

	assert.Equal(t, StatusOK, result.Status)
	assert.Contains(t, result.Message, "healthy")
}

func TestOverlayHealthCheck_StaleStepID(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	content := `[[step-overrides]]
step_id = "nonexistent-step-from-old-binary"
mode = "replace"
description = "This won't match anything"
`
	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "mol-polecat-work.toml"), []byte(content), 0o644))

	check := overlayCheckWithSteps(polecatWorkSteps)
	result := check.Run(&CheckContext{TownRoot: tmpDir})

	assert.Equal(t, StatusWarning, result.Status)
	assert.Contains(t, result.Message, "stale")
	require.NotEmpty(t, result.Details)
	assert.Contains(t, result.Details[0], "nonexistent-step-from-old-binary")
	assert.NotEmpty(t, result.FixHint)
}

func TestOverlayHealthCheck_MalformedTOML(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "mol-polecat-work.toml"), []byte("[[invalid"), 0o644))

	check := overlayCheckWithSteps(polecatWorkSteps)
	result := check.Run(&CheckContext{TownRoot: tmpDir})

	assert.Equal(t, StatusError, result.Status)
	assert.Contains(t, result.Message, "malformed")
}

// TestOverlayHealthCheck_RigLevelDirIsReportedNotRead: there is one overlay
// dir (gt-fd2cu.3). A rig-level overlay, and any .bak beside it, is reported as
// unread, and --fix leaves it for an operator.
func TestOverlayHealthCheck_RigLevelDirIsReportedNotRead(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	rigDir := filepath.Join(tmpDir, "testrig", "formula-overlays")
	require.NoError(t, os.MkdirAll(rigDir, 0o755))

	content := `[[step-overrides]]
step_id = "old-removed-step"
mode = "skip"
`
	overlay := filepath.Join(rigDir, "mol-polecat-work.toml")
	bak := filepath.Join(rigDir, "mol-polecat-work.toml.pre-x.bak")
	require.NoError(t, os.WriteFile(overlay, []byte(content), 0o644))
	require.NoError(t, os.WriteFile(bak, []byte(content), 0o644))

	check := overlayCheckWithSteps(polecatWorkSteps)
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	assert.Equal(t, StatusWarning, result.Status)
	assert.Contains(t, result.Message, "2 rig-level overlay file(s) not read")
	require.Len(t, result.Details, 2)
	assert.Contains(t, result.Details[0], overlay)
	assert.Contains(t, result.Details[1], bak)

	require.NoError(t, check.Fix(ctx))
	assert.FileExists(t, overlay)
	assert.FileExists(t, bak)
}

func TestOverlayHealthCheck_UnknownFormula(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	content := `[[step-overrides]]
step_id = "some-step"
mode = "replace"
description = "Override for non-existent formula"
`
	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "nonexistent-formula.toml"), []byte(content), 0o644))

	check := overlayCheckWithSteps(polecatWorkSteps)
	result := check.Run(&CheckContext{TownRoot: tmpDir})

	// All step IDs should be reported as stale since the formula doesn't exist.
	assert.Equal(t, StatusWarning, result.Status)
	assert.Contains(t, result.Details[0], "some-step")
}

// TestOverlayHealthCheck_UncookableFormulaFailsClosed: an overlay on a shipped
// formula bd cannot cook has unverified step IDs, so the check reports it as
// not verified (never healthy) and --fix leaves the file alone.
func TestOverlayHealthCheck_UncookableFormulaFailsClosed(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))
	content := "[[step-overrides]]\nstep_id = \"synthesis\"\nmode = \"skip\"\n"
	overlayPath := filepath.Join(overlayDir, "code-review.toml")
	require.NoError(t, os.WriteFile(overlayPath, []byte(content), 0o644))

	check := overlayCheckWithSteps(polecatWorkSteps) // code-review does not cook
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)
	assert.Equal(t, StatusSkipped, result.Status)
	require.NotEmpty(t, result.Details)
	assert.Contains(t, result.Details[0], "bd cannot cook code-review")

	require.NoError(t, check.Fix(ctx))
	data, err := os.ReadFile(overlayPath)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
}

func TestOverlayHealthCheck_Fix_RemovesStaleEntries(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	// Create overlay with one valid and one stale override.
	content := "[[step-overrides]]\nstep_id = \"load-context\"" + "\nmode = \"append\"\ndescription = \"Keep this\"\n\n" +
		"[[step-overrides]]\nstep_id = \"ghost-step\"\nmode = \"skip\"\n"
	overlayPath := filepath.Join(overlayDir, "mol-polecat-work.toml")
	require.NoError(t, os.WriteFile(overlayPath, []byte(content), 0o644))

	check := overlayCheckWithSteps(polecatWorkSteps)
	ctx := &CheckContext{TownRoot: tmpDir}

	// Verify it's warning before fix.
	result := check.Run(ctx)
	assert.Equal(t, StatusWarning, result.Status)

	// Run fix.
	require.NoError(t, check.Fix(ctx))

	// Re-check — should be healthy now.
	result = check.Run(ctx)
	assert.Equal(t, StatusOK, result.Status)

	// File should still exist (has valid entries).
	_, err := os.Stat(overlayPath)
	assert.NoError(t, err)
}

func TestOverlayHealthCheck_Fix_RemovesEmptyFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	// All overrides are stale.
	content := `[[step-overrides]]
step_id = "ghost-step-1"
mode = "skip"

[[step-overrides]]
step_id = "ghost-step-2"
mode = "replace"
description = "Also stale"
`
	overlayPath := filepath.Join(overlayDir, "mol-polecat-work.toml")
	require.NoError(t, os.WriteFile(overlayPath, []byte(content), 0o644))

	check := overlayCheckWithSteps(polecatWorkSteps)
	ctx := &CheckContext{TownRoot: tmpDir}

	require.NoError(t, check.Fix(ctx))

	// File should be removed entirely.
	_, err := os.Stat(overlayPath)
	assert.True(t, os.IsNotExist(err), "overlay file should be removed when all entries are stale")

	// Re-check — should be OK (no overlays).
	result := check.Run(ctx)
	assert.Equal(t, StatusOK, result.Status)
}

func TestOverlayHealthCheck_Fix_SkipsMalformed(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	setupRigsJSON(t, tmpDir, []string{"testrig"})

	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	overlayPath := filepath.Join(overlayDir, "mol-polecat-work.toml")
	require.NoError(t, os.WriteFile(overlayPath, []byte("[[invalid"), 0o644))

	check := overlayCheckWithSteps(polecatWorkSteps)
	ctx := &CheckContext{TownRoot: tmpDir}

	// Fix should not error — just skips malformed files.
	require.NoError(t, check.Fix(ctx))

	// File should still exist (untouched).
	data, err := os.ReadFile(overlayPath)
	require.NoError(t, err)
	assert.Equal(t, "[[invalid", string(data))
}

// --- helpers ---

// polecatWorkSteps is bd's cooked step ids for mol-polecat-work.
var polecatWorkSteps = map[string][]string{"mol-polecat-work": {"load-context", "branch-setup", "implement"}}

// overlayCheckWithSteps is the check with bd's cook answered from steps: a
// formula not in it fails to cook.
func overlayCheckWithSteps(steps map[string][]string) *OverlayHealthCheck {
	c := NewOverlayHealthCheck()
	c.stepIDs = func(_, name string) ([]string, error) {
		ids, ok := steps[name]
		if !ok {
			return nil, errors.New("bd cannot cook " + name)
		}
		return ids, nil
	}
	return c
}
