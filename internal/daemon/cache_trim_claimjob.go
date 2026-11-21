package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// cacheTrimClaimWaitTimeout bounds the wait on a claim-carried trim Job: its pod
// walks 1.16 million files in 7 s, a first trim of a 90 GB claim is unmeasured,
// and bead 4/10 sets the bound again from the elapsed time it logs.
const cacheTrimClaimWaitTimeout = 15 * time.Minute

// cacheTrimClaimJobRun applies one claim's trim through the claim Job and
// reports whether the Job finished: nil once it did, and an error when the
// manifest was refused, the apply failed, or the Job was still running at
// cacheTrimClaimWaitTimeout. Nothing calls it yet (cache trim, bead 7/10: the
// chain's later beads are its callers).
func (d *Daemon) cacheTrimClaimJobRun(ctx context.Context, kubeconfig string, claim cacheTrimClaim, image, namespace string, capBytes int64, sandboxes int, apply bool) error {
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
		return err
	}
	if !apply {
		d.logger.Printf("disk_maintenance: cache trim %s in %s dry_run: %s", claim.ClaimName, namespace, cacheTrimManifestLine(jobName, m))
		return nil
	}
	finished, err := d.cacheTrimApply(ctx, kubeconfig, namespace, jobName, m, cacheTrimClaimWaitTimeout)
	if err != nil {
		return fmt.Errorf("applying the trim Job %s: %w", jobName, err)
	}
	if !finished {
		return fmt.Errorf("the trim Job %s did not finish within %s", jobName, cacheTrimClaimWaitTimeout)
	}
	return nil
}
