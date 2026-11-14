package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/jev/omclass"
)

const (
	// jevOMClassInterval is how often a pass is due. A pass takes the findings
	// since the last one, so the interval is the most a new finding waits before
	// it is asked about.
	jevOMClassInterval = 15 * time.Minute

	// jevOMClassRunBudget bounds one pass: the Jev call for every finding it
	// takes, up to omclass.MaxFindingsPerRun.
	jevOMClassRunBudget = 10 * time.Minute
)

// triggerJevOMClass starts one pass on its own goroutine when the patrol is on
// and due. The pass makes network calls, so it never runs on the heartbeat. A
// single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevOMClass() {
	if !d.isPatrolActive("jev_omclass") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_omclass", d.jevOMClassRun.last(), now, jevOMClassInterval)
	if !dec.due {
		return
	}
	if !d.jevOMClassRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_omclass: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevOMClassRunning.Store(false)
		d.runJevOMClass()
		d.recordPatrolRun(&d.jevOMClassRun, "jev_omclass", d.clk().Now())
	}()
}

// runJevOMClass does one pass with the settings in the patrol config. A failure
// is logged, and the last-run time still advances, so a missing price or a
// down Dolt costs one line per interval rather than one per heartbeat.
func (d *Daemon) runJevOMClass() {
	var settings omclass.Settings
	if c := d.patrol(); c != nil && c.Patrols != nil && c.Patrols.JevOMClass != nil {
		settings = omclass.Settings{
			MaxUSDPerDay:        c.Patrols.JevOMClass.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevOMClass.USDPerBillionTokens,
			Scrub:               c.Patrols.JevOMClass.Scrub,
			ScrubCheck:          c.Patrols.JevOMClass.ScrubCheck,
		}
	}
	open := d.jevOMClassOpen
	if open == nil {
		open = omclass.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_omclass: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevOMClassRunBudget)
	defer cancel()
	// The E-stop gate stops a pass already in flight between billed calls.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_omclass: stopped: E-STOP active after asking %d finding(s)", sum.Asked)
		return
	}
	if errors.Is(err, omclass.ErrBusy) {
		d.logger.Printf("jev_omclass: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_omclass: error: %v", err)
		return
	}
	if sum.Started {
		d.logger.Printf("jev_omclass: first pass set the cursor; findings written before it are not asked")
	}
	if sum.Capped {
		d.logger.Printf("jev_omclass: stopped at %d findings this run; the rest wait for the next pass", omclass.MaxFindingsPerRun)
	}
	if sum.Asked > 0 || sum.Skipped > 0 || sum.Failed > 0 || sum.CapReached {
		d.logger.Printf("jev_omclass: asked %d findings, skipped %d, gave up on %d (daily cap reached: %v)", sum.Asked, sum.Skipped, sum.Failed, sum.CapReached)
	}
}
