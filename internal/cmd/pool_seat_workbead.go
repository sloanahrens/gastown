package cmd

import "github.com/steveyegge/gastown/internal/seatfacts"

// poolSeatWorkBead returns the work bead a sessionless seat holds: the intent
// record's work_bead, or the agent bead's hook_bead for a seat whose record
// predates the intent fields. A record that could not be read is an error, not
// the bead.
func poolSeatWorkBead(facts seatfacts.Facts, readErr error, beadHook string) (string, error) {
	if readErr != nil {
		return "", readErr
	}
	if facts.TurnState == "" && facts.WorkBead == "" {
		return beadHook, nil
	}
	return facts.WorkBead, nil
}
