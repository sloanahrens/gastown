package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// The overrun_guard patrol watches how long each polecat seat's bead has been
// in progress, counted from the bead's dispatch rather than from the sling
// that reached the seat holding it, so a bead handed between seats keeps one
// clock. It nudges the seat once when the bead runs long, asking for a split
// proposal in the bead notes, and past the stop threshold it stops the
// session and hands the bead back. This file is the patrol's config, ticker
// entry, dry-run and nudge; the stop is overrunStop (overrun_guard_stop.go).
// The pure decision is decideOverrun and the per-bead memory is overrunState
// (overrun_guard_decide.go).

const (
	defaultOverrunGuardInterval   = time.Minute
	defaultOverrunGuardNudgeAfter = 12 * time.Minute
	defaultOverrunGuardStopAfter  = 20 * time.Minute
	// defaultOverrunGuardScheduledStopAfter is the one limit on a scheduled
	// sling. A deliberate deep-review runs long, but not without end.
	defaultOverrunGuardScheduledStopAfter = 60 * time.Minute
)

// overrunNudgeTimeout bounds one nudge. Queue mode only writes a file, so it
// is generous.
const overrunNudgeTimeout = 30 * time.Second

// overrunMaxGateCharge is how much gate time one tick may charge a seat: twice
// the guard's interval. A cycle takes the interval plus its own bd reads, so
// the slack leaves a slow tick charged in full; a wider gap is a tick the guard
// skipped, where the seat may have left and re-entered its gate unwatched, and
// the rest of that gap counts as the seat's own time (gt-vfwuc).
func overrunMaxGateCharge(interval time.Duration) time.Duration { return 2 * interval }

// overrunMaxGateTotal is the ceiling on the gate time one sling may bank: twice
// the stop threshold. A gate that never returns is a stuck seat, and the
// exemption must not hold its clock for good. At the default stop the ceiling
// bounds any sling, gated or not, at an hour of wall time — the same hour the
// scheduled-sling limit gives a deep review — and leaves a real gate the margin
// over the slowest one seen (the devops suite, 10 to 25 minutes on a loaded
// host) that keeps a long gate from counting as the seat's own time (gt-vfwuc).
func overrunMaxGateTotal(stopAfter time.Duration) time.Duration { return 2 * stopAfter }

// overrunMaxEscalationTotal is the ceiling on the escalation time one sling may
// bank, the gate's shape: twice the stop threshold. An escalation nobody answers
// is a seat held for a person, and past this ceiling the clock runs again even
// while the escalation stays open (gt-i1qwy).
func overrunMaxEscalationTotal(stopAfter time.Duration) time.Duration { return 2 * stopAfter }

// overrunNudgeMessage is what the seat reads at the nudge threshold. It says
// which time the clock measures, because a seat waiting on its own gate is the
// one case where a long-running bead is not an overrun (gt-vfwuc).
const overrunNudgeMessage = "This bead has run longer than expected. Write a split proposal in the bead notes now " +
	"(gt bead note): ordered steps, each small enough for one pass. Then keep going on the first step. " +
	"Time spent waiting inside your own gate — a container-gate slot you hold, or a gate suite running in your " +
	"pane — does not count toward this clock, up to a fixed ceiling: a gate that never returns is still a stop. " +
	"If the bead is still running at the stop threshold, the session is stopped and the bead goes back to the pool."

// overrunSeat is one polecat seat the guard judges.
type overrunSeat struct {
	Rig, Polecat, Bead, Branch string
	Submitted, Parked          bool
	// SubmittedAt is the time on the bead's READY TO LAND block, zero when it
	// carries none. The submission ended the run it was made in, so a rework
	// re-slung after a landing rejection is dated from that re-sling, not from
	// the first dispatch (gt-chd44). It comes from the bead read the seat
	// already costs, so the guard reads no bead twice for one clock.
	SubmittedAt time.Time
	// RestartedAt is the latest supervised restart of the seat's session, from
	// the intent record's restart stamps; zero when it never was restarted. A
	// restarted seat gets a fresh clock (overrunState.withRestart), because the
	// time it spent dead was not time a session was alive on the bead
	// (gt-y9lkx).
	RestartedAt time.Time
	// Scheduled is set when the bead carries a scheduled: label, the mark of a
	// run the daemon's scheduled_slings patrol created. Those run long by
	// design (the deep-review is report-only), so the guard never nudges them
	// and gives them the longer scheduled_stop_after limit in place of the 12
	// and 20 minute rule. Past it the session is stopped and the bead is not
	// handed back (overrunScheduledStop).
	Scheduled bool
	// SlingAt dates the bead's dispatch clock, when known: the earliest sling
	// of the bead this run, not necessarily one to this seat. The stop carries
	// it so it can tell a head the seat produced during the run from one it
	// merely resumed: a rework bead is slung onto a branch already on origin,
	// and an idle seat there would otherwise read as finished work
	// (overrunFinishedOnOrigin).
	SlingAt time.Time
	// ReworkSHA is set only on the seat handed to handBack, when the stopped
	// seat's finished work was already on origin: the commit both hold for the
	// kept branch. It turns the hand-back into rework (overrunStop, step 5).
	ReworkSHA string
}

// overrunSeams are the guard's reads and side effects. A test sets all of
// them with a fake clock; production fills them in (overrunProdSeams).
type overrunSeams struct {
	// now is the clock; nil means time.Now.
	now func() time.Time
	// workingSeats lists the seats that are alive and hold a bead.
	workingSeats func() ([]overrunSeat, error)
	// slungAt dates the seat's bead dispatch, false when it is unknown. It takes
	// the seat because the bead's last submission is read with it (the seat's
	// bead read), so the clock and the run end it started after are one read.
	slungAt func(seat overrunSeat) (time.Time, bool)
	// nudge delivers msg to the seat's session.
	nudge func(seat overrunSeat, msg string) error
	// nudged is told each nudge that was delivered, and when, so the progress
	// battery can ask about the seat (jev_progress.go). It is called only after
	// nudge succeeds, and nil means nothing is asked.
	nudged func(seat overrunSeat, at time.Time)

	// submittedNow re-reads, at the moment of acting, whether the seat has
	// submitted its work: gt done is running (a fresh exiting heartbeat) or the
	// bead is gt:ready-to-land. The seat list can be minutes old by then, and
	// gt done sets the label only after its rebase and gate. nil means no
	// re-check.
	submittedNow func(seat overrunSeat) bool

	// gateRunning reports whether the seat is inside its own gate now: it holds
	// a slot of the town's container-gate pool, or a gate suite is running
	// under its tmux pane (overrun_guard_gate.go). The time it spends there is
	// subtracted from the sling's elapsed time, so waiting on a gate is not an
	// overrun. An error means the seat's gate state could not be read, and the
	// guard then counts all of its time — the direction that stops. nil means
	// the same: an unset seam is no exemption.
	gateRunning func(seat overrunSeat) (bool, error)

	// escalationWaiting reports whether the seat is waiting on an open
	// escalation it raised for its bead (overrun_guard_escalation.go). The time
	// it spends there is subtracted from the sling's elapsed time, with its own
	// ceiling. An error means the escalation state could not be read, and the
	// guard then counts all of the seat's time. nil means no exemption.
	escalationWaiting func(seat overrunSeat) (bool, error)

	// The stop's side effects, called in this order by overrunStop:
	// pause writes the seat's pause marker, stopSession ends its session,
	// removeSandbox pushes and removes the sandbox a sandboxed seat ran in
	// (nil means none; a refusal holds the stop), checkpoint commits its
	// worktree, endRun records the run the stop ended,
	// handBack returns its bead to the pool after elapsed, with note added to
	// its comment (empty unless the stop gave up on the checkpoint). clearPause
	// removes the marker again when the session would not stop.
	pause         func(seat overrunSeat) error
	clearPause    func(seat overrunSeat) error
	stopSession   func(seat overrunSeat) error
	removeSandbox func(seat overrunSeat) error
	checkpoint    func(seat overrunSeat) error
	// stillOurs reports whether the bead is still hooked to the stopped seat:
	// the stop reads again just before it ends the run, because a bead closed,
	// or re-slung to another polecat, since the seat list was read is that
	// holder's, and recording an end for it would end the new holder's run
	// (gt-tzbtf). An error fails the step, so nothing is written on an
	// unreadable bead. nil means the stop cannot tell and records the end; the
	// hand-back's own check still refuses a bead that is not the seat's.
	stillOurs func(seat overrunSeat) (bool, error)
	// endRun records on the bead that the stop ended its run
	// (events.TypeOverrunStop), at the time the run ended, so the dispatch
	// after the hand-back is a new run the guard polices afresh. Without it a
	// handed-back bead keeps its old clock and the one-stop-per-run rule leaves
	// it unpoliced until a done event that a hand-back never writes. The time
	// is the one saved with the stop, not the tick's, so a record repeated by a
	// retried stop carries the same end and cannot move the boundary past the
	// dispatch it ends. An error fails the step: the bead is handed back only
	// after the run's end is recorded. nil means nothing is recorded.
	endRun   func(seat overrunSeat, at time.Time) error
	handBack func(seat overrunSeat, elapsed time.Duration, note string) error

	// push puts the seat's kept branch on origin, after the checkpoint and
	// before handBack, so the comment never names a branch that exists only
	// in the worktree. resetAgentBead clears the seat's agent bead of the
	// handed-back bead, in the release step before retire. nil means no
	// push / no reset.
	push           func(seat overrunSeat) error
	resetAgentBead func(seat overrunSeat) error

	// finishedOnOrigin is read just before push, after the checkpoint: it
	// returns the commit when the kept branch's head in the worktree is already
	// origin's head for that branch and the seat produced that head after it was
	// slung, "" otherwise (no worktree, no branch, not on origin, a different
	// commit, origin not reachable, a head whose committer time predates the
	// sling, a sling time the stop does not know). "Produced" is judged by the
	// head's committer time, which a rebase or cherry-pick during the run
	// rewrites: a head the seat merely resumed keeps the time it was pushed
	// with. A checkpoint that committed moved the head off origin's, so the
	// answer also says the checkpoint had nothing to commit. nil means ""
	// every time.
	finishedOnOrigin func(seat overrunSeat) string

	// recordCleanup records the released seat's cleanup status on its agent
	// bead, which resetAgentBead leaves missing, and retire ends the seat's
	// hold and stops its record, both after resetAgentBead and before the stop
	// is marked finished. A recordCleanup failure is logged and the release
	// goes on; a retire failure leaves the stop pending. nil means no record /
	// no retire (a hold left standing).
	recordCleanup func(seat overrunSeat) error
	retire        func(seat overrunSeat) error

	// resetWorktree runs in the release step, before recordCleanup, only for a
	// stop whose push step succeeded: it confirms the push by reading origin's
	// head for the branch back and, when that is the worktree's HEAD, detaches
	// the worktree at origin's default branch and deletes the local branch, so
	// the retired seat does not read as unpushed work once the branch lands. An
	// unconfirmed push is not an error: it leaves the worktree as it is. An
	// error is logged and the release goes on. nil means no reset.
	resetWorktree func(seat overrunSeat) error

	// comment adds one comment to the seat's bead: the scheduled stop's
	// record. nil means no comment.
	comment func(seat overrunSeat, text string) error

	// closeBead closes a stopped scheduled run's bead with reason, after
	// clearing its assignee (a close by an actor that is not the assignee is
	// refused). It is the scheduled stop's hand-back: the run is report-only and
	// the scheduled_slings patrol creates the next one. nil means the bead is
	// left hooked and the seat stays paused.
	closeBead func(seat overrunSeat, reason string) error

	// stopLapsed re-reads, before a resumed stop runs its checkpoint or
	// hand-back, whether the stop still owns the seat. It returns why not
	// (the pause marker is gone, a session is running) or "" when it does; an
	// error means the seat could not be read and the step is retried. nil means
	// no re-check. sessionStopped is false for the check before the session is
	// stopped, when the stop's own session may still be running and only a
	// missing pause marker means the seat was taken back.
	stopLapsed func(seat overrunSeat, sessionStopped bool) (string, error)

	// spanEnd closes the attempt the stop ended on the seat's bead at at: the
	// 20-minute stop is what ended the run, so the reason is overrun. It runs
	// once the bead is handed back, in the give-up branch as well, and its
	// failure is logged and never fails the stop. nil records nothing.
	spanEnd func(seat overrunSeat, at time.Time) error
}

// overrunGuardConfig returns the patrol's config, or nil when absent.
func overrunGuardConfig(c *DaemonPatrolConfig) *OverrunGuardConfig {
	if c == nil || c.Patrols == nil {
		return nil
	}
	return c.Patrols.OverrunGuard
}

// overrunDuration parses s as a positive duration, or returns def.
func overrunDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// overrunGuardInterval returns the configured run interval, or the default
// (1m).
func overrunGuardInterval(c *DaemonPatrolConfig) time.Duration {
	if cfg := overrunGuardConfig(c); cfg != nil {
		return overrunDuration(cfg.IntervalStr, defaultOverrunGuardInterval)
	}
	return defaultOverrunGuardInterval
}

// overrunGuardThresholds returns the nudge and stop thresholds, defaulting to
// 12m and 20m.
func overrunGuardThresholds(c *DaemonPatrolConfig) (nudgeAfter, stopAfter time.Duration) {
	cfg := overrunGuardConfig(c)
	if cfg == nil {
		return defaultOverrunGuardNudgeAfter, defaultOverrunGuardStopAfter
	}
	return overrunDuration(cfg.NudgeAfter, defaultOverrunGuardNudgeAfter),
		overrunDuration(cfg.StopAfter, defaultOverrunGuardStopAfter)
}

// overrunScheduledStopAfter returns the limit on a scheduled sling: the
// configured scheduled_stop_after, 60m when unset or unusable, and zero when it
// is set to zero, which never stops one. The config loader keeps a negative or
// unparseable value as written, so it falls back to 60m here.
func overrunScheduledStopAfter(c *DaemonPatrolConfig) time.Duration {
	cfg := overrunGuardConfig(c)
	if cfg == nil || cfg.ScheduledStopAfter == "" {
		return defaultOverrunGuardScheduledStopAfter
	}
	d, err := time.ParseDuration(cfg.ScheduledStopAfter)
	if err != nil || d < 0 {
		return defaultOverrunGuardScheduledStopAfter
	}
	return d
}

// triggerOverrunGuard runs an overrun_guard cycle when the patrol is due, on
// its own goroutine. Due-ness comes from the persisted last-run time, as for
// checkpoint_dog (triggerCheckpointDog), so a daemon restart does not reset
// the cadence.
func (d *Daemon) triggerOverrunGuard() {
	if !d.isPatrolActive("overrun_guard") {
		return
	}

	dec := evaluatePatrolDue(d.config.TownRoot, "overrun_guard", d.overrunGuardRun.last(), time.Now(), overrunGuardInterval(d.patrolConfig))
	if !dec.due {
		return
	}

	if !d.overrunGuardRunning.CompareAndSwap(false, true) {
		d.logger.Printf("overrun_guard: previous cycle still running — skipping this check")
		return
	}

	if dec.warn != "" {
		d.logger.Printf("overrun_guard: WARNING: %s — %s", dec.warn, dec.note)
	}

	go func() {
		defer d.overrunGuardRunning.Store(false)
		defer func() {
			d.recordPatrolRun(&d.overrunGuardRun, "overrun_guard", time.Now())
		}()
		d.runOverrunGuard(d.overrunProdSeams())
	}()
}

// runOverrunGuard judges every working seat once. A seat that is submitted,
// parked or undated is left alone. At the nudge threshold the seat is nudged
// once per run of the bead, remembered in the bead's state file; past the stop
// threshold the stop seam runs. A seat on a scheduled sling is never nudged and
// is judged by scheduled_stop_after alone (zero leaves it alone). The clock it
// judges is the later of the bead's dispatch (slungAt) and a restart of the
// seat since then, less the time the seat spends inside its gate, or waiting on
// an escalation it raised; a state that cannot be read counts all of its time
// (overrun_guard_gate.go, overrun_guard_escalation.go, withRestart).
// In dry-run mode nothing is sent, stopped or written, and the log says what
// would have happened.
func (d *Daemon) runOverrunGuard(s overrunSeams) {
	if !d.isPatrolActive("overrun_guard") {
		return
	}
	if s.now == nil {
		s.now = time.Now
	}
	nudgeAfter, stopAfter := overrunGuardThresholds(d.patrolConfig)
	scheduledStopAfter := overrunScheduledStopAfter(d.patrolConfig)
	dry := false
	if cfg := overrunGuardConfig(d.patrolConfig); cfg != nil {
		dry = cfg.DryRun
	}
	// An unset gate seam is no exemption: every seat's time counts.
	gateRunning := s.gateRunning
	if gateRunning == nil {
		gateRunning = func(overrunSeat) (bool, error) { return false, nil }
	}
	// An unset escalation seam is no exemption, as for the gate.
	escalationWaiting := s.escalationWaiting
	if escalationWaiting == nil {
		escalationWaiting = func(overrunSeat) (bool, error) { return false, nil }
	}
	maxGateCharge := overrunMaxGateCharge(overrunGuardInterval(d.patrolConfig))
	maxGateTotal := overrunMaxGateTotal(stopAfter)
	maxEscalationTotal := overrunMaxEscalationTotal(stopAfter)
	// The escalation read is logged once per tick, not once per seat: a failed
	// read is the same failure for every seat it reaches this cycle.
	escalationReadFailed := false

	seats, err := s.workingSeats()
	if err != nil {
		d.logger.Printf("overrun_guard: cannot list working seats: %v", err)
		return
	}
	now := s.now()

	// Stops that began on an earlier tick and did not finish go first. Their
	// seats are no longer working seats (the session is gone or the seat is
	// parked), so the seat list below cannot find them.
	var resumed map[string]bool
	if !dry {
		resumed = d.resumeOverrunStops(s)
	}

	for _, seat := range seats {
		if seat.Submitted || seat.Parked || (seat.Scheduled && scheduledStopAfter <= 0) || seat.Bead == "" || resumed[seat.Bead] {
			continue
		}
		who := seat.Rig + "/" + seat.Polecat
		slung, ok := s.slungAt(seat)
		if !ok {
			continue
		}
		state, err := readOverrunState(d.config.TownRoot, seat.Bead)
		if err != nil {
			// An unreadable record could hide a nudge already sent; act on
			// nothing until it reads.
			d.logger.Printf("overrun_guard: %s: cannot read state for %s: %v", who, seat.Bead, err)
			continue
		}
		state.Bead = seat.Bead
		// Everything that outlives this tick is compared against before, written
		// below only when it moved.
		before := state

		// A restart of the seat starts the clock over (withRestart), so the nudge
		// and stop limits below measure the later of the sling and the restart;
		// slung keeps dating the run for the stop's own state.
		state, clockStart := state.withRestart(slung, seat.RestartedAt)

		// A nudge sent before this clock belongs to an earlier run of the bead
		// (a run that ended, after which the bead was dispatched again). The
		// nudge is the bead's, not the seat's: a bead handed to a second seat
		// keeps the clock, and the nudge already went out for it.
		nudged := !state.NudgedAt.IsZero() && !state.NudgedAt.Before(clockStart)

		// Gate time does not count (overrun_guard_gate.go). A state that cannot
		// be read counts all of the seat's time, the direction that stops.
		inGate, err := gateRunning(seat)
		if err != nil {
			d.logger.Printf("overrun_guard: %s: cannot read the gate state for %s, counting all of its time: %v", who, seat.Bead, err)
			inGate = false
		}
		state, gateSpent := state.withGate(clockStart, now, inGate, maxGateCharge, maxGateTotal)

		// A seat waiting on an escalation it raised is waiting on a person, not
		// overrunning (overrun_guard_escalation.go). The escalation is read only
		// outside a gate, which is already exempt, so no stretch is exempt twice.
		waiting := false
		if !inGate {
			waiting, err = escalationWaiting(seat)
			if err != nil {
				if !escalationReadFailed {
					escalationReadFailed = true
					d.logger.Printf("overrun_guard: %s: cannot read the escalation state for %s, counting all of its time: %v", who, seat.Bead, err)
				}
				waiting = false
			}
		}
		state, escalationSpent := state.withEscalation(clockStart, now, waiting, maxGateCharge, maxEscalationTotal)

		if !dry && !state.sameWait(before) {
			// The gate and escalation accounting outlives the tick: it is saved
			// when it moves, so the next tick — and a stop that begins later —
			// measures the same clock.
			if err := writeOverrunState(d.config.TownRoot, state); err != nil {
				d.logger.Printf("overrun_guard: WARNING: %s: gate or escalation time on %s not saved (%v) — it is counted again next tick", who, seat.Bead, err)
			}
		}

		elapsed := now.Sub(clockStart) - gateSpent - escalationSpent
		if dry && inGate {
			// A dry run writes nothing, so it has no banked gate time to
			// subtract: it says what it sees and leaves the verdict to a real
			// run rather than logging a stop the guard would not make.
			d.logger.Printf("overrun_guard: dry run: %s is inside its gate on %s after %s wall — its clock does not run",
				who, seat.Bead, elapsed.Round(time.Second))
			continue
		}
		if dry && waiting && escalationSpent < maxEscalationTotal {
			// A dry run banks no escalation time either, so without this a seat
			// the live guard exempts as waiting on a person reads as past the
			// stop threshold on every dry tick (gt-4nqej). Past the ceiling the
			// live guard lets the clock run again, so the thresholds below
			// decide there.
			d.logger.Printf("overrun_guard: dry run: %s is waiting on an escalation it raised on %s after %s wall — its clock does not run",
				who, seat.Bead, elapsed.Round(time.Second))
			continue
		}

		in := overrunInput{
			Now: now, SlungAt: clockStart, Nudged: nudged,
			NudgeAt: nudgeAfter, StopAt: stopAfter, GateSpent: gateSpent, EscalationSpent: escalationSpent,
		}
		if seat.Scheduled {
			// Both thresholds at the one limit: the stop wins from there on
			// and no nudge can fall due before it.
			in.NudgeAt, in.StopAt = scheduledStopAfter, scheduledStopAfter
		}
		action := decideOverrun(in)

		switch action {
		case overrunNudge:
			if dry {
				d.logger.Printf("overrun_guard: dry run: would nudge %s on %s after %s", who, seat.Bead, elapsed.Round(time.Second))
				continue
			}
			if state.nudgeHeld(clockStart, now) {
				continue
			}
			if err := s.nudge(seat, overrunNudgeMessage); err != nil {
				state.NudgeFailures = state.nudgeFailures(clockStart) + 1
				state.NudgeFailedAt = now
				d.logger.Printf("overrun_guard: nudge %s on %s failed (attempt %d of %d): %v",
					who, seat.Bead, state.NudgeFailures, overrunNudgeMaxAttempts, err)
				if state.NudgeFailures >= overrunNudgeMaxAttempts {
					d.logger.Printf("overrun_guard: nudge to %s on %s abandoned after %d failed attempts; the stop at %s still applies",
						who, seat.Bead, state.NudgeFailures, stopAfter)
				}
				if err := writeOverrunState(d.config.TownRoot, state); err != nil {
					d.logger.Printf("overrun_guard: WARNING: %s nudge failure on %s not saved (%v) — it may be retried next tick", who, seat.Bead, err)
				}
				continue
			}
			if s.nudged != nil {
				s.nudged(seat, now)
			}
			state.NudgedAt = now
			if err := writeOverrunState(d.config.TownRoot, state); err != nil {
				d.logger.Printf("overrun_guard: WARNING: %s nudged on %s but state not saved (%v) — it may be nudged again", who, seat.Bead, err)
				continue
			}
			d.logger.Printf("overrun_guard: nudged %s on %s after %s", who, seat.Bead, elapsed.Round(time.Second))
		case overrunStop:
			// The stop ran for this run of the bead already and the bead is
			// back in the pool, labeled for a split or as rework. The stop
			// itself ends the run — it records events.TypeOverrunStop before
			// the hand-back — and the dispatch clock reads that same record, so
			// a dispatch after the recorded end is a fresh run the guard
			// polices, even one written before the stop finished
			// (overrunState.stoppedInsideRun, gt-lfszo).
			if state.stoppedInsideRun(slung) {
				continue
			}
			if dry {
				kind := ""
				if seat.Scheduled {
					kind = "scheduled "
				}
				d.logger.Printf("overrun_guard: dry run: would stop %s%s on %s after %s", kind, who, seat.Bead, elapsed.Round(time.Second))
				continue
			}
			if seat.Scheduled {
				d.overrunScheduledStop(s, seat, state, elapsed)
				continue
			}
			// The stop's rework decision needs the sling it is stopping: a head
			// from before it is the branch the seat resumed, not its work.
			seat.SlingAt = slung
			d.overrunStop(s, seat, state, elapsed)
		}
	}
}

// overrunProdSeams returns the production seams: the daemon's rigs, event
// log and notifier, the gate reader, and the stop's pause, session, checkpoint
// and hand-back.
func (d *Daemon) overrunProdSeams() overrunSeams {
	sl := &overrunSlungReader{d: d}
	gate := newOverrunGateReader(d)
	escalation := newOverrunEscalationReader(d)
	return d.overrunStopSeams(overrunSeams{
		now:               time.Now,
		workingSeats:      d.overrunWorkingSeats,
		slungAt:           sl.slungAt,
		gateRunning:       gate.running,
		escalationWaiting: escalation.waiting,
		nudged:            d.enqueueJevProgress,
		nudge: func(seat overrunSeat, msg string) error {
			ctx, cancel := context.WithTimeout(context.Background(), overrunNudgeTimeout)
			defer cancel()
			// Queue mode: the message waits for the seat's next hook. The
			// default waits up to 15 s for an idle prompt and then watches the
			// busy seat for 60 s more, which outlasts this context. A seat
			// running tool turns drains the queue through the polecat
			// PostToolUse hook, gt tap nudge-drain (gt-drwph).
			return d.notify().Nudge(ctx, seat.Rig+"/"+seat.Polecat, msg, notify.Queue())
		},
	})
}

// overrunWorkingSeats lists every polecat with a live session that holds a
// bead, across the known rigs. A seat with no live session, or none holding
// work, is idle and absent.
func (d *Daemon) overrunWorkingSeats() ([]overrunSeat, error) {
	var out []overrunSeat
	for _, rigName := range d.getKnownRigs() {
		polecatsDir := filepath.Join(d.config.TownRoot, rigName, "polecats")
		names, err := listPolecatWorktrees(polecatsDir)
		if err != nil {
			continue // a rig without polecats
		}
		for _, name := range names {
			seat, ok := d.overrunSeatFor(polecatsDir, rigName, name)
			if ok {
				out = append(out, seat)
			}
		}
	}
	return out, nil
}

// overrunSeatFor reads one polecat's seat state, false when it is idle or
// cannot be read. An unreadable record or bead drops the seat: the guard acts
// only on what it could confirm.
func (d *Daemon) overrunSeatFor(polecatsDir, rigName, name string) (overrunSeat, bool) {
	seat := overrunSeat{Rig: rigName, Polecat: name}
	rec, err := intent.Read(d.config.TownRoot, supervisor.IntentSeat(
		supervisor.SeatIn(d.prefixRegistry(), rigName, constants.RolePolecat, name)))
	if err != nil {
		d.logger.Printf("overrun_guard: %s/%s: cannot read intent record: %v", rigName, name, err)
		return seat, false
	}
	seat.Parked = rec.Held()
	seat.Submitted = rec.Submitted()
	seat.RestartedAt = latestRestart(rec.Restarts)
	if seat.Parked || seat.Submitted {
		return seat, false // nothing to time, and no reason to spend a bd read
	}

	sessionName := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(rigName), name)
	alive, err := d.tmux.HasSession(sessionName)
	if err != nil || !alive {
		return seat, false
	}
	if d.gtDoneInFlight(sessionName) {
		seat.Submitted = true
		return seat, false // gt done is running; its intent record and label come minutes later
	}

	bead := rec.WorkBead
	if bead == "" {
		bead, err = d.assignedActiveWorkBead(rigName, fmt.Sprintf("%s/polecats/%s", rigName, name))
		if err != nil {
			d.logger.Printf("overrun_guard: %s/%s: cannot read assigned work: %v", rigName, name, err)
			return seat, false
		}
	}
	if bead == "" {
		return seat, false
	}
	// One read answers the seat's whole bead: whether its work is over, its
	// labels, and the end of the run a submission made. A bead that cannot be
	// read is not judged: the submission it may hold is the run end the clock
	// has to start after, so acting on the log alone could date a rework from
	// the dispatch its submission ended (gt-chd44).
	st, err := d.workBeadState(bead)
	if err != nil {
		d.logger.Printf("overrun_guard: %s/%s: cannot read bead %s, not judging it this tick: %v", rigName, name, bead, err)
		return seat, false
	}
	if st.closed {
		return seat, false
	}
	seat.Bead = bead
	seat.Submitted = st.submitted
	seat.SubmittedAt = st.submittedAt
	seat.Scheduled = hasScheduledSlingLabel(st.labels)

	if workDir := resolvePolecatWorktree(polecatsDir, name, rigName); workDir != "" {
		if branch, err := d.gitAt(workDir).CurrentBranch(); err == nil {
			seat.Branch = branch
		}
	}
	return seat, true
}

// gtDoneInFlight reports whether the session's heartbeat says gt done is
// running. gt done writes state exiting first (internal/done/done.go) and renews
// it through its gate stages, before the intent record or gt:ready-to-land
// exist, so a fresh exiting heartbeat is the early submitted signal.
func (d *Daemon) gtDoneInFlight(sessionName string) bool {
	return heartbeatShowsGtDone(polecat.ReadSessionHeartbeat(d.config.TownRoot, sessionName), time.Now())
}

// heartbeatShowsGtDone reports whether hb is an exiting heartbeat fresher than
// the stale threshold. A stale one is a gt done that died or wedged: the seat
// stays subject to the guard.
func heartbeatShowsGtDone(hb *polecat.SessionHeartbeat, now time.Time) bool {
	return hb != nil && hb.EffectiveState() == polecat.HeartbeatExiting &&
		now.Sub(hb.Timestamp) < polecat.SessionHeartbeatStaleThreshold
}

// latestRestart returns the newest of the seat's restart stamps, zero when it
// was never restarted. The supervisor keeps only the stamps of its budget
// window, so a run longer than that reads the restart from the guard's own
// record once it has adopted it (withRestart).
func latestRestart(stamps []time.Time) time.Time {
	var out time.Time
	for _, t := range stamps {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// hasScheduledSlingLabel reports whether labels include one with the
// scheduled: prefix that scheduledSlingLabel gives a scheduled sling's bead.
func hasScheduledSlingLabel(labels []string) bool {
	return slices.ContainsFunc(labels, func(l string) bool { return strings.HasPrefix(l, scheduledSlingLabelPrefix) })
}

// overrunSlungReader dates slings from the town event log, reading it once
// per cycle.
type overrunSlungReader struct {
	d      *Daemon
	loaded bool
	clock  *events.DispatchClock
}

// slungAt is the time the seat's bead was dispatched, false when it is unknown.
// The clock belongs to the bead, not to the seat holding it: it is the earliest
// sling event for the bead that its last run-end event does not end
// (events.DispatchStart), so a bead handed from one seat to another keeps the
// dispatch it first went out on, and a bead whose work was finished, closed or
// whose seat the guard stopped starts over. The seat's SubmittedAt ends a run
// too, for a submission whose done event the log never got (gt-chd44). With no
// sling in the log at all it falls back to the seat's agent bead update time,
// which can only read short, so the fallback errs toward not stopping anyone.
func (r *overrunSlungReader) slungAt(seat overrunSeat) (time.Time, bool) {
	if !r.loaded {
		r.loaded = true
		r.clock = events.NewDispatchClock()
		evs, err := events.Read(r.d.config.TownRoot)
		if err != nil {
			r.d.logger.Printf("overrun_guard: reading events: %v", err)
		}
		r.clock.Add(evs)
	}
	if start, ok := r.clock.StartSince(seat.Bead, seat.SubmittedAt); ok {
		return start, true
	}

	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(r.d.config.TownRoot, seat.Rig), seat.Rig, seat.Polecat)
	issue, err := r.d.workBeads(r.d.workBeadsEnv(seat.Rig), workBeadReadTimeout).Show(id)
	if err != nil {
		return time.Time{}, false
	}
	if t := polecat.AgentBeadUpdatedAt(issue); !t.IsZero() {
		return t, true
	}
	return time.Time{}, false
}
