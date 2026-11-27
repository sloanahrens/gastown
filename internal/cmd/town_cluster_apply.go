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
// as applied. A plan and approval that verify are not applied yet, since the
// applies are the next beads of the chain; it returns an error saying so. No
// cluster call is made. Nothing in the tree calls it yet: the command that runs
// it is gt-7bxoh0.
func runTownClusterApply(ctx context.Context, kube update.RunFunc, planDir, approvalPath string, kubeconfigs planFlags, verify approvalVerifier) error { //nolint:unparam // ctx is unused until the applies of the next beads of this chain
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
	return fmt.Errorf("gt town cluster apply: the plan %s reads with %d held Deployments, but apply is not built yet, so nothing was applied", record.ID, len(held))
}
