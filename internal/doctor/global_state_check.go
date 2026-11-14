// ABOUTME: Doctor check for Gas Town global state configuration.
// ABOUTME: Validates that the global state file is readable and consistent.

package doctor

import (
	"os"

	"github.com/steveyegge/gastown/internal/state"
)

type GlobalStateCheck struct {
	BaseCheck
	// load reads the state file; tests supply a stub so the check stays off
	// the process environment.
	load func() (*state.State, error)
}

func NewGlobalStateCheck() *GlobalStateCheck {
	return &GlobalStateCheck{
		BaseCheck: BaseCheck{
			CheckName:        "global-state",
			CheckDescription: "Validates Gas Town global state",
			CheckCategory:    CategoryCore,
		},
		load: state.Load,
	}
}

func (c *GlobalStateCheck) Run(ctx *CheckContext) *CheckResult {
	result := &CheckResult{
		Name:   c.Name(),
		Status: StatusOK,
	}

	var details []string
	var warnings []string

	s, err := c.load()
	if err != nil {
		if os.IsNotExist(err) {
			result.Message = "Global state not initialized"
			result.FixHint = "Run from the town root: gt install . --force"
			result.Status = StatusWarning
			return result
		}
		result.Message = "Cannot read global state"
		result.Details = []string{err.Error()}
		result.Status = StatusError
		return result
	}

	if s.Enabled {
		details = append(details, "Gas Town: enabled")
	} else {
		details = append(details, "Gas Town: disabled")
		warnings = append(warnings, "Gas Town is disabled globally")
	}

	if s.Version != "" {
		details = append(details, "Version: "+s.Version)
	}

	if s.MachineID != "" {
		details = append(details, "Machine ID: "+s.MachineID)
	}

	result.Details = details

	if len(warnings) > 0 {
		result.Status = StatusWarning
		result.Message = warnings[0]
		result.FixHint = "Run from the town root: gt install . --force"
	} else {
		result.Message = "Global state healthy"
	}

	return result
}
