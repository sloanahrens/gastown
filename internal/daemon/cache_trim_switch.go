package daemon

import "context"

// cacheTrimClaimViaJob trims one claim through the claim Job and nothing else:
// it reads the pods that mount the claim only for the sandbox count the Job's
// planner needs, and never lists the claim, never execs into a seat pod, and
// never reads a listing through a Job log. It reports whether the claim is
// judged for this pass: false is a failure the pass retries or parks, true a
// trim that ran or a verdict. The run and park records are the beads that
// follow the switch.
//
// Nothing calls it yet (cache trim, bead 7/10: cacheTrimClaim is routed to it
// by the next bead).
func (d *Daemon) cacheTrimClaimViaJob(ctx context.Context, kubeconfig string, claim cacheTrimClaim, image, namespace string, capBytes int64, apply bool, trimmed map[string]string) bool {
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
	if _, _, _, err := d.cacheTrimClaimJobRun(kubeconfig, claim, image, namespace, capBytes, sandboxes, apply); err != nil {
		d.cacheTrimLogf("disk_maintenance: cache trim %s in %s: failed: %v", claim.ClaimName, namespace, err)
		return false
	}
	return true
}
