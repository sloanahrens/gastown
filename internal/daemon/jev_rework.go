package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/jev/rework"
)

const (
	// jevReworkInterval is how often a pass is due. A pass takes the refusals
	// since the last one, so the interval is the most a refused review waits
	// before it is asked about.
	jevReworkInterval = 15 * time.Minute

	// jevReworkRunBudget bounds one pass: the bead reads, the diff and the Jev
	// call for every refusal it reaches.
	jevReworkRunBudget = 10 * time.Minute
)

// triggerJevRework starts one pass on its own goroutine when the patrol is on
// and due. The pass makes network calls and runs git, so it never runs on the
// heartbeat. A single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevRework() {
	if !d.isPatrolActive("jev_rework") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_rework", d.jevReworkRun.last(), now, jevReworkInterval)
	if !dec.due {
		return
	}
	if !d.jevReworkRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_rework: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevReworkRunning.Store(false)
		d.runJevRework()
		d.recordPatrolRun(&d.jevReworkRun, "jev_rework", d.clk().Now())
	}()
}

// runJevRework does one pass with the settings in the patrol config. A failure
// is logged, and the last-run time still advances, so a missing price or a
// down Dolt costs one line per interval rather than one per heartbeat.
func (d *Daemon) runJevRework() {
	var settings rework.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevRework != nil {
		settings = rework.Settings{
			MaxUSDPerDay:        c.Patrols.JevRework.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevRework.USDPerBillionTokens,
			Scrub:               c.Patrols.JevRework.Scrub,
		}
	}
	open := d.jevReworkOpen
	if open == nil {
		open = rework.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_rework: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevReworkRunBudget)
	defer cancel()
	// The E-stop gate stops a pass already in flight between billed calls.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_rework: stopped: E-STOP active after asking %d refusal(s)", sum.Asked)
		return
	}
	if errors.Is(err, rework.ErrBusy) {
		d.logger.Printf("jev_rework: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_rework: error: %v", err)
		return
	}
	if sum.Asked > 0 || sum.Skipped > 0 || sum.CapReached {
		d.logger.Printf("jev_rework: asked %d refusals, skipped %d (daily cap reached: %v)", sum.Asked, sum.Skipped, sum.CapReached)
	}
}
