package daemon

import (
	"context"
	"errors"
	"time"
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
// A finished apply is recorded as one run with the Job's counts. A dry run
// applies nothing, so it records no run. The no-op item is cleared by the route
// that calls this, not here.
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
		d.cacheTrimPark(namespace, claim.ClaimName, err)
		return false
	}
	d.cacheTrimClearAlert(ctx, cacheTrimNoPodListKey(namespace), "the sandbox may list pods in "+namespace)
	// A trim through the claim Job measures nothing, so an item an earlier measure
	// raised describes nothing this pass can judge. It is cleared only now that the
	// pod read has succeeded: a failed read is an unknown reading and keeps it.
	d.cacheTrimNoOp.clear(cacheTrimNoOpKey(namespace, claim.ClaimName))
	// This refusal is the town's config, the same on every pass, so it is a
	// verdict: the claim is judged again at the full interval, not soon.
	if other, taken := trimmed[namespace]; taken {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: the claim %s is trimmed in the same namespace, and a trim's labels cannot tell the two sets of objects apart; nothing trimmed", claim.ClaimName, namespace, other)
		return true
	}
	trimmed[namespace] = claim.ClaimName
	removed, absent, kept, err := d.cacheTrimClaimJobRun(kubeconfig, claim, image, namespace, capBytes, sandboxes, apply)
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: failed: %v", claim.ClaimName, namespace, err)
		// A Job still running at the wait's end is no failure: it is read at a
		// later pass, so nothing is recorded or parked.
		if !errors.Is(err, errCacheTrimJobPending) {
			d.cacheTrimRecordRun(cacheTrimRun{claim: claim.ClaimName, namespace: namespace, started: started, outcome: cacheTrimRunFailed})
			d.cacheTrimPark(namespace, claim.ClaimName, err)
		}
		return false
	}
	if !apply {
		return true
	}
	run := cacheTrimRun{claim: claim.ClaimName, namespace: namespace, started: started, outcome: cacheTrimRunOK,
		trimmed: &removed, absent: &absent, kept: &kept}
	d.cacheTrimRecordRun(run)
	elapsed := d.diskMaintenanceSeams().now().Sub(started).Round(time.Second)
	d.cacheTrimLogf("disk_maintenance: cache trim %s in %s finished: removed %d absent %d kept %d elapsed %s", claim.ClaimName, namespace, removed, absent, kept, elapsed)
	return true
}
