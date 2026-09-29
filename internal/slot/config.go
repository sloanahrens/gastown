package slot

import "github.com/steveyegge/gastown/internal/config"

// PoolFromConfig is the pool the town's settings/config.json describes
// (operational.container_gate). Every caller that acquires or reports on the
// pool builds it here, so a knob added to the config block reaches all of
// them. A nil block is the defaults: one slot, nothing reserved, yielding to
// the gate on.
func PoolFromConfig(cg *config.ContainerGateThresholds) Pool {
	return Pool{
		Slots:           cg.SlotsV(),
		ReservedForGate: cg.ReservedForGateV(),
		YieldToGate:     cg.YieldToGateV(),
		MaxGateYield:    cg.MaxGateYieldD(),
	}
}
