package daemon

import "context"

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
	removed, absent, kept, err := d.cacheTrimClaimJobRun(kubeconfig, claim, image, namespace, capBytes, sandboxes, apply)
	if err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: failed: %v", claim.ClaimName, namespace, err)
		return false
	}
	if !apply {
		return true
	}
	run := cacheTrimRun{claim: claim.ClaimName, namespace: namespace, started: started, outcome: cacheTrimRunOK,
		trimmed: &removed, absent: &absent, kept: &kept}
	d.cacheTrimRecordRun(run)
	return true
}
