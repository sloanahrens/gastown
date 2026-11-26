package cmd

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/update"
)

// approvalVerifier is the apply's check that an approval record authorizes the
// plan it applies. Reading and verifying the approval are the next beads of this
// chain (gt-7o8q52, gt-xfkp99); the apply takes the type now so those beads
// change its body, not its signature.
type approvalVerifier func(rec clusterapproval.Record, plan planRecord) error

// runTownClusterApply starts the apply: it reads the plan with loadApplyPlan
// and then the approval at approvalPath with readApplyApproval, under the flags
// the command was given, and returns the first reader's error, which names
// nothing as applied. A plan and approval that read are not applied yet, since
// the approval's verification and the applies are the next beads of the chain;
// it returns an error saying so. No cluster call is made. Nothing in the tree
// calls it yet: the command that runs it is gt-7bxoh0.
func runTownClusterApply(ctx context.Context, kube update.RunFunc, planDir, approvalPath string, kubeconfigs planFlags, verify approvalVerifier) error { //nolint:unparam // ctx is unused until the applies of the next beads of this chain
	record, held, err := loadApplyPlan(planDir, kubeconfigs)
	if err != nil {
		return err
	}
	if _, err := readApplyApproval(approvalPath); err != nil {
		return err
	}
	return fmt.Errorf("gt town cluster apply: the plan %s reads with %d held Deployments, but the approval at %s is not verified and apply is not built yet, so nothing was applied", record.ID, len(held), approvalPath)
}
