package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/jev/landing"
)

const (
	// jevLandingInterval is how often a pass is due. A pass takes the
	// landings since the last one, so the interval is the most a landing waits
	// before it is asked about.
	jevLandingInterval = 15 * time.Minute

	// jevLandingRunBudget bounds one pass: the git diff and the Jev call for
	// every landing it reaches.
	jevLandingRunBudget = 10 * time.Minute
)

// triggerJevLanding starts one pass on its own goroutine when the patrol is on
// and due. The pass makes network calls and runs git, so it never runs on the
// heartbeat. A single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevLanding() {
	if !d.isPatrolActive("jev_landing") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_landing", d.jevLandingRun.last(), now, jevLandingInterval)
	if !dec.due {
		return
	}
	if !d.jevLandingRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_landing: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevLandingRunning.Store(false)
		d.runJevLanding()
		d.recordPatrolRun(&d.jevLandingRun, "jev_landing", d.clk().Now())
	}()
}

// runJevLanding does one pass with the settings in the patrol config. A
// failure is logged, and the last-run time still advances, so a missing price
// costs one line per interval rather than one per heartbeat.
func (d *Daemon) runJevLanding() {
	var settings landing.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevLanding != nil {
		settings = landing.Settings{
			MaxUSDPerDay:        c.Patrols.JevLanding.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevLanding.USDPerBillionTokens,
			Scrub:               c.Patrols.JevLanding.Scrub,
			ScrubCheck:          c.Patrols.JevLanding.ScrubCheck,
		}
	}
	open := d.jevLandingOpen
	if open == nil {
		open = landing.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_landing: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevLandingRunBudget)
	defer cancel()
	// The E-stop gate stops a pass already in flight between billed calls.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_landing: stopped: E-STOP active after asking %d landing(s)", sum.Asked)
		return
	}
	if errors.Is(err, landing.ErrBusy) {
		d.logger.Printf("jev_landing: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_landing: error: %v", err)
		return
	}
	if sum.Asked > 0 || sum.Skipped > 0 || sum.CapReached {
		d.logger.Printf("jev_landing: asked %d landings, skipped %d (daily cap reached: %v)", sum.Asked, sum.Skipped, sum.CapReached)
	}
}
