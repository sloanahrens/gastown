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

// readGateSlotHolder reads the container-gate pool's current holder for the
// 'gt status' slot line, or nil when no slot is held or the read failed.
//
// Flock-only (StatusPoolLocksOnly): its caller runs on every refresh and this
// result carries nothing but the holder, so the full StatusPool's `docker ps`
// cross-check would be a subprocess whose output is never read (gt-a8kx).
func readGateSlotHolder(townRoot string) *SlotInfo {
	rep, err := slot.StatusPoolLocksOnly(townRoot, containerGatePool(townRoot))
	if err != nil || !rep.Held || rep.Owner == nil {
		return nil
	}
	return &SlotInfo{Role: rep.Owner.Role, PID: rep.Owner.PID, AcquiredAt: rep.Owner.AcquiredAt}
}
