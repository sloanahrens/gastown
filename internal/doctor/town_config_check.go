package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/daemon"
)

// TownConfigParseCheck reports a town config file (mayor/daemon.json,
// settings/config.json) that exists but does not parse. The town-running
// commands, agent session starts and the daemon refuse on the same check
// (daemon.CheckTownConfig, gt-fcxe9.10). It has no --fix: gt never rewrites
// an unparseable file, the operator fixes it by hand.
type TownConfigParseCheck struct {
	BaseCheck
}

// NewTownConfigParseCheck creates the town config parse check.
func NewTownConfigParseCheck() *TownConfigParseCheck {
	return &TownConfigParseCheck{
		BaseCheck: BaseCheck{
			CheckName:        "town-config-parse",
			CheckDescription: "Check that mayor/daemon.json and settings/config.json parse",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run reports each town config file that does not parse.
func (c *TownConfigParseCheck) Run(ctx *CheckContext) *CheckResult {
	err := daemon.CheckTownConfig(ctx.TownRoot)
	if err == nil {
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "Town config files parse"}
	}
	var details []string
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			details = append(details, e.Error())
		}
	} else {
		details = []string{err.Error()}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("%d town config file(s) do not parse; the town will not start", len(details)),
		Details: details,
		FixHint: "Fix the file by hand at the offset shown; gt never rewrites it",
	}
}
