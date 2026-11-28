package cmd

import (
	"errors"
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/session"
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

// poolSandboxRecordRead is the hasRecord read withoutSandboxlessSeats takes: it
// answers whether the town's sandbox registry holds a record for a seat (gt-t9e6g1).
// It builds no session name from a rig prefix, whose guess for a rigs.json it cannot
// read would name another rig's absent record. Every entry it cannot read may be the
// seat's own, so a failed listing, an unreadable record, a listed name that is no
// session name, or a record naming no rig or polecat answers false with that error;
// a record found for the seat answers true, and only a registry read whole with no
// match, or a missing one, answers false with no error.
func poolSandboxRecordRead(townRoot string) func(rig, polecat string) (bool, error) {
	names, err := session.SandboxSessions(townRoot)
	if err != nil {
		return func(string, string) (bool, error) { return false, fmt.Errorf("listing the sandbox registry: %w", err) }
	}
	held := map[[2]string]bool{}
	var errs error
	fail := func(err error) { errs = errors.Join(errs, err) }
	for _, name := range names {
		if !session.ValidSandboxSessionName(name) {
			fail(fmt.Errorf("sandbox registry names %q, which is not a session name", name))
			continue
		}
		rec, ok, err := session.ReadSandboxRecord(townRoot, name)
		switch {
		case err != nil:
			fail(fmt.Errorf("reading sandbox record %s: %w", name, err))
		case !ok:
			// Removed since the listing: it names no seat's record now.
		case rec.Rig == "" || rec.Polecat == "":
			fail(fmt.Errorf("sandbox record %s names no rig or polecat", name))
		default:
			held[[2]string{rec.Rig, rec.Polecat}] = true
		}
	}
	return func(rig, polecat string) (bool, error) {
		if held[[2]string{rig, polecat}] {
			return true, nil
		}
		return false, errs
	}
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
