package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// The overrun_guard patrol watches how long each polecat seat has held its
// bead. It nudges the seat once when the bead runs long, asking for a split
// proposal in the bead notes, and past the stop threshold it stops the
// session. This file is the patrol's config, ticker entry, dry-run and nudge;
// the stop only logs for now. The pure decision is decideOverrun and the
// per-bead memory is overrunState (overrun_guard_decide.go).

const (
	defaultOverrunGuardInterval   = time.Minute
	defaultOverrunGuardNudgeAfter = 12 * time.Minute
	defaultOverrunGuardStopAfter  = 20 * time.Minute
)

// overrunNudgeMessage is what the seat reads at the nudge threshold.
const overrunNudgeMessage = "This bead has run longer than expected. Write a split proposal in the bead notes now " +
	"(gt bead note): ordered steps, each small enough for one pass. Then keep going on the first step. " +
	"If the bead is still running at the stop threshold, the session is stopped and the bead goes back to the pool."

// overrunSeat is one polecat seat the guard judges.
type overrunSeat struct {
	Rig, Polecat, Bead, Branch string
	Submitted, Parked          bool
}

// overrunSeams are the guard's reads and side effects. A test sets all of
// them with a fake clock; production fills them in (overrunProdSeams).
type overrunSeams struct {
	// now is the clock; nil means time.Now.
	now func() time.Time
	// workingSeats lists the seats that are alive and hold a bead.
	workingSeats func() ([]overrunSeat, error)
	// slungAt dates the bead's sling to the seat, false when it is unknown.
	slungAt func(bead, rig, polecat string) (time.Time, bool)
	// nudge delivers msg to the seat's session.
	nudge func(seat overrunSeat, msg string) error
	// stop ends the seat's session after elapsed.
	stop func(seat overrunSeat, elapsed time.Duration) error
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

// triggerOverrunGuard runs an overrun_guard cycle when the patrol is due, on
// its own goroutine. Due-ness comes from the persisted last-run time, as for
// checkpoint_dog (triggerCheckpointDog), so a daemon restart does not reset
// the cadence.
func (d *Daemon) triggerOverrunGuard() {
	if !d.isPatrolActive("overrun_guard") {
		return
	}

	dec := evaluatePatrolDue(d.config.TownRoot, "overrun_guard", time.Time{}, time.Now(), overrunGuardInterval(d.patrolConfig))
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
			if err := savePatrolLastRun(d.config.TownRoot, "overrun_guard", time.Now()); err != nil {
				d.logger.Printf("overrun_guard: WARNING: cannot persist last-run time (%v)", err)
			}
		}()
		d.runOverrunGuard(d.overrunProdSeams())
	}()
}

// runOverrunGuard judges every working seat once. A seat that is submitted,
// parked or undated is left alone. At the nudge threshold the seat is nudged
// once per sling, remembered in the bead's state file; past the stop
// threshold the stop seam runs. In dry-run mode nothing is sent, stopped or
// written, and the log says what would have happened.
func (d *Daemon) runOverrunGuard(s overrunSeams) {
	if !d.isPatrolActive("overrun_guard") {
		return
	}
	if s.now == nil {
		s.now = time.Now
	}
	nudgeAfter, stopAfter := overrunGuardThresholds(d.patrolConfig)
	dry := false
	if cfg := overrunGuardConfig(d.patrolConfig); cfg != nil {
		dry = cfg.DryRun
	}

	seats, err := s.workingSeats()
	if err != nil {
		d.logger.Printf("overrun_guard: cannot list working seats: %v", err)
		return
	}
	now := s.now()

	for _, seat := range seats {
		if seat.Submitted || seat.Parked || seat.Bead == "" {
			continue
		}
		who := seat.Rig + "/" + seat.Polecat
		slung, ok := s.slungAt(seat.Bead, seat.Rig, seat.Polecat)
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

		// A nudge sent before this sling belongs to an earlier run of the
		// bead (a rework re-sling starts a new clock).
		nudged := !state.NudgedAt.IsZero() && !state.NudgedAt.Before(slung)
		action := decideOverrun(overrunInput{
			Now: now, SlungAt: slung, Nudged: nudged,
			NudgeAt: nudgeAfter, StopAt: stopAfter,
		})
		elapsed := now.Sub(slung)

		switch action {
		case overrunNudge:
			if dry {
				d.logger.Printf("overrun_guard: dry run: would nudge %s on %s after %s", who, seat.Bead, elapsed.Round(time.Second))
				continue
			}
			if err := s.nudge(seat, overrunNudgeMessage); err != nil {
				d.logger.Printf("overrun_guard: nudge %s on %s failed: %v", who, seat.Bead, err)
				continue
			}
			state.NudgedAt = now
			if err := writeOverrunState(d.config.TownRoot, state); err != nil {
				d.logger.Printf("overrun_guard: WARNING: %s nudged on %s but state not saved (%v) — it may be nudged again", who, seat.Bead, err)
				continue
			}
			d.logger.Printf("overrun_guard: nudged %s on %s after %s", who, seat.Bead, elapsed.Round(time.Second))
		case overrunStop:
			if dry {
				d.logger.Printf("overrun_guard: dry run: would stop %s on %s after %s", who, seat.Bead, elapsed.Round(time.Second))
				continue
			}
			if err := s.stop(seat, elapsed); err != nil {
				d.logger.Printf("overrun_guard: stop %s on %s failed: %v", who, seat.Bead, err)
			}
		}
	}
}

// overrunProdSeams returns the production seams: the daemon's rigs, event
// log and notifier. The stop only logs until the real stop is built.
func (d *Daemon) overrunProdSeams() overrunSeams {
	sl := &overrunSlungReader{d: d}
	return overrunSeams{
		now:          time.Now,
		workingSeats: d.overrunWorkingSeats,
		slungAt:      sl.slungAt,
		nudge: func(seat overrunSeat, msg string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return d.notify().Nudge(ctx, seat.Rig+"/"+seat.Polecat, msg)
		},
		stop: func(seat overrunSeat, elapsed time.Duration) error {
			d.logger.Printf("overrun_guard: %s/%s on %s is past the stop threshold (%s); stopping is not built yet",
				seat.Rig, seat.Polecat, seat.Bead, elapsed.Round(time.Second))
			return nil
		},
	}
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
	if seat.Parked || seat.Submitted {
		return seat, false // nothing to time, and no reason to spend a bd read
	}

	sessionName := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(rigName), name)
	alive, err := d.tmux.HasSession(sessionName)
	if err != nil || !alive {
		return seat, false
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
	closed, submitted, _ := d.beadFinished(bead)
	if closed {
		return seat, false
	}
	seat.Bead = bead
	seat.Submitted = submitted

	if workDir := resolvePolecatWorktree(polecatsDir, name, rigName); workDir != "" {
		if branch, err := d.gitAt(workDir).CurrentBranch(); err == nil {
			seat.Branch = branch
		}
	}
	return seat, true
}

// overrunSlungReader dates slings from the town event log, reading it once
// per cycle.
type overrunSlungReader struct {
	d      *Daemon
	loaded bool
	evs    []events.Event
}

// slungAt is the time of the latest sling event that sent bead to
// <rig>/polecats/<polecat>. With no such event it falls back to the seat's
// agent bead update time, which can only read short, so the fallback errs
// toward not stopping anyone.
func (r *overrunSlungReader) slungAt(bead, rig, polecatName string) (time.Time, bool) {
	if !r.loaded {
		r.loaded = true
		evs, err := events.Read(r.d.config.TownRoot)
		if err != nil {
			r.d.logger.Printf("overrun_guard: reading events: %v", err)
		}
		r.evs = evs
	}
	target := rig + "/polecats/" + polecatName
	var latest time.Time
	for _, ev := range r.evs {
		if ev.Type != events.TypeSling {
			continue
		}
		if b, _ := ev.Payload["bead"].(string); b != bead {
			continue
		}
		if t, _ := ev.Payload["target"].(string); t != target {
			continue
		}
		ts, err := time.Parse(time.RFC3339, ev.Timestamp)
		if err != nil {
			continue
		}
		if ts.After(latest) {
			latest = ts
		}
	}
	if !latest.IsZero() {
		return latest, true
	}

	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(r.d.config.TownRoot, rig), rig, polecatName)
	issue, err := r.d.workBeads(r.d.workBeadsEnv(rig), workBeadReadTimeout).Show(id)
	if err != nil {
		return time.Time{}, false
	}
	if t := polecat.AgentBeadUpdatedAt(issue); !t.IsZero() {
		return t, true
	}
	return time.Time{}, false
}
