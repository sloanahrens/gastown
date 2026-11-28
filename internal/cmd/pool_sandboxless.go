package cmd

import (
	"fmt"
	"io"
)

// poolSandboxAgentRead answers whether an agent's polecat_pool seat is an
// openshell seat, the config.Seats read polecat.NewSandboxSeatProbe makes; what
// it cannot read answers false for every agent with one warning (gt-0hgh5v).
func poolSandboxAgentRead(townRoot string, warnOut io.Writer) func(agent string) bool {
	agents, err := dashSandboxSeatAgents(townRoot)
	if err != nil {
		if warnOut != nil {
			fmt.Fprintf(warnOut, "warning: cannot read the town's seats, so no seat counts as sandboxless: %v\n", err)
		}
		return func(string) bool { return false }
	}
	return func(agent string) bool { return agents[agent] }
}

// seatRunsNothing reports whether a seat's own read found nothing held and
// nobody working: a measured sessionless disposition (DeadRead), no live work
// in flight and an idle agent bead. A read that errored is a seat the pool
// cannot see, not one it may free, so it answers false (gt-n54w0f).
func seatRunsNothing(state poolSeatState, err error) bool {
	return err == nil && state.DeadRead && !state.HookHeld && !state.AgentWorking
}

// withoutSandboxlessSeats drops the sandbox seats whose sandbox is gone: a
// landing seat, or a dead-hooked seat that runsNothing (gt-p1og4z), with a
// sandbox agent and no record means no worker runs, so it should not hold a
// cap slot. A record, a read error (the pool cannot see the seat, not free it),
// a seat naming no rig or polecat, a host agent, a live session, a claim or a
// dead-hooked seat that runs something is kept; a nil isSandboxAgent or
// hasRecord returns all (gt-iew5w4).
func withoutSandboxlessSeats(sessions []poolSession, isSandboxAgent func(agent string) bool, hasRecord func(rig, polecat string) (bool, error)) []poolSession {
	if isSandboxAgent == nil || hasRecord == nil {
		return sessions
	}
	kept := sessions[:0:0]
	for _, s := range sessions {
		if (s.landing || s.deadHooked && s.runsNothing) && isSandboxAgent(s.agent) && s.rig != "" && s.polecat != "" {
			holds, err := hasRecord(s.rig, s.polecat)
			if err == nil && !holds {
				continue
			}
		}
		kept = append(kept, s)
	}
	return kept
}
