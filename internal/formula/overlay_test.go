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

// TestFormulaOverlayValidate pins the one validator: the modes loadOverlayFile
// accepts, and the messages it returned before Validate was exported.
func TestFormulaOverlayValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		overlay FormulaOverlay
		wantErr string
	}{
		{
			name: "replace",
			overlay: FormulaOverlay{StepOverrides: []StepOverride{
				{StepID: "build", Mode: ModeReplace, Description: "d"},
			}},
		},
		{
			name: "append",
			overlay: FormulaOverlay{StepOverrides: []StepOverride{
				{StepID: "build", Mode: ModeAppend, Description: "d"},
			}},
		},
		{
			name: "skip",
			overlay: FormulaOverlay{StepOverrides: []StepOverride{
				{StepID: "build", Mode: ModeSkip},
			}},
		},
		{
			name: "typo in mode",
			overlay: FormulaOverlay{StepOverrides: []StepOverride{
				{StepID: "build", Mode: OverrideMode("apend"), Description: "d"},
			}},
			wantErr: `step-overrides[0] (step_id="build"): invalid mode "apend" (must be replace, append, or skip)`,
		},
		{
			name: "empty step_id",
			overlay: FormulaOverlay{StepOverrides: []StepOverride{
				{Mode: ModeReplace, Description: "d"},
			}},
			wantErr: "step-overrides[0]: step_id is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.overlay.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.wantErr)
		})
	}
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
