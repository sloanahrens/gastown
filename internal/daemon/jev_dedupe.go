package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/jev/dedupe"
)

const (
	// jevDedupeInterval is how often a pass is due. A pass takes the findings
	// filed since the last one, so the interval is the most a finding waits
	// before it is asked about.
	jevDedupeInterval = 15 * time.Minute

	// jevDedupeRunBudget bounds one pass: the hm queries and the Jev calls
	// for every finding it reaches.
	jevDedupeRunBudget = 10 * time.Minute
)

// triggerJevDedupe starts one pass on its own goroutine when the patrol is on
// and due. The pass makes network calls and runs hm, so it never runs on the
// heartbeat. A single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevDedupe() {
	if !d.isPatrolActive("jev_dedupe") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_dedupe", time.Time{}, now, jevDedupeInterval)
	if !dec.due {
		return
	}
	if !d.jevDedupeRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_dedupe: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevDedupeRunning.Store(false)
		d.runJevDedupe()
		if err := savePatrolLastRun(d.config.TownRoot, "jev_dedupe", d.clk().Now()); err != nil {
			d.logger.Printf("jev_dedupe: WARNING: cannot persist last-run time (%v)", err)
		}
	}()
}

// runJevDedupe does one pass with the settings in the patrol config. A
// failure is logged, and the last-run time still advances, so a missing
// price or a down hm costs one line per interval rather than one per
// heartbeat.
func (d *Daemon) runJevDedupe() {
	var settings dedupe.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevDedupe != nil {
		settings = dedupe.Settings{
			MaxUSDPerDay:        c.Patrols.JevDedupe.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevDedupe.USDPerBillionTokens,
		}
	}
	open := d.jevDedupeOpen
	if open == nil {
		open = dedupe.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_dedupe: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevDedupeRunBudget)
	defer cancel()
	sum, err := pass.Run(ctx)
	if errors.Is(err, dedupe.ErrBusy) {
		d.logger.Printf("jev_dedupe: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_dedupe: error: %v", err)
		return
	}
	if sum.Asked > 0 || sum.CapReached {
		d.logger.Printf("jev_dedupe: asked %d findings (daily cap reached: %v)", sum.Asked, sum.CapReached)
	}
}
