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
// returns the counts of the files it removed, found absent and kept. A Job
// whose result line is missing or malformed is an error and never zero counts
// with nil, as a line read as zero would record a run that trimmed nothing. A
// dry run is zero counts and nil. Nothing calls it yet (cache trim, bead 7/10:
// the chain's later beads are its callers).
func (d *Daemon) cacheTrimClaimJobRun(ctx context.Context, kubeconfig string, claim cacheTrimClaim, image, namespace string, capBytes int64, sandboxes int, apply bool) (removed, absent, kept int64, err error) {
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
	finished, err := d.cacheTrimApply(ctx, kubeconfig, namespace, jobName, m, cacheTrimClaimWaitTimeout)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("applying the trim Job %s: %w", jobName, err)
	}
	if !finished {
		return 0, 0, 0, fmt.Errorf("the trim Job %s did not finish within %s", jobName, cacheTrimClaimWaitTimeout)
	}
	removed, absent, kept, err = d.cacheTrimRemovedCounts(ctx, kubeconfig, namespace, jobName)
	if err != nil {
		return 0, 0, 0, err
	}
	return removed, absent, kept, nil
}
