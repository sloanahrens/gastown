package cmd

import (
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/slot"
)

// containerGatePool resolves the town's container-gate pool from
// settings/config.json (operational.container_gate). Defaults to the
// single-slot pool, so a town that never set it behaves exactly as before
// gt-yihz.
func containerGatePool(townRoot string) slot.Pool {
	cg := config.LoadOperationalConfig(townRoot).GetContainerGateConfig()
	return slot.Pool{Slots: cg.SlotsV(), ReservedForGate: cg.ReservedForGateV()}
}
