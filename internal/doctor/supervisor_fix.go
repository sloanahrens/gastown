package doctor

import (
	"github.com/steveyegge/gastown/internal/supervisor"
)

// doctorActor is the actor gt doctor's kills are logged under.
const doctorActor = "gt doctor --fix"

// fixSupervisor returns the supervisor gt doctor --fix kills through
// (gt-4k3fj.3): the same pause, e-stop and actor-logging rules as the
// daemon, over the check's own tmux.
func fixSupervisor(townRoot string, k supervisor.Killer) *supervisor.Supervisor {
	return supervisor.New(supervisor.Options{TownRoot: townRoot, Tmux: k})
}

// killSessionForFix kills a town session through the supervisor: a session
// that names a seat through Kill, which a parked seat or an e-stop refuses;
// anything else through KillStray.
func killSessionForFix(townRoot string, k supervisor.Killer, sess, reason string) error {
	sup := fixSupervisor(townRoot, k)
	if seat, err := supervisor.SeatForSession(sess); err == nil {
		return sup.Kill(seat, reason, doctorActor)
	}
	return sup.KillStray(sess, reason, doctorActor)
}
