package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
)

// RigNameMismatchCheck detects when a rig's config.json has a name or beads
// prefix that doesn't match the authoritative sources (directory name and
// rigs.json registry respectively).
type RigNameMismatchCheck struct {
	FixableCheck
}

// NewRigNameMismatchCheck creates a new rig name mismatch check.
func NewRigNameMismatchCheck() *RigNameMismatchCheck {
	return &RigNameMismatchCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "rig-name-mismatch",
				CheckDescription: "Check rig config.json name and prefix match directory and registry",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// rigConfigLocal is the read-only view of config.json the checks here need.
// Fix edits the raw JSON map instead, so keys this type does not model survive
// a fix (gt-ay3b0).
type rigConfigLocal struct {
	Type    string               `json:"type"`
	Version int                  `json:"version"`
	Name    string               `json:"name"`
	Beads   *rigConfigBeadsLocal `json:"beads,omitempty"`
}

type rigConfigBeadsLocal struct {
	Prefix string `json:"prefix"`
}

func rigConfigPath(rigPath string) string {
	return filepath.Join(rigPath, "config.json")
}

func loadRigConfigLocal(rigPath string) (*rigConfigLocal, error) {
	data, err := os.ReadFile(rigConfigPath(rigPath))
	if err != nil {
		return nil, err
	}
	var cfg rigConfigLocal
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// loadRigConfigRaw reads config.json as a key-to-value map so Fix can replace a
// single key and leave every other key, including keys doctor does not model,
// exactly as it found it. Decoding into a struct loses those keys on the way
// back out (gt-ay3b0): merge_queue, polecat_pool_size, polecat_names, push_url,
// upstream_url, witness and beads.repo all live in a rig's config.json.
func loadRigConfigRaw(rigPath string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(rigConfigPath(rigPath))
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("%s is not a JSON object", rigConfigPath(rigPath))
	}
	return raw, nil
}

// saveRigConfigRaw writes the map back with values untouched. json.Marshal sorts
// top-level keys, so a rewrite may reorder the file; every value survives.
func saveRigConfigRaw(rigPath string, raw map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(rigConfigPath(rigPath), data, 0644)
}

// Run checks for name/prefix mismatches between config.json and the
// authoritative sources (directory name and rigs.json).
func (c *RigNameMismatchCheck) Run(ctx *CheckContext) *CheckResult {
	if ctx.RigName == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rig specified (skipped)",
		}
	}

	rigPath := ctx.RigPath()
	cfg, err := loadRigConfigLocal(rigPath)
	if err != nil {
		// Missing or unreadable config.json — we could not verify name/prefix
		// match, not that they do match.
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "No config.json found (skipped)",
			Details: []string{err.Error()},
		}
	}

	var details []string

	// Check 1: config.Name vs directory name
	if cfg.Name != ctx.RigName {
		details = append(details, fmt.Sprintf(
			"config.json name is %q but directory is %q", cfg.Name, ctx.RigName))
	}

	// Check 2: config beads prefix vs rigs.json prefix
	rigsPath := filepath.Join(ctx.TownRoot, "mayor", "rigs.json")
	rigsConfig, rigsErr := config.LoadRigsConfig(rigsPath)
	if rigsErr == nil && cfg.Beads != nil && cfg.Beads.Prefix != "" {
		if entry, ok := rigsConfig.Rigs[ctx.RigName]; ok && entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
			if cfg.Beads.Prefix != entry.BeadsConfig.Prefix {
				details = append(details, fmt.Sprintf(
					"config.json beads prefix is %q but rigs.json says %q",
					cfg.Beads.Prefix, entry.BeadsConfig.Prefix))
			}
		}
	}

	if len(details) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Rig config name and prefix match directory and registry",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d rig config mismatch(es) found", len(details)),
		Details: details,
		FixHint: "Run 'gt doctor fix rig-name-mismatch' to update config.json to match directory name and registry prefix",
	}
}

// Fix updates config.json to match the directory name and rigs.json prefix. It
// replaces those two values and nothing else: a fix for a renamed rig must not
// cost the rig its merge queue or pool settings (gt-ay3b0).
func (c *RigNameMismatchCheck) Fix(ctx *CheckContext) error {
	if ctx.RigName == "" {
		return nil
	}

	rigPath := ctx.RigPath()
	cfg, err := loadRigConfigLocal(rigPath)
	if err != nil {
		return nil // Nothing to fix
	}
	raw, err := loadRigConfigRaw(rigPath)
	if err != nil {
		return err
	}

	modified := false

	// Fix name to match directory
	if cfg.Name != ctx.RigName {
		name, err := json.Marshal(ctx.RigName)
		if err != nil {
			return err
		}
		raw["name"] = name
		modified = true
	}

	// Fix prefix to match rigs.json
	rigsPath := filepath.Join(ctx.TownRoot, "mayor", "rigs.json")
	rigsConfig, rigsErr := config.LoadRigsConfig(rigsPath)
	if rigsErr == nil && cfg.Beads != nil && cfg.Beads.Prefix != "" {
		if entry, ok := rigsConfig.Rigs[ctx.RigName]; ok && entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
			if cfg.Beads.Prefix != entry.BeadsConfig.Prefix {
				if err := setRigConfigBeadsPrefix(raw, entry.BeadsConfig.Prefix); err != nil {
					return err
				}
				modified = true
			}
		}
	}

	if modified {
		return saveRigConfigRaw(rigPath, raw)
	}

	return nil
}

// setRigConfigBeadsPrefix replaces beads.prefix in the raw config, keeping the
// object's other keys (such as beads.repo) as they were.
func setRigConfigBeadsPrefix(raw map[string]json.RawMessage, prefix string) error {
	beads := map[string]json.RawMessage{}
	if data, ok := raw["beads"]; ok {
		if err := json.Unmarshal(data, &beads); err != nil {
			return err
		}
	}
	value, err := json.Marshal(prefix)
	if err != nil {
		return err
	}
	beads["prefix"] = value
	data, err := json.Marshal(beads)
	if err != nil {
		return err
	}
	raw["beads"] = data
	return nil
}
