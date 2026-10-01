package formula

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// OverrideMode specifies how a step override is applied.
type OverrideMode string

const (
	// ModeReplace swaps the step description entirely.
	ModeReplace OverrideMode = "replace"
	// ModeAppend adds text after the existing step description.
	ModeAppend OverrideMode = "append"
	// ModeSkip removes the step from the formula.
	ModeSkip OverrideMode = "skip"
)

// StepOverride describes a single step override from an overlay file.
type StepOverride struct {
	StepID      string       `toml:"step_id"`
	Mode        OverrideMode `toml:"mode"`
	Description string       `toml:"description"`
}

// FormulaOverlay holds the parsed overlay for a formula.
type FormulaOverlay struct {
	StepOverrides []StepOverride `toml:"step-overrides"`
}

// OverlayDirName is the one overlay directory, directly under the town root
// (gt-fd2cu.3). A rig-level <rig>/formula-overlays directory is not read.
const OverlayDirName = "formula-overlays"

// OverlayDir returns the town's one overlay directory.
func OverlayDir(townRoot string) string {
	return filepath.Join(townRoot, OverlayDirName)
}

// OverlayPath returns where the overlay for formulaName lives.
func OverlayPath(townRoot, formulaName string) string {
	return filepath.Join(OverlayDir(townRoot), formulaName+".toml")
}

// LoadFormulaOverlay reads <townRoot>/formula-overlays/<formulaName>.toml.
// If the file does not exist, returns nil with no error.
func LoadFormulaOverlay(formulaName, townRoot string) (*FormulaOverlay, error) {
	path := OverlayPath(townRoot, formulaName)
	overlay, err := loadOverlayFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading overlay %s: %w", path, err)
	}
	return overlay, nil
}

// loadOverlayFile reads and parses a single overlay TOML file.
// Returns (nil, nil) if the file does not exist.
func loadOverlayFile(path string) (*FormulaOverlay, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from trusted overlay directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var overlay FormulaOverlay
	if _, err := toml.Decode(string(data), &overlay); err != nil {
		return nil, fmt.Errorf("parsing overlay TOML: %w", err)
	}

	// Validate overrides.
	for i, so := range overlay.StepOverrides {
		if so.StepID == "" {
			return nil, fmt.Errorf("step-overrides[%d]: step_id is required", i)
		}
		switch so.Mode {
		case ModeReplace, ModeAppend, ModeSkip:
			// valid
		default:
			return nil, fmt.Errorf("step-overrides[%d] (step_id=%q): invalid mode %q (must be replace, append, or skip)", i, so.StepID, so.Mode)
		}
	}

	return &overlay, nil
}
