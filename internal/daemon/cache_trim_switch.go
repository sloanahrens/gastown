package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// cacheTrimClaimViaJob trims one claim through the claim Job and nothing else:
// it reads the pods that mount the claim only for the sandbox count the Job's
// planner needs, and never lists the claim, never execs into a seat pod and
// never reads a listing through a Job log. A count of zero is a valid input (no
// running pod mounts the claim; the Job mounts it itself) and is applied as
// such. It reports whether the claim is judged for this pass: false is a
// failure, true a trim that ran or a verdict.
//
// The pods are read before the same-namespace refusal, in the order of the old
// body, and the pod-read block below is copied from the old body of cacheTrimClaim and
// dies with it (bead gt-4bgici). cacheTrimClaim routes to it (bead gt-zc1d92).
//
// A finished apply is recorded as one run with the Job's counts and decides the
// claim's no-op item by the disk it reads (cacheTrimDiskItem). A dry run applies
// nothing, so it records no run and leaves the item as it is.
func (d *Daemon) cacheTrimClaimViaJob(ctx context.Context, kubeconfig string, claim cacheTrimClaim, image, namespace string, capBytes int64, apply bool, trimmed map[string]string) bool {
	// A claim whose trim failed stays parked until the daemon restarts: a
	// verdict, no retry and no cluster call.
	if d.cacheTrimParked(namespace, claim.ClaimName) {
		// The parked claim's failed Job and pods may still be there, so it keeps the
		// namespace's marker: a second claim here is refused, as it was when this
		// claim failed.
		if _, taken := trimmed[namespace]; !taken {
			trimmed[namespace] = claim.ClaimName
		}
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: PARKED after a failure; nothing trimmed until the daemon restarts", claim.ClaimName, namespace)
		return true
	}
	started := d.diskMaintenanceSeams().now()
	sandboxes, _, err := d.cacheTrimPods(ctx, kubeconfig, namespace, claim.ClaimName)
	if err != nil {
		// A read the daemon's shutdown cut short is not a trim failure: record,
		// park and raise nothing, and leave the mark for the restarted daemon.
		if d.shuttingDown() {
			d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the daemon is shutting down; the trim is left for the restarted daemon", claim.ClaimName, namespace)
			return true
		}
		// A read the cluster refuses names the verb in an alert, and the pass
		// skips the namespace; any other failure is only logged.
		if d.cacheTrimPodListDenied(ctx, kubeconfig, namespace) {
			d.cacheTrimReportNoPodList(ctx, namespace)
			return false
		}
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the pods that mount it could not be read: %v; nothing trimmed", claim.ClaimName, namespace, err)
		// Any other failed read fails loud and stops, like a failed Job: parked, not
		// retried on the next pass. A refused read above is the exception: its alert
		// clears on its own when the sandbox may list pods.
		d.cacheTrimRecordRun(cacheTrimRun{claim: claim.ClaimName, namespace: namespace, started: started, outcome: cacheTrimRunFailed})
		// The run ended, so the mark it carried is spent; a dry run touches no mark.
		if apply {
			d.cacheTrimSetPending(namespace, claim.ClaimName, false)
		}
		d.cacheTrimPark(namespace, claim.ClaimName, err)
		return false
	}
	d.cacheTrimClearAlert(ctx, cacheTrimNoPodListKey(namespace), "the sandbox may list pods in "+namespace)
	// This refusal is the town's config, the same on every pass, so it is a
	// verdict: the claim is judged again at the full interval, not soon.
	if other, taken := trimmed[namespace]; taken {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the claim %s is trimmed in the same namespace, and a trim's labels cannot tell the two sets of objects apart; nothing trimmed", claim.ClaimName, namespace, other)
		return true
	}
	trimmed[namespace] = claim.ClaimName
	// An earlier Job that completed after its wait is recorded before the stale
	// Job is deleted and the new one applied, so its result is not lost.
	if apply {
		// A skipped late read applies no Job; a failed one ends the pass.
		if ok, skip := d.cacheTrimLateResult(ctx, kubeconfig, claim, namespace, started); skip || !ok {
			return skip
		}
	}
	removed, absent, kept, err := d.cacheTrimClaimJobRun(kubeconfig, claim, image, namespace, capBytes, sandboxes, apply)
	if err != nil {
		// A failure while the daemon shuts down is not a trim failure: its context cut
		// the run short and its Job may still complete. Nothing is recorded or parked;
		// an apply marks the claim pending for the restarted daemon, a dry run nothing.
		if d.shuttingDown() {
			if apply {
				d.cacheTrimSetPending(namespace, claim.ClaimName, true)
			}
			d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the daemon is shutting down; the trim is left for the restarted daemon", claim.ClaimName, namespace)
			return true
		}
		// A Job still running at the wait's end is a verdict, not a failure: its
		// result is read at a later pass, so nothing is recorded or parked.
		if errors.Is(err, errCacheTrimJobPending) {
			// An apply marks the claim for the pass that reads it; a dry run touches no
			// mark.
			if apply {
				d.cacheTrimSetPending(namespace, claim.ClaimName, true)
			}
			d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the trim Job %s is still running; its result is read at the next pass", claim.ClaimName, namespace, cacheTrimJobName(claim.ClaimName))
			return true
		}
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: failed: %v", claim.ClaimName, namespace, err)
		d.cacheTrimRecordRun(cacheTrimRun{claim: claim.ClaimName, namespace: namespace, started: started, outcome: cacheTrimRunFailed})
		if apply {
			d.cacheTrimSetPending(namespace, claim.ClaimName, false)
		}
		d.cacheTrimPark(namespace, claim.ClaimName, err)
		return false
	}
	if !apply {
		return true
	}
	run := cacheTrimRun{claim: claim.ClaimName, namespace: namespace, started: started, outcome: cacheTrimRunOK,
		trimmed: &removed, absent: &absent, kept: &kept}
	d.cacheTrimRecordRun(run)
	d.cacheTrimSetPending(namespace, claim.ClaimName, false)
	// A success is logged every time: the once-an-hour hold of cacheTrimLogf is for
	// refusals, and would hide the second of two trims with the same counts.
	elapsed := d.diskMaintenanceSeams().now().Sub(started).Round(time.Second)
	d.logger.Printf("disk_maintenance: cache trim %s in %s finished: removed %d absent %d kept %d elapsed %s", claim.ClaimName, namespace, removed, absent, kept, elapsed)
	d.cacheTrimDiskItem(claim.ClaimName, namespace)
	return true
}

// cacheTrimLateResult records the run of the claim's earlier trim Job when that
// Job completed after its wait ended: its result line is read and recorded the
// way a finished trim's is. A Job still running is left to cacheTrimApply, which
// refuses the apply as pending, and so is a Job that did not complete. A Job the
// controller marked Failed parks the claim, as a failed read does. It reports
// false when the Job's state or result line cannot be read: the run is recorded
// as failed, the claim parked and its mark cleared, like any other failure. A
// failure the daemon's shutdown cut short is not a trim failure: it reports ok
// and skip true, records, parks and raises nothing and leaves the mark for the
// restart.
//
// The late read is for a claim a pass left pending (its Job still running at the
// wait's end): a claim with no mark returns true at once, since a Job that
// finished inside its own pass was recorded then. The mark is memory only, so a
// result whose Job completed after its wait and then outlives a daemon restart is
// not read late; that loss is the accepted cost of recording each Job once, and
// persisting the mark is gt-d4cxyx.
func (d *Daemon) cacheTrimLateResult(ctx context.Context, kubeconfig string, claim cacheTrimClaim, namespace string, started time.Time) (ok, skip bool) {
	// Only a claim a pass left pending has a result to read late: a Job that
	// finished inside its own pass was recorded then, and is not recorded again.
	if !d.cacheTrimIsPending(namespace, claim.ClaimName) {
		return true, false
	}
	jobName := cacheTrimJobName(claim.ClaimName)
	fail := func(err error) (bool, bool) {
		// A failure the daemon's shutdown cut short is not a trim failure: the mark
		// is left for the restarted daemon.
		if d.shuttingDown() {
			d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the daemon is shutting down; the trim is left for the restarted daemon", claim.ClaimName, namespace)
			return true, true
		}
		d.cacheTrimRecordRun(cacheTrimRun{claim: claim.ClaimName, namespace: namespace, started: started, outcome: cacheTrimRunFailed})
		d.cacheTrimSetPending(namespace, claim.ClaimName, false)
		d.cacheTrimPark(namespace, claim.ClaimName, err)
		return false, false
	}
	_, running, completed, failed, err := d.cacheTrimExisting(ctx, kubeconfig, namespace)
	if err != nil {
		return fail(err)
	}
	// A Job a pass left pending that the controller then failed (past its
	// deadline or its backoff limit) is read here, before the apply deletes it as
	// stale: the claim parks. The verdict is the Failed condition alone: a Job that
	// completed after failing a pod, or sits between two retries, has failed pods
	// and no such condition, and is read below or left to the apply.
	if failed[jobName] {
		return fail(fmt.Errorf("the trim Job %s failed after a pass left it pending", jobName))
	}
	if running[jobName] || !completed[jobName] {
		return true, false
	}
	removed, absent, kept, err := d.cacheTrimRemovedCounts(ctx, kubeconfig, namespace, jobName)
	if err != nil {
		return fail(err)
	}
	d.cacheTrimRecordRun(cacheTrimRun{claim: claim.ClaimName, namespace: namespace, started: started, outcome: cacheTrimRunOK,
		trimmed: &removed, absent: &absent, kept: &kept})
	d.logger.Printf("disk_maintenance: cache trim %s in %s: the earlier trim Job %s completed after its wait; recorded: removed %d absent %d kept %d", claim.ClaimName, namespace, jobName, removed, absent, kept)
	// The late trim finished too, so it makes the same decision as a finished
	// apply: a stale item goes when the disk it left reads under the percent.
	d.cacheTrimDiskItem(claim.ClaimName, namespace)
	return true, false
}

// cacheTrimDiskItem decides the claim's cache-trim-noop item by the sandbox disk
// a finished trim reads: at or over the early trim percent the item is raised,
// since files younger than the age floor cannot be removed yet and open sandbox
// paths can hold space; under it a stale item is cleared. A disk that cannot be
// read leaves the item as it was (an unreadable disk is neither full nor empty)
// and says so once an hour. The read has its own diskMaintenanceContext, because
// the pass context can end during a trim the claim Job runs under a longer one.
func (d *Daemon) cacheTrimDiskItem(claim, namespace string) {
	ctx, cancel := d.diskMaintenanceContext()
	defer cancel()
	percent, err := diskMaintenanceCacheTrimEarlyPercent(d.patrol())
	if err != nil {
		return // the early tick refuses a setting it cannot read
	}
	used, err := d.sandboxDiskUsedPct(ctx)
	if err != nil {
		d.cacheTrimLogKeyed("cache_trim_disk_unknown:"+namespace+"/"+claim,
			"disk_maintenance: cache trim %s in %s: the sandbox disk could not be read after the trim: %v; the attention item is left as it was", claim, namespace, err)
		return
	}
	if used < float64(percent) {
		d.cacheTrimNoOp.clear(cacheTrimNoOpKey(namespace, claim))
		return
	}
	d.cacheTrimNoOp.raise(attention.Item{
		Key:      cacheTrimNoOpKey(namespace, claim),
		Kind:     attention.KindCacheTrimNoOp,
		Severity: attention.SeverityLow,
		Summary:  fmt.Sprintf("cache trim of %s in %s completed but the sandbox disk still reads %.0f%% against the early trim percent %d%%: files younger than the age floor cannot be removed yet and open sandbox paths can hold space", claim, namespace, used, percent),
	})
}
