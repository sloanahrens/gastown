package doctor

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/doltpause"
)

// DoltPauseCheck warns about a Dolt pause marker left behind (gt-8z769.2):
// one past its until (clients already ignore it, but its owner never lifted
// it), one written more than doltpause.MaxDuration ago or running further
// ahead than that, and one that cannot be parsed. An active, fresh pause is
// reported as OK with its message.
type DoltPauseCheck struct {
	BaseCheck

	// now is the clock; nil is time.Now.
	now func() time.Time
}

// NewDoltPauseCheck creates the stale pause marker check.
func NewDoltPauseCheck() *DoltPauseCheck {
	return &DoltPauseCheck{
		BaseCheck: BaseCheck{
			CheckName:        "dolt-pause-marker",
			CheckDescription: "Warn when daemon/dolt.pause is stale (past its until, or older than 24h) or unreadable",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run reads the marker and judges it.
func (c *DoltPauseCheck) Run(ctx *CheckContext) *CheckResult {
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	result := func(status CheckStatus, msg, hint string) *CheckResult {
		return &CheckResult{Name: c.Name(), Status: status, Message: msg, FixHint: hint, Category: c.CheckCategory}
	}
	const lift = "If no GC or backup is running, lift it: gt dolt unpause"

	m, err := doltpause.Read(ctx.TownRoot)
	if err != nil {
		return result(StatusWarning, fmt.Sprintf("Unreadable Dolt pause marker: %v", err), "Clients ignore it. "+lift)
	}
	if m == nil {
		return result(StatusOK, "Dolt not paused", "")
	}
	if why := m.Stale(now); why != "" {
		return result(StatusWarning, fmt.Sprintf("Stale Dolt pause marker (%s): %s", why, m.Message()), lift)
	}
	return result(StatusOK, m.Message(), "")
}
