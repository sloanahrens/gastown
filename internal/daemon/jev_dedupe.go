package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/jev"
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
	// The E-stop holds the patrol from starting (the step is lifecycle), but a
	// pass already in flight went on paying until the daily cap or its budget
	// (gt-0h4i0). The gate checks between billed calls.
	gate := &estopGatedAsker{
		Asker:  pass.Asker,
		cancel: cancel,
		active: func() bool { return estop.IsActive(d.config.TownRoot) },
	}
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_dedupe: stopped: E-STOP active after asking %d finding(s)", sum.Asked)
		return
	}
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

// estopGatedAsker is the E-stop's check between one billed call and the next
// in a running pass. The pass's own loop asks before the call, so a call in
// flight finishes and is logged and recorded: ending the pass from the
// context leaves nothing half-asked that the next pass would bill again. Every
// later finding is left for a pass run after the E-stop lifts.
type estopGatedAsker struct {
	dedupe.Asker
	// cancel ends the pass's context, which dedupe.Run reads after the call
	// in flight and reports instead of asking the next finding.
	cancel context.CancelFunc
	// active reports whether the town E-stop is up now.
	active func() bool
	// stopped is true once the gate has ended the pass. The pass runs on one
	// goroutine, so this needs no lock.
	stopped bool
}

func (a *estopGatedAsker) Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	if a.active() {
		a.stopped = true
		a.cancel()
		return nil, context.Canceled
	}
	return a.Asker.Ask(ctx, state, qs)
}
