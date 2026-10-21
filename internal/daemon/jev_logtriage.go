package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/jev/logtriage"
)

const (
	// jevLogtriageInterval is how often a pass is due. A pass reads the daemon
	// log's lines since the last one, so the interval is the most a warning
	// waits before it is triaged.
	jevLogtriageInterval = 15 * time.Minute

	// jevLogtriageRunBudget bounds one pass: the log read and the Jev call for
	// each group it reaches, at most MaxGroupsPerPass of them.
	jevLogtriageRunBudget = 10 * time.Minute
)

// triggerJevLogtriage starts one pass on its own goroutine when the patrol is
// on and due. The pass reads the daemon log and makes network calls, so it
// never runs on the heartbeat. A single-flight guard keeps one pass in flight
// at a time.
func (d *Daemon) triggerJevLogtriage() {
	if !d.isPatrolActive("jev_logtriage") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_logtriage", time.Time{}, now, jevLogtriageInterval)
	if !dec.due {
		return
	}
	if !d.jevLogtriageRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_logtriage: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevLogtriageRunning.Store(false)
		d.runJevLogtriage()
		if err := savePatrolLastRun(d.config.TownRoot, "jev_logtriage", d.clk().Now()); err != nil {
			d.logger.Printf("jev_logtriage: WARNING: cannot persist last-run time (%v)", err)
		}
	}()
}

// runJevLogtriage does one pass with the settings in the patrol config. A
// failure is logged, and the last-run time still advances, so a missing price
// costs one line per interval rather than one per heartbeat.
func (d *Daemon) runJevLogtriage() {
	var settings logtriage.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevLogtriage != nil {
		settings = logtriage.Settings{
			MaxUSDPerDay:        c.Patrols.JevLogtriage.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevLogtriage.USDPerBillionTokens,
		}
	}
	open := d.jevLogtriageOpen
	if open == nil {
		open = logtriage.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_logtriage: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevLogtriageRunBudget)
	defer cancel()
	sum, err := pass.Run(ctx)
	if errors.Is(err, logtriage.ErrBusy) {
		d.logger.Printf("jev_logtriage: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_logtriage: error: %v", err)
		return
	}
	if sum.Asked > 0 || sum.Unanswered > 0 || sum.Deferred > 0 || sum.Dropped > 0 || sum.CapReached {
		d.logger.Printf("jev_logtriage: asked %d groups, %d unanswered, %d deferred to the next pass, %d dropped from %d matched lines (daily cap reached: %v)",
			sum.Asked, sum.Unanswered, sum.Deferred, sum.Dropped, sum.Matched, sum.CapReached)
	}
}
