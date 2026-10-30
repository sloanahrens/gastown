package daemon

import (
	"context"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/jev/progress"
	"github.com/steveyegge/gastown/internal/session"
)

// jevProgressRunBudget bounds one pass: the Jev calls for every nudge it
// reaches. Each call has its own timeout inside the client.
const jevProgressRunBudget = 5 * time.Minute

// enqueueJevProgress queues a nudge the overrun guard just delivered, so the
// progress pass asks about the seat on the next heartbeat. The transcript is
// looked up now, from the seat's worktree, while it is the live one. It does
// nothing when the patrol is off, so a town that has not opted in queues
// nothing.
func (d *Daemon) enqueueJevProgress(seat overrunSeat, at time.Time) {
	if !d.isPatrolActive("jev_progress") {
		return
	}
	latest := d.jevProgressLatest
	if latest == nil {
		latest = func(workDir string) (string, error) { return agentlog.LatestTranscript(d.config.TownRoot, workDir) }
	}
	polecatsDir := filepath.Join(d.config.TownRoot, seat.Rig, "polecats")
	var transcript, worktree string
	if workDir := resolvePolecatWorktree(polecatsDir, seat.Polecat, seat.Rig); workDir != "" {
		if path, err := latest(workDir); err == nil {
			transcript = path
			worktree = workDir
		}
	}
	n := progress.Nudge{
		Rig:        seat.Rig,
		Polecat:    seat.Polecat,
		Bead:       seat.Bead,
		Session:    session.PolecatSessionName(d.prefixRegistry().PrefixForRig(seat.Rig), seat.Polecat),
		At:         at,
		Transcript: transcript,
		Worktree:   worktree,
	}
	if !d.jevProgressQueue.Push(n) {
		d.logger.Printf("jev_progress: queue full, dropped the nudge to %s/%s on %s", seat.Rig, seat.Polecat, seat.Bead)
	}
}

// triggerJevProgress starts one pass on its own goroutine when the patrol is
// on and a nudge is queued. It is not due on an interval: a nudge waits for
// the next heartbeat, so the answer arrives while the seat is still inside its
// clock. A single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevProgress() {
	if !d.isPatrolActive("jev_progress") {
		return
	}
	if d.jevProgressQueue.Len() == 0 {
		return
	}
	if !d.jevProgressRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.jevProgressRunning.Store(false)
		d.runJevProgress()
	}()
}

// runJevProgress does one pass with the settings in the patrol config. When
// the pass cannot be opened, the queued nudges are dropped and logged: the
// seat has moved on by the next heartbeat, and a missing price should cost one
// line per nudge, not a growing queue.
func (d *Daemon) runJevProgress() {
	var settings progress.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevProgress != nil {
		settings = progress.Settings{
			MaxUSDPerDay:        c.Patrols.JevProgress.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevProgress.USDPerBillionTokens,
			Scrub:               c.Patrols.JevProgress.Scrub,
		}
	}
	open := d.jevProgressOpen
	if open == nil {
		open = progress.Open
	}
	pass, err := open(d.config.TownRoot, &d.jevProgressQueue, settings, d.logger.Writer())
	if err != nil {
		dropped := len(d.jevProgressQueue.Drain())
		d.logger.Printf("jev_progress: could not open the pass: %v (dropped %d nudges)", err, dropped)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevProgressRunBudget)
	defer cancel()
	// The E-stop holds the patrol from starting, but a pass already in flight
	// went on paying for every queued nudge (gt-dlnkz). The gate checks
	// between billed calls, as jev_dedupe's does (gt-0h4i0). The nudges the
	// stop cut short were drained already and are dropped, as a stopped pass's
	// are.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_progress: stopped: E-STOP active after asking %d nudge(s) (dropped %d)", sum.Asked, sum.Dropped)
		return
	}
	if err != nil {
		// A stopped pass logs what it reached before it stopped, so its line
		// never reads like a clean pass that dropped nothing.
		d.logger.Printf("jev_progress: pass stopped: %v (asked %d, unanswered %d, dropped %d)",
			err, sum.Asked, sum.Unanswered, sum.Dropped)
		return
	}
	if sum.Asked > 0 || sum.Unanswered > 0 || sum.Dropped > 0 || sum.CapReached {
		d.logger.Printf("jev_progress: asked %d nudges, %d unanswered, %d dropped (daily cap reached: %v)",
			sum.Asked, sum.Unanswered, sum.Dropped, sum.CapReached)
	}
}
