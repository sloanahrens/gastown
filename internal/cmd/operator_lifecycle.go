// Operator kill and respawn verbs, through the supervisor (gt-4k3fj.4.1).
//
// The supervisor is the one place a session is killed or replaced, so every
// operator verb that does either goes through it and lands in
// .runtime/supervisor/actions.jsonl with its actor and reason. Per verb:
//
//   - Stop (not refused by an e-stop or a park): gt down, gt crew stop
//     [--all], gt crew remove --force, gt crew rename, gt rig remove --force.
//     An operator ending a session is what an e-stop asks for, never what it
//     forbids.
//   - Respawn (refused by an e-stop, a park and a gt down in progress): gt
//     crew restart [--all], gt crew refresh, gt crew start over a running or
//     dead session (--resume, or a zombie), gt crew at reviving an exited
//     runtime. An e-stop means no restarts.
package cmd

import (
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
)

// operatorSupervisor returns the supervisor an operator verb kills and
// respawns through, over the real tmux and the town's session prefixes.
func operatorSupervisor(townRoot string) *supervisor.Supervisor {
	return supervisor.New(supervisor.Options{TownRoot: townRoot, Tmux: tmux.NewTmux(), Prefixes: townRegistry()})
}

// operatorActor names who ran an operator verb: the command and the caller's
// role, like gt kill-all's "gt kill-all/<actor>".
func operatorActor(verb string) string {
	return operatorActorFor(verb, detectActor())
}

func operatorActorFor(verb, who string) string {
	return verb + "/" + who
}

// superviseCrewRespawns routes the crew manager's session replacements
// through sup.Respawn for crew member seats of rigName.
func superviseCrewRespawns(m *crew.Manager, sup *supervisor.Supervisor, reg *session.PrefixRegistry, rigName, actor string) {
	m.Respawn = func(name, reason string, run func() error) error {
		return sup.Respawn(supervisor.SeatIn(reg, rigName, string(session.RoleCrew), name), reason, actor, run)
	}
}
