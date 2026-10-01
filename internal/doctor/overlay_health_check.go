package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/formula"
)

// OverlayHealthCheck verifies that formula overlay files reference valid step IDs.
// It scans the one overlay dir, <townRoot>/formula-overlays, and checks that every
// step_id in an overlay names a step of the formula as bd cooks it (extends,
// expansions and aspects applied), for a formula this binary ships. Fix mode
// removes stale step-override entries.
// A rig-level <rig>/formula-overlays dir is not read (gt-fd2cu.3), so one that
// still holds overlays is reported for an operator to move or delete.
type OverlayHealthCheck struct {
	FixableCheck

	// stepIDs is every step id of formula name as bd cooks it in townRoot;
	// cookedStepIDs (the bd on PATH) by default.
	stepIDs func(townRoot, name string) ([]string, error)
}

// NewOverlayHealthCheck creates a new overlay health check.
func NewOverlayHealthCheck() *OverlayHealthCheck {
	return &OverlayHealthCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "overlay-health",
				CheckDescription: "Check formula overlay step IDs are valid",
				CheckCategory:    CategoryConfig,
			},
		},
		stepIDs: cookedStepIDs,
	}
}

// cookedStepIDs cooks formula name with the bd on PATH from the town root
// (GT_ROOT the town, no overlay applied) and returns every step id in the
// tree, children included. bd is the one formula engine (gt-fd2cu.1.1).
func cookedStepIDs(townRoot, name string) ([]string, error) {
	env := beads.StripEnvKey(beads.StripEnvKey(os.Environ(), "GT_ROOT"), "BD_FORMULA_OVERLAY_DIR")
	env = append(env, "GT_ROOT="+townRoot)
	out, err := beads.NewPinned(filepath.Join(townRoot, ".beads"), beads.WithWorkDir(townRoot), beads.WithEnv(env)).Cook(name, nil)
	if err != nil {
		return nil, err
	}
	type step struct {
		ID       string `json:"id"`
		Children []step `json:"children"`
	}
	var tree struct {
		Steps []step `json:"steps"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, fmt.Errorf("cook %s: bd printed no step tree: %w", name, err)
	}
	var ids []string
	var walk func([]step)
	walk = func(steps []step) {
		for _, s := range steps {
			ids = append(ids, s.ID)
			walk(s.Children)
		}
	}
	walk(tree.Steps)
	return ids, nil
}

// overlayFile represents a discovered overlay file with its parsed contents.
type overlayFile struct {
	Path        string
	FormulaName string
	Overlay     *formula.FormulaOverlay
	ParseErr    error    // non-nil if TOML parsing failed
	StaleIDs    []string // step IDs that don't match any formula step
}

// Run checks all formula overlay files for stale step IDs and malformed TOML,
// and reports rig-level overlay dirs that are no longer read.
func (c *OverlayHealthCheck) Run(ctx *CheckContext) *CheckResult {
	result := c.runOverlayDir(ctx.TownRoot)
	unread := unreadRigOverlayFiles(ctx.TownRoot)
	if len(unread) == 0 {
		return result
	}
	if result.Status == StatusOK {
		result.Status = StatusWarning
		result.Message = fmt.Sprintf("%d rig-level overlay file(s) not read", len(unread))
		result.FixHint = "Move a still-wanted overlay into <townRoot>/formula-overlays; delete the rest"
	} else {
		result.Message += fmt.Sprintf("; %d rig-level overlay file(s) not read", len(unread))
	}
	for _, path := range unread {
		result.Details = append(result.Details, fmt.Sprintf("%s: rig-level overlay is not read (one overlay dir: %s)", path, formula.OverlayDir(ctx.TownRoot)))
	}
	return result
}

// runOverlayDir checks the overlay files in the one overlay dir.
func (c *OverlayHealthCheck) runOverlayDir(townRoot string) *CheckResult {
	files := c.scanOverlays(townRoot)

	if len(files) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "no overlay files found",
		}
	}

	var malformed, stale, ok int
	var details []string

	for _, f := range files {
		if f.ParseErr != nil {
			malformed++
			details = append(details, fmt.Sprintf("%s: malformed TOML: %v", f.Path, f.ParseErr))
			continue
		}
		if len(f.StaleIDs) > 0 {
			stale++
			details = append(details, fmt.Sprintf("%s: stale step IDs: %s",
				f.Path, strings.Join(f.StaleIDs, ", ")))
			continue
		}
		ok++
	}

	if malformed > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d malformed overlay(s)", malformed),
			Details: details,
			FixHint: "Fix malformed TOML in overlay files manually",
		}
	}

	if stale > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d overlay(s) with stale step IDs", stale),
			Details: details,
			FixHint: "Run 'gt doctor --fix' to remove stale step overrides",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("%d overlay(s) healthy", ok),
	}
}

// Fix removes stale step-override entries from overlay files.
// Malformed TOML files are left untouched (require manual intervention).
func (c *OverlayHealthCheck) Fix(ctx *CheckContext) error {
	files := c.scanOverlays(ctx.TownRoot)

	for _, f := range files {
		if f.ParseErr != nil || len(f.StaleIDs) == 0 {
			continue
		}

		// Build set of stale IDs for quick lookup.
		staleSet := make(map[string]bool, len(f.StaleIDs))
		for _, id := range f.StaleIDs {
			staleSet[id] = true
		}

		// Filter out stale overrides.
		var kept []formula.StepOverride
		for _, so := range f.Overlay.StepOverrides {
			if !staleSet[so.StepID] {
				kept = append(kept, so)
			}
		}

		if len(kept) == 0 {
			// All overrides were stale — remove the file entirely.
			if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing empty overlay %s: %w", f.Path, err)
			}
			continue
		}

		// Re-encode with remaining overrides.
		f.Overlay.StepOverrides = kept
		if err := writeOverlayFile(f.Path, f.Overlay); err != nil {
			return fmt.Errorf("writing overlay %s: %w", f.Path, err)
		}
	}

	return nil
}

// scanOverlays discovers and validates the overlay files in the one overlay dir.
func (c *OverlayHealthCheck) scanOverlays(townRoot string) []overlayFile {
	return c.scanOverlayDir(townRoot, formula.OverlayDir(townRoot))
}

// unreadRigOverlayFiles lists every file under a registered rig's
// formula-overlays dir, sorted. Nothing reads those dirs any more.
func unreadRigOverlayFiles(townRoot string) []string {
	var paths []string
	for rigName := range loadRigNames(filepath.Join(townRoot, "mayor", "rigs.json")) {
		dir := filepath.Join(townRoot, rigName, formula.OverlayDirName)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)
	return paths
}

// scanOverlayDir reads all .toml files in a formula-overlays directory,
// parses each one, and validates step IDs against the cooked formula.
func (c *OverlayHealthCheck) scanOverlayDir(townRoot, dir string) []overlayFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // directory doesn't exist — that's fine
	}

	var results []overlayFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		formulaName := strings.TrimSuffix(entry.Name(), ".toml")

		of := overlayFile{
			Path:        path,
			FormulaName: formulaName,
		}

		// Parse the overlay file.
		data, err := os.ReadFile(path) //nolint:gosec // G304: trusted overlay directory
		if err != nil {
			of.ParseErr = err
			results = append(results, of)
			continue
		}

		var overlay formula.FormulaOverlay
		if _, err := toml.Decode(string(data), &overlay); err != nil {
			of.ParseErr = fmt.Errorf("parsing TOML: %w", err)
			results = append(results, of)
			continue
		}
		of.Overlay = &overlay

		// A formula this binary does not ship has no valid step IDs.
		if _, err := formula.GetEmbeddedFormulaContent(formulaName); err != nil {
			// Formula not found in embedded binary — all step IDs are stale.
			for _, so := range overlay.StepOverrides {
				of.StaleIDs = append(of.StaleIDs, so.StepID)
			}
			results = append(results, of)
			continue
		}

		// Overlays apply to the cooked formula, so an overlay may target an
		// inherited or expanded step.
		ids, err := c.stepIDs(townRoot, formulaName)
		if err != nil {
			// bd cannot cook the formula: skip validation.
			results = append(results, of)
			continue
		}
		validIDs := make(map[string]bool, len(ids))
		for _, id := range ids {
			validIDs[id] = true
		}

		// Check each override step_id.
		for _, so := range overlay.StepOverrides {
			if !validIDs[so.StepID] {
				of.StaleIDs = append(of.StaleIDs, so.StepID)
			}
		}

		results = append(results, of)
	}

	return results
}

// writeOverlayFile encodes a FormulaOverlay back to TOML and writes it to disk.
func writeOverlayFile(path string, overlay *formula.FormulaOverlay) error {
	var buf strings.Builder
	encoder := toml.NewEncoder(&buf)
	if err := encoder.Encode(overlay); err != nil {
		return fmt.Errorf("encoding TOML: %w", err)
	}
	return os.WriteFile(path, []byte(buf.String()), 0644) //nolint:gosec // G306: overlay files are not sensitive
}
