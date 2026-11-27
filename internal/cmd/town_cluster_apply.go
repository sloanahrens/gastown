package cmd

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/update"
)

// approvalVerifier is the apply's check that an approval record authorizes the
// plan it applies. The apply takes the type so the command supplies the real
// check, whose live-version comparison is a later bead of this chain.
type approvalVerifier func(rec clusterapproval.Record, plan planRecord) error

// runTownClusterApply starts the apply: it reads the plan with loadApplyPlan
// and then the approval at approvalPath with readApplyApproval, under the flags
// the command was given, and returns the first reader's error, which names
// nothing as applied. An approval whose PlanSHA256 is not the plan record's ID
// authorizes another plan and is refused before verify. Otherwise verify is
// called with the record and the plan, and its error is returned naming nothing
// as applied. Each held Deployment is then re-read with recheckApplyDeployments;
// one the cluster no longer holds at the plan's resourceVersion refuses with
// nothing applied, before any apply call. A plan and approval that pass are not
// applied yet, since the applies are the next beads of the chain; it returns an
// error saying so, after the reads and no apply. Nothing in the tree calls it
// yet: the command that runs it is gt-7bxoh0.
func runTownClusterApply(ctx context.Context, kube update.RunFunc, planDir, approvalPath string, kubeconfigs planFlags, verify approvalVerifier) error {
	record, held, err := loadApplyPlan(planDir, kubeconfigs)
	if err != nil {
		return err
	}
	approval, err := readApplyApproval(approvalPath)
	if err != nil {
		return err
	}
	// The approval's hash is checked here, before verify, so an approval of
	// another plan is refused without the verifier's clock and live reads.
	if approval.PlanSHA256 != record.ID {
		return fmt.Errorf("gt town cluster apply: the approval at %s is for another plan, %s, not %s, so nothing was applied", approvalPath, approval.PlanSHA256, record.ID)
	}
	if err := verify(approval, record); err != nil {
		return fmt.Errorf("gt town cluster apply: the approval at %s does not authorize this plan, so nothing was applied: %w", approvalPath, err)
	}
	if err := recheckApplyDeployments(ctx, kube, held); err != nil {
		return err
	}
	return fmt.Errorf("gt town cluster apply: the plan %s reads with %d held Deployments, but apply is not built yet, so nothing was applied", record.ID, len(held))
}

// applyKubeconfigRunner binds kube to kubeconfig the way the plan's dry runs
// pass it, --kubeconfig before the verb, so a read cannot fall back to the
// operator's default cluster.
func applyKubeconfigRunner(kube update.RunFunc, kubeconfig string) update.RunFunc {
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		return kube(ctx, dir, env, name, append([]string{"--kubeconfig", kubeconfig}, args...)...)
	}
}

// recheckApplyDeployments re-reads each Deployment the plan holds, through the
// kubeconfig of its phase, and refuses one the cluster no longer holds at the
// plan's resourceVersion: absent, without a resourceVersion, or at another one.
// The error names the object and never a version value, so the operator re-reads
// it rather than diffing values in a log line. A read that failed is refused too,
// so an unread cluster never reads as in step; every refusal says nothing was
// applied, and no apply call is made.
func recheckApplyDeployments(ctx context.Context, kube update.RunFunc, held []applyDeployment) error {
	for _, d := range held {
		runner := applyKubeconfigRunner(kube, d.kubeconfig)
		live, absent, err := readLiveObject(ctx, clusterDriftDeps{kubectl: runner}, "deployment/"+d.name, d.namespace, false)
		if err != nil {
			return fmt.Errorf("gt town cluster apply: cannot re-read %s, so nothing was applied: %w", d.key, err)
		}
		if absent {
			return fmt.Errorf("gt town cluster apply: the cluster no longer holds %s, which the plan names, so nothing was applied", d.key)
		}
		metadata, _ := live["metadata"].(map[string]any)
		version, _ := metadata["resourceVersion"].(string)
		if version != d.version {
			return fmt.Errorf("gt town cluster apply: the live %s is not the version the plan names, so nothing was applied", d.key)
		}
	}
	return nil
}
