package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/estop"
	jevattention "github.com/steveyegge/gastown/internal/jev/attention"
)

const (
	// jevAttentionInterval is how often a pass is due. A pass takes the items
	// raised since the last one, so the interval is the most an item waits
	// before it is asked about.
	jevAttentionInterval = 15 * time.Minute

	// jevAttentionRunBudget bounds one pass: the Jev calls for every item it
	// reaches.
	jevAttentionRunBudget = 10 * time.Minute
)

// triggerJevAttention starts one pass on its own goroutine when the patrol is
// on and due. The pass makes network calls, so it never runs on the heartbeat.
// A single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevAttention() {
	if !d.isPatrolActive("jev_attention") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_attention", time.Time{}, now, jevAttentionInterval)
	if !dec.due {
		return
	}
	if !d.jevAttentionRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_attention: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevAttentionRunning.Store(false)
		d.runJevAttention()
		if err := savePatrolLastRun(d.config.TownRoot, "jev_attention", d.clk().Now()); err != nil {
			d.logger.Printf("jev_attention: WARNING: cannot persist last-run time (%v)", err)
		}
	}()
}

// runJevAttention does one pass with the settings in the patrol config. A
// failure is logged, and the last-run time still advances, so a missing price
// or a failed read costs one line per interval rather than one per heartbeat.
func (d *Daemon) runJevAttention() {
	var settings jevattention.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevAttention != nil {
		settings = jevattention.Settings{
			MaxUSDPerDay:        c.Patrols.JevAttention.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevAttention.USDPerBillionTokens,
		}
	}
	open := d.jevAttentionOpen
	if open == nil {
		open = jevattention.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_attention: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevAttentionRunBudget)
	defer cancel()
	// The E-stop holds the patrol from starting, but a pass already in flight
	// went on paying until the daily cap or its budget (gt-tevxz). The gate
	// checks between billed calls, as jev_dedupe's does (gt-0h4i0).
	gate := &estopGatedAsker{
		Asker:  pass.Asker,
		cancel: cancel,
		active: func() bool { return estop.IsActive(d.config.TownRoot) },
	}
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_attention: stopped: E-STOP active after asking %d item(s)", sum.Asked)
		return
	}
	if errors.Is(err, jevattention.ErrBusy) {
		d.logger.Printf("jev_attention: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_attention: error: %v", err)
		return
	}
	if sum.Asked > 0 || sum.Failed > 0 || sum.Held > 0 || sum.CapReached {
		d.logger.Printf("jev_attention: %s", sum.Report())
	}
}
