package cmd

import (
	"github.com/steveyegge/gastown/internal/slot"
)

// containerGatePool is the town's container-gate pool (slot.PoolForTown).
// The resolution itself lives in the slot package so the daemon's landing
// worker builds the same pool the commands here do.
func containerGatePool(townRoot string) slot.Pool {
	return slot.PoolForTown(townRoot)
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
