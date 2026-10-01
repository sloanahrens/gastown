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
