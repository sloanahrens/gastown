package doctor

import (
	"errors"
	"fmt"
	"os"

	"github.com/steveyegge/gastown/internal/hooks"
)

// HooksBaseCheck warns when ~/.gt/hooks-base.json is missing or unreadable.
// Without this file, gt hooks diff has no reference point and cannot detect
// drift when gt's default hook configuration changes after initial setup.
type HooksBaseCheck struct {
	FixableCheck

	// store is where hooks-base.json lives; nil is the hooks package's ~/.gt.
	store hooksBaseStore
}

// hooksBaseStore reads and writes the base hooks config.
type hooksBaseStore interface {
	LoadBase() (*hooks.HooksConfig, error)
	SaveBase(cfg *hooks.HooksConfig) error
	BasePath() string
}

// gtHooksBase is the base config under ~/.gt (or $GT_HOME/.gt).
type gtHooksBase struct{}

func (gtHooksBase) LoadBase() (*hooks.HooksConfig, error) { return hooks.LoadBase() }
func (gtHooksBase) SaveBase(cfg *hooks.HooksConfig) error { return hooks.SaveBase(cfg) }
func (gtHooksBase) BasePath() string                      { return hooks.BasePath() }

func (c *HooksBaseCheck) base() hooksBaseStore {
	if c.store != nil {
		return c.store
	}
	return gtHooksBase{}
}

// NewHooksBaseCheck creates a new hooks base config check.
func NewHooksBaseCheck() *HooksBaseCheck {
	return &HooksBaseCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "hooks-base-missing",
				CheckDescription: "Check that ~/.gt/hooks-base.json exists for drift detection",
				CheckCategory:    CategoryHooks,
			},
		},
	}
}

// Run checks whether hooks-base.json exists.
func (c *HooksBaseCheck) Run(ctx *CheckContext) *CheckResult {
	store := c.base()
	_, err := store.LoadBase()
	if err == nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("hooks-base.json present at %s", store.BasePath()),
		}
	}
	if !errors.Is(err, os.ErrNotExist) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("hooks-base.json is unreadable: %v", err),
			Details: []string{fmt.Sprintf("File: %s", store.BasePath())},
			FixHint: "Fix the file by hand; 'gt doctor fix' will not overwrite it",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "hooks-base.json is missing — gt hooks diff cannot detect drift",
		Details: []string{
			fmt.Sprintf("Expected at: %s", store.BasePath()),
			"Without this file, hooks sync works but drift detection is unavailable.",
		},
		FixHint: "Run 'gt doctor fix hooks-base-missing' or 'gt hooks base --show' to create it",
	}
}

// Fix creates hooks-base.json from current defaults.
func (c *HooksBaseCheck) Fix(ctx *CheckContext) error {
	store := c.base()
	_, err := store.LoadBase()
	if err == nil {
		return nil // already exists
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("hooks-base.json at %s is unreadable, fix it by hand (not overwriting): %w", store.BasePath(), err)
	}
	base := hooks.DefaultBase()
	if err := store.SaveBase(base); err != nil {
		return fmt.Errorf("creating hooks-base.json: %w", err)
	}
	fmt.Printf("  Created hooks-base.json at %s\n", store.BasePath())
	return nil
}

// CanFix returns true — we can auto-create the file.
func (c *HooksBaseCheck) CanFix() bool {
	return true
}
