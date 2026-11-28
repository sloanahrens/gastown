package cmd

// seatRunsNothing reports whether a seat's own read found nothing held and
// nobody working: a measured sessionless disposition (DeadRead), no live work
// in flight and an idle agent bead. A read that errored is a seat the pool
// cannot see, not one it may free, so it answers false (gt-n54w0f).
func seatRunsNothing(state poolSeatState, err error) bool {
	return err == nil && state.DeadRead && !state.HookHeld && !state.AgentWorking
}

// withoutSandboxlessSeats drops the landing sandbox seats whose sandbox is
// gone: mid-landing, a sandbox agent, and no record means no worker runs, so
// it should not hold a cap slot. A record, a read error (the pool cannot see
// the seat, not free it), a seat naming no rig or polecat, a host agent or a
// non-landing seat is kept; a nil isSandboxAgent or hasRecord returns all (gt-iew5w4).
func withoutSandboxlessSeats(sessions []poolSession, isSandboxAgent func(agent string) bool, hasRecord func(rig, polecat string) (bool, error)) []poolSession {
	if isSandboxAgent == nil || hasRecord == nil {
		return sessions
	}
	kept := sessions[:0:0]
	for _, s := range sessions {
		if s.landing && isSandboxAgent(s.agent) && s.rig != "" && s.polecat != "" {
			holds, err := hasRecord(s.rig, s.polecat)
			if err == nil && !holds {
				continue
			}
		}
		kept = append(kept, s)
	}
	return kept
}
