package daemon

import (
	"github.com/steveyegge/gastown/internal/jev/testsel"
	"github.com/steveyegge/gastown/internal/land"
)

// jevTestselCandidate wraps a rig's candidate gate with the test-selection
// shadow battery when patrols.jev_testsel is enabled. Off, it returns the gate
// unchanged, so a landing runs exactly as it did before the battery existed.
// The battery is read at each landing's gate, never on a ticker.
func (d *Daemon) jevTestselCandidate(rigName string, gate *land.CandidateGate) land.Candidate {
	if !d.isPatrolActive("jev_testsel") {
		return gate
	}
	var settings testsel.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevTestsel != nil {
		settings = testsel.Settings{
			MaxUSDPerDay:        c.Patrols.JevTestsel.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevTestsel.USDPerBillionTokens,
			Scrub:               c.Patrols.JevTestsel.Scrub,
			ScrubCheck:          c.Patrols.JevTestsel.ScrubCheck,
		}
	}
	pass, err := testsel.Open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_testsel: error: %v; the shadow battery is off for %s", err, rigName)
		return gate
	}
	return testsel.Shadow{Gate: gate, Results: gate, Pass: pass, Rig: rigName, Logf: d.logger.Printf}
}
