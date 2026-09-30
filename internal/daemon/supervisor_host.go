package daemon

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/mayor"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/witness"
)

// The daemon hosts the town's long-lived supervisor (ADR 0003,
// gt-4k3fj.3). Every kill or restart the daemon makes goes through
// d.sup().Kill or d.sup().Restart; this file wires the verbs to the daemon's
// tmux, the role managers that start sessions, the escalation channel and
// the agent-bead display mirror.

// sup returns the daemon's supervisor, building it on first use so a Daemon
// built as a struct literal (every unit test) gets one over its own tmux.
func (d *Daemon) sup() *supervisor.Supervisor {
	d.supOnce.Do(func() {
		if d.supervisor != nil {
			return
		}
		opts := supervisor.Options{
			TownRoot: d.config.TownRoot,
			Tmux:     d.tmux,
			Restart:  d.restartSeat,
			Escalate: func(seat supervisor.Seat, line string) {
				d.escalateAlert("restart-budget:"+supervisor.IntentSeat(seat).String(), "restart-budget", line)
			},
			Logf: d.logger.Printf,
			Now:  d.clk().Now,
		}
		// The mirror shells out to bd, so it runs only in a daemon that
		// resolved one; a test daemon has no bdPath and never reaches the
		// real store.
		if d.bdPath != "" {
			opts.Mirror = d.mirrorAgentBead
		}
		if d.restartSeatFn != nil {
			opts.Restart = d.restartSeatFn
		}
		d.supervisor = supervisor.New(opts)
	})
	return d.supervisor
}

// errNoDaemonStarter is returned, as a declined restart, for a seat the
// daemon does not start: it spends no budget.
var errNoDaemonStarter = fmt.Errorf("%w: the daemon has no start path for this role", supervisor.ErrDeclined)

// restartSeat replaces a seat's session with a fresh one. It is the
// supervisor's restart executor, run only after the guards pass.
func (d *Daemon) restartSeat(seat supervisor.Seat) error {
	switch seat.Role {
	case session.RoleDeacon:
		if seat.Name != "" {
			return fmt.Errorf("%w: %s", errNoDaemonStarter, seat.SessionName())
		}
		if err := d.tmux.KillSessionWithProcesses(seat.SessionName()); err != nil {
			return fmt.Errorf("clearing the old deacon session: %w", err)
		}
		return declineIf(d.startDeacon(), deacon.ErrAlreadyRunning)
	case session.RoleWitness:
		mgr := witness.NewManager(&rig.Rig{Name: seat.Rig, Path: filepath.Join(d.config.TownRoot, seat.Rig)})
		return declineIf(mgr.Start(false, "", nil), witness.ErrAlreadyRunning)
	case session.RoleRefinery:
		mgr := refinery.NewManager(&rig.Rig{Name: seat.Rig, Path: filepath.Join(d.config.TownRoot, seat.Rig)})
		mgr.SetStartAttribution("daemon-heartbeat", "daemon")
		return declineIf(mgr.Start(false, ""), refinery.ErrAlreadyRunning, refinery.ErrSafetyStopped, refinery.ErrForkRig)
	case session.RoleMayor:
		mgr := mayor.NewManager(d.config.TownRoot)
		if err := mgr.Stop(); err != nil && !errors.Is(err, mayor.ErrNotRunning) {
			return fmt.Errorf("stopping the old mayor session: %w", err)
		}
		return mgr.Start("")
	default:
		// Polecats are restarted by the witness patrol scan until the
		// witness becomes a daemon tick (gt-4k3fj.6); dogs by their handler.
		return fmt.Errorf("%w: %s", errNoDaemonStarter, seat.SessionName())
	}
}

// declineIf wraps err as supervisor.ErrDeclined when it is one of the
// errors by which a role manager says it started nothing on purpose, so the
// attempt does not spend the seat's restart budget.
func declineIf(err error, declined ...error) error {
	for _, d := range declined {
		if errors.Is(err, d) {
			return fmt.Errorf("%w: %w", supervisor.ErrDeclined, err)
		}
	}
	return err
}

// mirrorAgentBead writes the seat's hold to its agent bead for display
// (gt polecat identity show, dashboards). Only a hold is mirrored: it is the
// one state the bead shows that the supervisor decides. Never read back.
func (d *Daemon) mirrorAgentBead(seat supervisor.Seat, rec intent.Record) error {
	if !rec.Held() {
		return nil
	}
	id := d.agentBeadIDForSeat(seat)
	if id == "" {
		return nil
	}
	return beads.New(d.config.TownRoot).ForAgentBead().UpdateAgentState(id, string(beads.AgentStatePaused))
}

// agentBeadIDForSeat returns the seat's agent bead ID, or "" for a seat with
// none.
func (d *Daemon) agentBeadIDForSeat(seat supervisor.Seat) string {
	switch seat.Role {
	case session.RoleMayor:
		return beads.MayorBeadIDTown()
	case session.RoleDeacon:
		if seat.Name != "" {
			return ""
		}
		return beads.DeaconBeadIDTown()
	case session.RoleDog:
		return beads.DogBeadIDTown(seat.Name)
	}
	if seat.Rig == "" {
		return ""
	}
	prefix := beads.GetPrefixForRig(d.config.TownRoot, seat.Rig)
	return beads.AgentBeadIDWithPrefix(prefix, seat.Rig, string(seat.Role), seat.Name)
}

// assessSeat runs the one liveness function for a seat against its session,
// comparing with the progress sample in the seat's intent record and writing
// the new sample back. An Unknown verdict leaves the record alone.
func (d *Daemon) assessSeat(seat supervisor.Seat, in liveness.Input) liveness.Result {
	iseat := supervisor.IntentSeat(seat)
	rec, err := intent.Read(d.config.TownRoot, iseat)
	if err != nil {
		return liveness.Result{Verdict: liveness.Unknown, Reason: "intent record unreadable", Err: err}
	}
	if in.Session == "" {
		in.Session = seat.SessionName()
	}
	in.Prev = rec.Progress
	if in.Now.IsZero() {
		in.Now = d.clk().Now()
	}
	res := liveness.Assess(d.tmux, in)
	if res.Verdict == liveness.Unknown {
		return res
	}
	if _, err := intent.Update(d.config.TownRoot, iseat, func(r *intent.Record) error {
		r.Progress = res.Sample
		return nil
	}); err != nil {
		d.logger.Printf("Warning: recording liveness sample for %s: %v", iseat, err)
	}
	return res
}

// logRefusal logs a supervisor refusal or failure for a call site that has
// nothing else to do with it. The supervisor already wrote the action line.
func (d *Daemon) logRefusal(what string, err error) {
	if errors.Is(err, supervisor.ErrRefused) {
		d.logger.Printf("%s: %v", what, err)
		return
	}
	d.logger.Printf("%s failed: %v", what, err)
}

// ClearAgentBackoff clears a supervisor freeze and empties the restart
// budget for the seat agentID names ("deacon", "mayor", "<rig>/witness",
// "<rig>/<polecat>", ...). It backs `gt daemon clear-backoff`; an operator
// park is left alone (that is `gt agent resume`).
func ClearAgentBackoff(townRoot, agentID string) error {
	id, err := session.ParseAddress(agentID)
	if err != nil {
		return fmt.Errorf("unknown agent %q: %w", agentID, err)
	}
	return supervisor.ClearHold(townRoot, *id, "gt daemon clear-backoff")
}
