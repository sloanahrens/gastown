package daemon

import "github.com/steveyegge/gastown/internal/notify"

// The unhandled-escalation push (gt-71714): an escalation the overseer owns
// pushes nothing when it is filed. One still open after its unhandled window is
// one nobody acted on, so the human is told once, and the push is recorded on
// the bead so a restart does not repeat it.

// pushUnhandledEscalations is the heartbeat step: one pass over the town's open
// escalations. It is a non-lifecycle step, so it runs under the town E-stop
// too: the town still owes the human the escalations nobody acted on.
func (d *Daemon) pushUnhandledEscalations() {
	ids, err := notify.PushUnhandled(d.config.TownRoot, d.clk().Now())
	for _, id := range ids {
		d.logger.Printf("unhandled escalations: pushed %s", id)
	}
	if err != nil {
		d.logger.Printf("unhandled escalations: %v", err)
	}
}
