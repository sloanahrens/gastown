package daemon

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
	jevrules "github.com/steveyegge/gastown/internal/jev/rules"
)

const (
	// jevRulesInterval is how often a pass is due. A pass takes the submissions
	// made since the last one, so the interval is the most a submission waits
	// before it is asked about.
	jevRulesInterval = 15 * time.Minute

	// jevRulesRunBudget bounds one pass: the submission lists and the Jev calls
	// for every submission it reaches.
	jevRulesRunBudget = 10 * time.Minute
)

// triggerJevRules starts one pass on its own goroutine when the patrol is on
// and due. The pass makes network calls, so it never runs on the heartbeat. A
// single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevRules() {
	if !d.isPatrolActive("jev_rules") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_rules", d.jevRulesRun.last(), now, jevRulesInterval)
	if !dec.due {
		return
	}
	if !d.jevRulesRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_rules: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevRulesRunning.Store(false)
		d.runJevRules()
		d.recordPatrolRun(&d.jevRulesRun, "jev_rules", d.clk().Now())
	}()
}

// jevRulesRigs returns the known rigs that have a beads directory. A rig that
// is registered but never cloned has none, and the pass's open fails on one of
// them, so skipping it here keeps one such rig from stopping the pass for all
// the others (gt-csq9g).
func (d *Daemon) jevRulesRigs() []string {
	var rigs []string
	for _, rig := range d.getKnownRigs() {
		dir := doltserver.FindRigBeadsDir(d.config.TownRoot, rig)
		if _, err := os.Stat(dir); err != nil {
			// Only a missing directory means the rig was never cloned. Any
			// other stat failure is reported as what it is, so an unreadable
			// path does not read as a rig the town knows is uncloned (gt-8chz5).
			if errors.Is(err, os.ErrNotExist) {
				d.logger.Printf("jev_rules: skipping rig %s: no beads directory at %s", rig, dir)
			} else {
				d.logger.Printf("jev_rules: WARNING: skipping rig %s: cannot stat %s: %v", rig, dir, err)
			}
			continue
		}
		rigs = append(rigs, rig)
	}
	return rigs
}

// runJevRules does one pass with the settings in the patrol config and the
// rigs the town knows. A failure is logged, and the last-run time still
// advances, so a missing price or an unreadable bead store costs one line per
// interval rather than one per heartbeat.
func (d *Daemon) runJevRules() {
	settings := jevrules.Settings{Rigs: d.jevRulesRigs()}
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevRules != nil {
		settings.MaxUSDPerDay = c.Patrols.JevRules.MaxUSDPerDay
		settings.USDPerBillionTokens = c.Patrols.JevRules.USDPerBillionTokens
		settings.Scrub = c.Patrols.JevRules.Scrub
	}
	open := d.jevRulesOpen
	if open == nil {
		open = jevrules.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_rules: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevRulesRunBudget)
	defer cancel()
	// The E-stop gate stops a pass already in flight between billed calls.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_rules: stopped: E-STOP active after asking %d session(s)", sum.Asked)
		return
	}
	if errors.Is(err, jevrules.ErrBusy) {
		d.logger.Printf("jev_rules: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_rules: error: %v", err)
		return
	}
	if sum.Asked > 0 || sum.Unanswered > 0 || sum.Refused > 0 || sum.CapReached {
		d.logger.Printf("jev_rules: asked %d session(s), %d unanswered, %d refused by the containment check (daily cap reached: %v)",
			sum.Asked, sum.Unanswered, sum.Refused, sum.CapReached)
	}
}
