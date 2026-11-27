package cmd

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/clusterapproval"
)

// townClusterApplyCmd is `gt town cluster apply`, registered under
// townClusterCmd by this file's init. It reads the plan and its approval, checks
// the approval against the plan and re-reads the held Deployments; then it
// applies the plan's files.
var townClusterApplyCmd = newTownClusterApplyCmd()

// newTownClusterApplyCmd builds `gt town cluster apply`: the plan directory and
// the approval record it will read, and each phase's kubeconfig.
func newTownClusterApplyCmd() *cobra.Command {
	var planDir, approvalPath string
	var kubeconfigs planFlags
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a plan whose approval record authorizes it",
		Long: `Check the plan a plan directory holds against the approval record that
authorizes it, re-read the Deployments the plan holds, and apply the plan's files.
Each phase takes its kubeconfig from its own flag: phase P --policy-kubeconfig,
the deployer-policy certificate, and phase W --apply-kubeconfig, the
deployer-apply certificate.

Phase P's files apply first, then phase W's, with the bd gateway Deployment moved
ahead of the daemon, the dashboard and trackerd: each Deployment is waited for,
its rollout and its Available condition, before the next object is applied, so a
gateway that does not come back stops the apply before the daemon behind it is
replaced. Before the first apply it prints the kubectl rollout undo command for
every Deployment it writes, with the revision plan.json records; a Deployment with
no recorded revision refuses with nothing applied. The apply prints those commands
and never runs one. Whatever the outcome, it appends one line to the apply ledger
beside the approval record.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTownClusterApply(cmd.Context(), realRunFunc(), cmd.OutOrStdout(), planDir, approvalPath, kubeconfigs, applyRecordVerifier(time.Now))
		},
	}
	cmd.Flags().StringVar(&planDir, "plan", "", "the plan directory holding plan.json and its files (required)")
	cmd.Flags().StringVar(&approvalPath, "approval", "", "the approval record file that authorizes this plan (required)")
	cmd.Flags().StringVar(&kubeconfigs.policyKubeconfig, "policy-kubeconfig", "", "phase P's kubeconfig, the deployer-policy certificate's: required when phase P has files")
	cmd.Flags().StringVar(&kubeconfigs.applyKubeconfig, "apply-kubeconfig", "", "phase W's kubeconfig, the deployer-apply certificate's: required when phase W has files")
	// Each flag is defined just above, and MarkFlagRequired's only error is an
	// unknown name, so marking them cannot fail.
	_ = cmd.MarkFlagRequired("plan")
	_ = cmd.MarkFlagRequired("approval")
	return cmd
}

// applyRecordVerifier is the approvalVerifier the command gives the apply: the
// record must approve this plan for the control cluster inside its window at
// now(). The live versions are not compared yet, so Verify gets no versions and
// no live map; that comparison is a later bead of the chain.
func applyRecordVerifier(now func() time.Time) approvalVerifier {
	return func(rec clusterapproval.Record, plan planRecord) error {
		return clusterapproval.Verify(rec, clusterapproval.Plan{Cluster: clusterapproval.ClusterControl, SHA256: plan.ID}, nil, now())
	}
}

func init() {
	townClusterCmd.AddCommand(townClusterApplyCmd)
}
