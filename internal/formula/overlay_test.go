package formula

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFormulaOverlay_NoFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	overlay, err := LoadFormulaOverlay("mol-polecat-work", tmpDir)
	require.NoError(t, err)
	assert.Nil(t, overlay)
}

func TestLoadFormulaOverlay_TownLevel(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	content := `
[[step-overrides]]
step_id = "submit-review"
mode = "replace"
description = "Custom submission instructions"
`
	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "mol-polecat-work.toml"), []byte(content), 0o644))

	overlay, err := LoadFormulaOverlay("mol-polecat-work", tmpDir)
	require.NoError(t, err)
	require.NotNil(t, overlay)
	require.Len(t, overlay.StepOverrides, 1)
	assert.Equal(t, "submit-review", overlay.StepOverrides[0].StepID)
	assert.Equal(t, ModeReplace, overlay.StepOverrides[0].Mode)
	assert.Equal(t, "Custom submission instructions", overlay.StepOverrides[0].Description)
}

// TestLoadFormulaOverlay_RigLevelDirIsNotRead: there is one overlay dir
// (gt-fd2cu.3); a <rig>/formula-overlays file changes nothing.
func TestLoadFormulaOverlay_RigLevelDirIsNotRead(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "gastown", "formula-overlays")
	require.NoError(t, os.MkdirAll(rigDir, 0o755))

	content := `
[[step-overrides]]
step_id = "build"
mode = "append"
description = "Also run integration tests"
`
	require.NoError(t, os.WriteFile(filepath.Join(rigDir, "mol-polecat-work.toml"), []byte(content), 0o644))

	overlay, err := LoadFormulaOverlay("mol-polecat-work", tmpDir)
	require.NoError(t, err)
	assert.Nil(t, overlay)
}

func TestLoadFormulaOverlay_InvalidMode(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	content := `
[[step-overrides]]
step_id = "build"
mode = "delete"
description = "Bad mode"
`
	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "test-formula.toml"), []byte(content), 0o644))

	overlay, err := LoadFormulaOverlay("test-formula", tmpDir)
	assert.Error(t, err)
	assert.Nil(t, overlay)
	assert.Contains(t, err.Error(), `invalid mode "delete"`)
}

func TestLoadFormulaOverlay_MissingStepID(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	content := `
[[step-overrides]]
mode = "replace"
description = "No step_id"
`
	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "test-formula.toml"), []byte(content), 0o644))

	overlay, err := LoadFormulaOverlay("test-formula", tmpDir)
	assert.Error(t, err)
	assert.Nil(t, overlay)
	assert.Contains(t, err.Error(), "step_id is required")
}

func TestLoadFormulaOverlay_InvalidTOML(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "formula-overlays")
	require.NoError(t, os.MkdirAll(overlayDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(overlayDir, "test-formula.toml"), []byte("[[invalid"), 0o644))

	overlay, err := LoadFormulaOverlay("test-formula", tmpDir)
	assert.Error(t, err)
	assert.Nil(t, overlay)
}
