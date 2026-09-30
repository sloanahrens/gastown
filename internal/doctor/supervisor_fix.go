package doctor

import (
	"fmt"

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

// killSessionForFix kills a seat's session through supervisor.Kill, which a
// parked seat or an e-stop refuses. A name that does not parse to a seat is
// refused: it may belong to a parked seat under a prefix the registry does
// not know, and a stray kill would skip that seat's hold.
func killSessionForFix(townRoot string, k supervisor.Killer, sess, reason string) error {
	seat, err := supervisor.SeatForSession(sess)
	if err != nil {
		return fmt.Errorf("not killing %s: it does not name a known seat (%v)", sess, err)
	}
	return fixSupervisor(townRoot, k).Kill(seat, reason, doctorActor)
}
