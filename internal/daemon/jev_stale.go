package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/stale"
)

const (
	// jevStaleInterval is how often a pass is due: once a week. A bead is
	// asked about at most once in thirty days, whatever the interval.
	jevStaleInterval = 7 * 24 * time.Hour

	// jevStaleRunBudget bounds one pass: the Jev calls for every bead it
	// reaches. Each call has its own timeout inside the client.
	jevStaleRunBudget = 2 * time.Hour
)

// triggerJevStale starts one pass on its own goroutine when the patrol is on
// and a week has passed since the last one. A pass makes network calls and
// runs hm, so it never runs on the heartbeat. A single-flight guard keeps one
// pass in flight at a time.
func (d *Daemon) triggerJevStale() {
	if !d.isPatrolActive("jev_stale") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_stale", d.jevStaleRun.last(), now, jevStaleInterval)
	if !dec.due {
		return
	}
	if !d.jevStaleRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_stale: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevStaleRunning.Store(false)
		if !d.runJevStale() {
			return
		}
		d.recordPatrolRun(&d.jevStaleRun, "jev_stale", d.clk().Now())
	}()
}

// runJevStale does one pass over the operational rigs with the settings in the
// patrol config. It reports false when the E-stop ended the pass: that week is
// not spent, so the next heartbeat after the E-stop clears runs the pass again,
// and the log keeps it from asking about the same beads twice. Any other end,
// including a failure, reports true and the last-run time advances, so a
// missing price or a down hm costs one line a week rather than one per
// heartbeat.
func (d *Daemon) runJevStale() bool {
	var settings stale.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevStale != nil {
		settings = stale.Settings{
			MaxUSDPerDay:        c.Patrols.JevStale.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevStale.USDPerBillionTokens,
			Scrub:               c.Patrols.JevStale.Scrub,
		}
	}
	open := d.jevStaleOpen
	if open == nil {
		open = stale.Open
	}
	pass, err := open(d.config.TownRoot, d.getPatrolRigs("jev_stale"), settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_stale: error: %v", err)
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevStaleRunBudget)
	defer cancel()
	// The E-stop holds the patrol from starting (the step is lifecycle), but a
	// pass already in flight would go on paying until the daily cap or its
	// budget. The gate checks between billed calls, as jev_dedupe's does.
	gate := &staleEstopGatedAsker{
		Asker:  pass.Asker,
		cancel: cancel,
		active: func() bool { return estop.IsActive(d.config.TownRoot) },
	}
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_stale: stopped: E-STOP active after asking %d bead(s)", sum.Asked)
		return false
	}
	if errors.Is(err, stale.ErrBusy) {
		d.logger.Printf("jev_stale: skipped: %v", err)
		return true
	}
	if err != nil {
		d.logger.Printf("jev_stale: error: %v", err)
		return true
	}
	if sum.Asked > 0 || sum.Unanswered > 0 || sum.Deferred > 0 || sum.CapReached || sum.UnreadRigs > 0 {
		d.logger.Printf("jev_stale: asked %d beads, %d unanswered, %d deferred, %d rigs unread (daily cap reached: %v)",
			sum.Asked, sum.Unanswered, sum.Deferred, sum.UnreadRigs, sum.CapReached)
	}
	return true
}

// staleEstopGatedAsker is the E-stop's check between one billed call and the
// next in a running pass. The call in flight finishes and is logged, and the
// pass ends from its context, so no bead is left half-asked for the next pass
// to bill again.
type staleEstopGatedAsker struct {
	stale.Asker
	// cancel ends the pass's context, which stale.Run reads after the call in
	// flight and reports instead of asking the next bead.
	cancel context.CancelFunc
	// active reports whether the town E-stop is up now.
	active func() bool
	// stopped is true once the gate has ended the pass. The pass runs on one
	// goroutine, so this needs no lock.
	stopped bool
}

func (a *staleEstopGatedAsker) Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	if a.active() {
		a.stopped = true
		a.cancel()
		return nil, context.Canceled
	}
	return a.Asker.Ask(ctx, state, qs)
}
