package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
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
			Prefixes: d.prefixRegistry(),
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
	case session.RolePolecat:
		// Only the patrol_scan tick restarts polecats from the daemon
		// (gt-4k3fj.6).
		return d.restartPolecatSession(seat)
	default:
		// The deacon, witness and dog roles were deleted
		// (gt-4k3fj.6.1, gt-ckunw).
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
	return beads.UpdateAgentState(beads.New(d.config.TownRoot), id, string(beads.AgentStatePaused))
}

// agentBeadIDForSeat returns the seat's agent bead ID, or "" for a seat with
// none.
func (d *Daemon) agentBeadIDForSeat(seat supervisor.Seat) string {
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
	if in.TownRoot == "" {
		in.TownRoot = d.config.TownRoot
	}
	if in.WorkDir == "" {
		in.WorkDir = d.seatWorkDir(seat)
	}
	in.Prev = rec.Progress
	if in.Now.IsZero() {
		in.Now = d.clk().Now()
	}
	// A sandboxed seat is assessed where it lives (session.SandboxLiveness);
	// a host seat on the host. The tmux probes route either way.
	res := session.AssessWhereItLives(d.config.TownRoot, d.tmux, in)
	if res.Verdict == liveness.Unknown {
		return res
	}
	d.recordSeatSample(iseat, rec, res.Sample)
	return res
}

// recordSeatSample writes sample as the seat's progress, but only while the
// record is still the one the assessment read. The read and the write are
// separate lock holds, and a sling or kill that lands between them (MarkSlung
// clears Progress for the session it starts) would otherwise have its clean
// record overwritten with the previous session's sample, which the new
// session's first real dead verdict then counts against. Every mutator sets
// UpdatedAt, so a moved UpdatedAt says the record changed.
func (d *Daemon) recordSeatSample(iseat intent.Seat, read intent.Record, sample *intent.Progress) {
	recorded := false
	if _, err := intent.Update(d.config.TownRoot, iseat, func(r *intent.Record) error {
		if !r.UpdatedAt.Equal(read.UpdatedAt) {
			return nil
		}
		r.Progress = sample
		recorded = true
		return nil
	}); err != nil {
		d.logger.Printf("Warning: recording liveness sample for %s: %v", iseat, err)
		return
	}
	if !recorded {
		d.logger.Printf("liveness sample for %s not recorded: the seat record changed after it was read", iseat)
	}
}

// seatWorkDir returns the directory a seat's agent session runs in, the path
// its Claude transcript is filed under. Claude names the project directory
// after the session's cwd, so the sample needs it or the lookup finds nothing.
// An existing directory is the cwd even without a .git, so a seat whose
// worktree broke is still readable. A seat with no worktree of its own (a
// town-level role) returns "" quietly and its sample carries no transcript
// evidence. A polecat or crew seat whose directories are all missing also
// returns "", but logs the paths it tried: that seat should have a worktree,
// and the log is what tells it from the quiet case.
func (d *Daemon) seatWorkDir(seat supervisor.Seat) string {
	if seat.Rig == "" || seat.Name == "" {
		return ""
	}
	var candidates []string
	switch seat.Role {
	case session.RolePolecat:
		candidates = polecatWorktreeCandidates(filepath.Join(d.config.TownRoot, seat.Rig, "polecats"), seat.Name, seat.Rig)
	case session.RoleCrew:
		candidates = []string{filepath.Join(d.config.TownRoot, seat.Rig, "crew", seat.Name)}
	}
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	if len(candidates) > 0 {
		d.logger.Printf("No work directory for %s: tried %s; its liveness sample carries no transcript",
			seat.Address(), strings.Join(candidates, ", "))
	}
	return ""
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
// budget for the seat agentID names ("<rig>/witness", "<rig>/<polecat>",
// ...). It backs `gt daemon clear-backoff`; an operator
// park is left alone (that is `gt agent resume`). reg resolves the seat's rig
// prefix.
func ClearAgentBackoff(reg *session.PrefixRegistry, townRoot, agentID string) error {
	id, err := session.ParseAddressWithRegistry(agentID, reg)
	if err != nil {
		return fmt.Errorf("unknown agent %q: %w", agentID, err)
	}
	return supervisor.ClearHold(townRoot, *id, "gt daemon clear-backoff")
}
