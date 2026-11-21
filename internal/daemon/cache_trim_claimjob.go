package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// cacheTrimClaimWaitTimeout bounds the wait on a claim-carried trim Job: more
// than three times a loaded run of the Job, and shorter than the Job's own
// 30-minute deadline (gt-uatljg), so a Job it leaves running is slow, not hung.
const cacheTrimClaimWaitTimeout = 20 * time.Minute

// cacheTrimClaimSlack is the room a claim's budget adds to its wait, for the apply.
const cacheTrimClaimSlack = 2 * time.Minute

// errCacheTrimJobPending marks a trim Job still running when the wait ends: its
// result line is read at a later pass, so the claim is not a failed trim.
var errCacheTrimJobPending = errors.New("the trim Job is still running")

// cacheTrimClaimJobRun applies one claim's trim through the claim Job and
// returns the counts of the files it removed, found absent and kept. A Job
// whose result line is missing or malformed is an error and never zero counts
// with nil, as a line read as zero would record a run that trimmed nothing. A
// dry run is zero counts and nil.
//
// A Job still running when the wait ends is decided by its status: active is
// pending (errCacheTrimJobPending), and a status that cannot be read is an
// ordinary error, never pending, so a failed Job never passes as a healthy run.
//
// The Job runs under its own budget, not the pass's (diskMaintRunTimeout is
// shorter than the wait): the daemon's context, bounded by the wait and
// cacheTrimClaimSlack. A wait the budget cuts off is an error, never pending.
//
// Nothing calls it yet (cache trim, bead 7/10: the chain's later beads are its
// callers).
func (d *Daemon) cacheTrimClaimJobRun(_ context.Context, kubeconfig string, claim cacheTrimClaim, image, namespace string, capBytes int64, sandboxes int, apply bool) (removed, absent, kept int64, err error) {
	jobName := cacheTrimJobName(claim.ClaimName)
	m, err := cachevol.BuildClaimTrimManifest(cachevol.TrimJobInput{
		Name:             jobName,
		Namespace:        namespace,
		Image:            image,
		ClaimName:        claim.ClaimName,
		CapBytes:         capBytes,
		SandboxesRunning: sandboxes,
		Now:              d.diskMaintenanceSeams().now(),
		NodeSelector:     claim.NodeSelector,
		Tolerations:      claim.Tolerations,
	})
	if err != nil {
		return 0, 0, 0, err
	}
	if !apply {
		d.logger.Printf("disk_maintenance: cache trim %s in %s dry_run: %s", claim.ClaimName, namespace, cacheTrimManifestLine(jobName, m))
		return 0, 0, 0, nil
	}
	base := d.ctx
	if base == nil {
		base = context.Background()
	}
	budget, cancel := context.WithTimeout(base, cacheTrimClaimWaitTimeout+cacheTrimClaimSlack)
	defer cancel()
	start := d.clk().Now()
	finished, err := d.cacheTrimApply(budget, kubeconfig, namespace, jobName, m, cacheTrimClaimWaitTimeout)
	if err != nil {
		// A wait the budget cuts is not an error from cacheTrimApply, so an error
		// with the budget over is the apply's; a failed Job's log read the budget
		// ends is read the same way.
		if cut := budget.Err(); cut != nil {
			return 0, 0, 0, fmt.Errorf("the trim Job %s was cut off during the apply after %s by its context (%v)",
				jobName, d.clk().Now().Sub(start).Round(time.Second), cut)
		}
		return 0, 0, 0, fmt.Errorf("applying the trim Job %s: %w", jobName, err)
	}
	if !finished {
		// A budget that ended before the wait cut it short: not pending.
		if err := budget.Err(); err != nil {
			return 0, 0, 0, fmt.Errorf("the trim Job %s was cut off during the wait after %s by its context (%v), before the %s wait ended",
				jobName, d.clk().Now().Sub(start).Round(time.Second), err, cacheTrimClaimWaitTimeout)
		}
		state, err := d.cacheTrimJobState(budget, kubeconfig, namespace, jobName)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("reading the status of the trim Job %s: %s", jobName, cacheTrimFirstLine(err.Error()))
		}
		switch state {
		case cacheTrimJobStateFailed:
			return 0, 0, 0, fmt.Errorf("the trim Job %s failed", jobName)
		case cacheTrimJobStateActive:
			return 0, 0, 0, fmt.Errorf("the trim Job %s is still running after the %s wait; its result is read at a later pass: %w", jobName, cacheTrimClaimWaitTimeout, errCacheTrimJobPending)
		}
		// A Job that completed after the wait is read as a finished one.
	}
	removed, absent, kept, err = d.cacheTrimRemovedCounts(budget, kubeconfig, namespace, jobName)
	if err != nil {
		return 0, 0, 0, err
	}
	return removed, absent, kept, nil
}
