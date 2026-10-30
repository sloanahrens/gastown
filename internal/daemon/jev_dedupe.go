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
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_dedupe", d.jevDedupeRun.last(), now, jevDedupeInterval)
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
		d.recordPatrolRun(&d.jevDedupeRun, "jev_dedupe", d.clk().Now())
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
			Scrub:               c.Patrols.JevDedupe.Scrub,
			ScrubCheck:          c.Patrols.JevDedupe.ScrubCheck,
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
	// The E-stop holds the patrol from starting (the step is lifecycle); the
	// gate stops a pass already in flight between billed calls.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
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

// jevAsker is the one Ask call every Jev pass's Asker interface declares, so
// a single gate serves all of them.
type jevAsker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// estopGatedAsker is the E-stop's check between one billed call and the next
// in a running pass. The pass's own loop asks before the call, so a call in
// flight finishes and is logged and recorded: ending the pass from the
// context leaves nothing half-asked that the next pass would bill again. Every
// later item is left for a pass run after the E-stop lifts (gt-0h4i0,
// gt-tevxz).
type estopGatedAsker struct {
	inner jevAsker
	// cancel ends the pass's context, which each pass's Run reads after the
	// call in flight and reports instead of asking the next item.
	cancel context.CancelFunc
	// active reports whether the town E-stop is up now.
	active func() bool
	// stopped is true once the gate has ended the pass. It stays true, and
	// every later Ask is refused, so a Run that did not honor the cancellation
	// cannot bill again if the E-stop lifts mid-pass. The pass runs on one
	// goroutine, so this needs no lock.
	stopped bool
}

// newEstopGate gates inner on the town E-stop, ending the pass through cancel.
func newEstopGate(townRoot string, inner jevAsker, cancel context.CancelFunc) *estopGatedAsker {
	return &estopGatedAsker{
		inner:  inner,
		cancel: cancel,
		active: func() bool { return estop.IsActive(townRoot) },
	}
}

func (a *estopGatedAsker) Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	if !a.stopped && a.active() {
		a.stopped = true
		a.cancel()
	}
	if a.stopped {
		return nil, context.Canceled
	}
	return a.inner.Ask(ctx, state, qs)
}
