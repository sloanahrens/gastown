package doctor

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
)

// RigsJSONCheck verifies that the rig registry loads and carries rig prefixes.
// An empty PrefixRegistry causes silent failures in session name parsing, crew
// cycling, and nudge routing.
type RigsJSONCheck struct {
	BaseCheck
}

// NewRigsJSONCheck creates a new rig registry prefix check.
func NewRigsJSONCheck() *RigsJSONCheck {
	return &RigsJSONCheck{
		BaseCheck: BaseCheck{
			CheckName:        "rigs-json",
			CheckDescription: "Check that the rig registry yields prefixes for PrefixRegistry",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run loads the rig registry through config.LoadRigsConfig, which follows
// mayor/rigs.json into the registry section of mayor/town.json on the two-file
// layout, and reports whether it carries any rig prefix.
func (c *RigsJSONCheck) Run(ctx *CheckContext) *CheckResult {
	rigsPath := filepath.Join(ctx.TownRoot, "mayor", "rigs.json")
	registry, err := config.LoadRigsConfig(rigsPath)
	if errors.Is(err, config.ErrNotFound) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "rig registry not found — PrefixRegistry is empty, session parsing broken",
			Details: []string{
				fmt.Sprintf("Looked in %s", config.SourcePathRel(ctx.TownRoot, rigsPath)),
				"Session cycling and nudge routing will fail silently",
			},
			FixHint: "Restore the rig registry or run 'gt doctor fix rigs-registry-exists'",
		}
	}
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "The rig registry does not load",
			Details: []string{err.Error()},
			FixHint: "Fix the file the error names by hand",
		}
	}

	prefixes := 0
	for _, entry := range registry.Rigs {
		if entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
			prefixes++
		}
	}
	if prefixes == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "rig registry has no rig prefixes — PrefixRegistry is empty, session parsing broken",
			Details: []string{
				"Session cycling and nudge routing will fail silently",
			},
			FixHint: "Set a beads prefix per rig ('gt rig list'), or restore the registry",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("rig registry provides %d rig prefix(es)", prefixes),
	}
}
