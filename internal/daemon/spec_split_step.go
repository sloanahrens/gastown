package daemon

// The spec_split step (gt-tal4li): the split route's last piece, which turns a
// bead the dispatcher labeled split into its two chained pieces. The route
// ships off; the patrols.spec_split block says why and how to turn it on
// (internal/config/daemon_config.go).
//
// One tick splits at most specSplitMaxPerTick beads that carry the split label
// and no assignee, and runs the same apply `gt spec split --apply` runs, in
// this process. The apply lives in package cmd, which starts the daemon, so it
// arrives through SetSpecSplitDeps rather than the daemon reaching back for it
// (the SetSchedulerDeps seam, gt-638go.9). A bead with an assignee, the rework
// label or a hold is never taken. What a refused apply does to its bead is the
// next bead's (gt-hrbq3v): this tick logs the reason and leaves the bead open.
//
// The tick runs on its own goroutine, one at a time: an apply estimates and
// lints both pieces, so it can take minutes and must not hold the heartbeat. A
// tick in flight is hard daemon work: a cut mid-apply leaves the pieces it
// created with no undo (the apply's own undo runs on a returned error, not on
// a killed process), so the upgrade restart waits for it as it does for a
// scheduled slings cycle.

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

const (
	// specSplitPatrol is the step's name in daemon.patrols and in the daemon
	// audit table.
	specSplitPatrol = "spec_split"

	// specSplitMaxPerTick is how many beads one tick splits. A split creates
	// two beads and moves the original's dependents, so a tick does only a
	// few before the next heartbeat.
	specSplitMaxPerTick = 2

	// specSplitMaxAttempts bounds the applies one tick runs. A refused apply
	// uses no split slot, so a refusal does not starve the beads behind it;
	// this cap keeps a long list of refusing beads from running an apply, with
	// its estimate and lint, per bead in one tick.
	specSplitMaxAttempts = 4
)

// SpecSplitDeps are the collaborators the spec_split step runs on.
type SpecSplitDeps struct {
	// Candidates lists the beads a tick may consider; nil reads the town's own.
	Candidates func() ([]*beads.Issue, error)
	// Apply splits one bead as `gt spec split --apply` does and returns its report.
	Apply func(townRoot, beadID string) (report string, err error)
}

// SetSpecSplitDeps installs the apply the spec_split step runs.
func (d *Daemon) SetSpecSplitDeps(deps SpecSplitDeps) {
	d.specSplitDeps = deps
}

// specSplitStep is the heartbeat entry: it starts one tick on its own
// goroutine and skips when the previous tick is still running.
func (d *Daemon) specSplitStep() {
	if !d.isPatrolActive(specSplitPatrol) {
		return
	}
	if !d.specSplitRunning.CompareAndSwap(false, true) {
		d.logger.Printf("spec_split: the previous tick is still running, so this one is skipped")
		return
	}
	d.specSplitCycles.Add(1)
	go func() {
		defer d.specSplitCycles.Done()
		defer d.specSplitRunning.Store(false)
		d.specSplitTick()
	}()
}

// specSplitTick runs one tick: the read, then an apply per eligible bead while
// a split slot and an attempt remain. A read that fails is logged and splits
// nothing: it must not read as a town with no split-labeled bead.
func (d *Daemon) specSplitTick() {
	if d.config == nil {
		d.logger.Printf("spec_split: no town root is known, so no bead is split")
		return
	}
	if d.specSplitDeps.Apply == nil {
		d.logger.Printf("spec_split: no apply is installed, so no bead is split")
		return
	}
	candidates := d.specSplitDeps.Candidates
	if candidates == nil {
		candidates = d.specSplitCandidates
	}
	issues, err := candidates()
	if err != nil {
		d.logger.Printf("spec_split: cannot read the split-labeled beads: %v", err)
		return
	}
	start := d.clk().Now()
	splits, attempts := 0, 0
	for _, issue := range issues {
		if splits == specSplitMaxPerTick || attempts == specSplitMaxAttempts {
			break
		}
		if !specSplitEligible(issue) {
			continue
		}
		attempts++
		report, err := d.specSplitDeps.Apply(d.config.TownRoot, issue.ID)
		if err != nil {
			d.logger.Printf("spec_split: %s was not split: %v", issue.ID, err)
			if line := lastLine(report); line != "" {
				d.logger.Printf("spec_split: %s: %s", issue.ID, line)
			}
			continue
		}
		splits++
		if line := lastLine(report); line != "" {
			d.logger.Printf("spec_split: %s", line)
		}
	}
	if splits > 0 {
		d.logger.Printf("spec_split: split %d bead(s) in %s", splits, d.clk().Since(start).Round(time.Second))
	}
}

// specSplitCyclesDone returns a channel closed once every tick goroutine the
// step has started has finished. A test waits on it under a bound: a bare
// WaitGroup.Wait has none, and a tick that never ended would hang the run.
func (d *Daemon) specSplitCyclesDone() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		d.specSplitCycles.Wait()
		close(done)
	}()
	return done
}

// specSplitCandidates reads every open, unassigned bead carrying the split
// label from each rig's own store. A store that cannot be read fails the
// whole read, so a tick never splits from a partial town.
func (d *Daemon) specSplitCandidates() ([]*beads.Issue, error) {
	var out []*beads.Issue
	for _, rig := range d.getKnownRigs() {
		issues, err := d.rigWorkBeads(rig).List(beads.ListOptions{
			Status:     "open",
			Label:      specdispatch.SplitLabel,
			NoAssignee: true,
			Priority:   -1,
			Limit:      0,
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rig, err)
		}
		out = append(out, issues...)
	}
	return out, nil
}

// specSplitEligible reports whether the step may split issue: it carries the
// split label, names no assignee, and wears neither the rework label nor a
// hold. The hold rule is the shared one, so the hold: prefix, the human hold
// labels and a hold decision in the bead's prose all keep it out of the tick.
func specSplitEligible(issue *beads.Issue) bool {
	if issue == nil || strings.TrimSpace(issue.Assignee) != "" {
		return false
	}
	if !dispatch.HasLabel(issue.Labels, specdispatch.SplitLabel) {
		return false
	}
	if dispatch.HasLabel(issue.Labels, land.LabelRework) {
		return false
	}
	return dispatch.SlingHoldFields(issue.Labels, issue.Assignee, issue.Design, issue.Notes, nil) == ""
}
