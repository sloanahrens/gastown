package daemon

import (
	"github.com/steveyegge/gastown/internal/jev/failcause"
	"github.com/steveyegge/gastown/internal/land"
)

// jevFailcauseCandidate wraps a rig's candidate gate with the failing-test cause
// battery when patrols.jev_failcause is enabled. Off, it returns the gate
// unchanged, so a landing runs exactly as it did before the battery existed.
// The battery reads each red verdict's excerpt, never a ticker.
func (d *Daemon) jevFailcauseCandidate(rigName string, gate land.Candidate) land.Candidate {
	if !d.isPatrolActive("jev_failcause") {
		return gate
	}
	var settings failcause.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevFailcause != nil {
		settings = failcause.Settings{
			MaxUSDPerDay:        c.Patrols.JevFailcause.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevFailcause.USDPerBillionTokens,
			Scrub:               c.Patrols.JevFailcause.Scrub,
			ScrubCheck:          c.Patrols.JevFailcause.ScrubCheck,
		}
	}
	open := d.jevFailcauseOpen
	if open == nil {
		open = failcause.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_failcause: error: %v; the cause battery is off for %s", err, rigName)
		return gate
	}
	// The battery judges on a context that cannot be canceled, so the E-stop
	// gate cannot cancel a call in flight. It refuses each Ask after the
	// E-stop instead, and the judging ends on that refusal.
	pass.Asker = newEstopGate(d.config.TownRoot, pass.Asker, func() {})
	return failcause.Shadow{Gate: gate, Pass: pass, Rig: rigName, Logf: d.logger.Printf}
}
