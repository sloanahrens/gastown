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
//
// Slice 2 (gt-4k3fj.4.1.1), per verb:
//
//   - Stop: gt session stop, gt polecat nuke (and gt polecat stale
//     --cleanup, which nukes), gt rig shutdown/stop/dock's polecat stops
//     (SessionManager.StopAll), gt mayor stop. Operator verbs that end a
//     session, like slice 1's.
//   - Respawn: gt handoff (self and remote) and gt handoff --cycle, gt
//     molecule step done cycling its pane, gt session restart over a running
//     session, gt mayor restart over a running Mayor, gt mayor attach
//     reviving an exited runtime, and every Start over a session whose agent
//     exited (gt session start/restart, gt up, gt sling's StartSession, gt
//     mayor start, gt up's Mayor). A handoff or step cycle is a restart, so
//     an e-stop refuses it and the session keeps running.
//   - Cleanup (logged, never refused): a polecat session Start created and
//     abandons after a failed startup, gt sling's rollback of a session that
//     died during startup, and the polecat manager's housekeeping kills (a
//     lingering session of a reallocated name, an idle session cleared for
//     reuse or repair, an orphan session without a directory). These clear
//     the way for a dispatch already decided on, which is where an e-stop
//     refuses (gt sling), and end no seat's running work.
//   - Unchanged: the daemon's restart executor (mayor Stop+Start, gt session
//     restart --requested-by daemon/patrol-scan) runs after
//     supervisor.Restart has guarded, budgeted and recorded the restart, so
//     it is not routed a second time.
//   - Already covered by slice 1: gt up's crew starts use getCrewManager,
//     which carries the Respawn hook.
package cmd

import (
	"path/filepath"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/mayor"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
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

// polecatSessionHooks routes a polecat SessionManager's kills through sup for
// rigName's polecat seats: Stop as the operator stop named stopReason,
// Cleanup for a session Start abandons, Respawn for Start over a session
// whose agent exited.
func polecatSessionHooks(sup *supervisor.Supervisor, reg *session.PrefixRegistry, rigName, stopReason, actor string) polecat.SessionHooks {
	seat := func(name string) supervisor.Seat {
		return supervisor.SeatIn(reg, rigName, constants.RolePolecat, name)
	}
	return polecat.SessionHooks{
		Stop: func(sessionID string) error { return sup.StopSession(sessionID, stopReason, actor) },
		Cleanup: func(name, reason string) error {
			return sup.Cleanup(seat(name), reason, actor)
		},
		Respawn: func(name, reason string, run func() error) error {
			return sup.Respawn(seat(name), reason, actor, run)
		},
	}
}

// supervisedPolecatSessions returns r's polecat SessionManager with its kills
// routed through the operator supervisor (see polecatSessionHooks).
func supervisedPolecatSessions(t *tmux.Tmux, r *rig.Rig, stopReason, actor string) *polecat.SessionManager {
	m := polecat.NewSessionManager(t, r, townRegistry())
	m.SetHooks(polecatSessionHooks(operatorSupervisor(filepath.Dir(r.Path)), townRegistry(), r.Name, stopReason, actor))
	return m
}

// polecatCleanup is a polecat Manager's cleanup hook: sup.Cleanup for
// rigName's polecat seats.
func polecatCleanup(sup *supervisor.Supervisor, reg *session.PrefixRegistry, rigName, actor string) func(name, reason string) error {
	return func(name, reason string) error {
		return sup.Cleanup(supervisor.SeatIn(reg, rigName, constants.RolePolecat, name), reason, actor)
	}
}

// supervisedPolecatManager is polecat.NewManager with its housekeeping kills
// routed through the operator supervisor's Cleanup.
func supervisedPolecatManager(r *rig.Rig, g *git.Git, t *tmux.Tmux, actor string) *polecat.Manager {
	m := polecat.NewManager(r, g, t, townRegistry())
	m.SetCleanup(polecatCleanup(operatorSupervisor(filepath.Dir(r.Path)), townRegistry(), r.Name, actor))
	return m
}

// mayorSeat is the Mayor's seat.
var mayorSeat = supervisor.SeatFor("", string(session.RoleMayor), "")

// superviseMayor routes the mayor manager's Stop through sup.Stop (gt mayor
// stop) and its replacement of a dead session through sup.Respawn.
func superviseMayor(m *mayor.Manager, sup *supervisor.Supervisor, actor string) {
	m.StopKill = func(string) error { return sup.Stop(mayorSeat, "mayor stop", actor) }
	m.Respawn = func(reason string, run func() error) error {
		return sup.Respawn(mayorSeat, reason, actor, run)
	}
}

// respawnSession runs run, which replaces sessionName's agent in place,
// through sup.Respawn for the session's seat. A session name that parses to
// no seat has no hold or rig to refuse it and runs directly.
func respawnSession(sup *supervisor.Supervisor, reg *session.PrefixRegistry, sessionName, reason, actor string, run func() error) error {
	seat, err := supervisor.SeatForSession(reg, sessionName)
	if err != nil {
		return run()
	}
	return sup.Respawn(seat, reason, actor, run)
}
