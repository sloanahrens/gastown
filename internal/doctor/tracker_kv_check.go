package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
)

// TrackerKVCheck reports the rigs routed to the tracker backend, whose
// key-value store is not built yet (plan task B4). A rig on the tracker reads
// no memories until the store exists, and its kv rows must be imported before
// its flip or they are lost. The check only reports; it has no auto-fix.
type TrackerKVCheck struct {
	BaseCheck
}

// NewTrackerKVCheck creates the tracker kv-store check.
func NewTrackerKVCheck() *TrackerKVCheck {
	return &TrackerKVCheck{
		BaseCheck: BaseCheck{
			CheckName:        "tracker-kv-store",
			CheckDescription: "Report rigs on the tracker whose key-value store is not built yet",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run lists the tracker-routed rigs. A route with no backend is Dolt.
func (c *TrackerKVCheck) Run(ctx *CheckContext) *CheckResult {
	res := &CheckResult{Name: c.Name(), Category: c.Category(), Status: StatusOK, Message: "no rig is routed to the tracker"}
	routes, err := beads.LoadRoutes(beads.GetTownBeadsPath(ctx.TownRoot))
	if err != nil {
		return res
	}
	var rigs []string
	for _, r := range routes {
		if r.Backend == beads.BackendTracker {
			rigs = append(rigs, r.Path)
		}
	}
	if len(rigs) == 0 {
		return res
	}
	res.Status = StatusWarning
	res.Message = fmt.Sprintf("%d rig(s) on the tracker have no key-value store yet (plan task B4)", len(rigs))
	res.Details = rigs
	res.FixHint = "import each rig's kv rows before its flip, or its memories are not read"
	return res
}
