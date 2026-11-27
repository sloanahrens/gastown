package cmd

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/clusterapproval"
)

// townClusterApplyCmd is `gt town cluster apply`, registered under
// townClusterCmd by this file's init. It reads the plan and its approval, checks
// the approval against the plan and re-reads the held Deployments; it applies
// nothing yet, since the applies are a later bead of the chain.
var townClusterApplyCmd = newTownClusterApplyCmd()

// newTownClusterApplyCmd builds `gt town cluster apply`: the plan directory and
// the approval record it will read, and each phase's kubeconfig.
func newTownClusterApplyCmd() *cobra.Command {
	var planDir, approvalPath string
	var kubeconfigs planFlags
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Check a plan's approval record before apply (applies nothing yet)",
		Long: `Check the plan a plan directory holds against the approval record that
authorizes it, and re-read the Deployments the plan holds. Each phase takes its
kubeconfig from its own flag: phase P --policy-kubeconfig, the deployer-policy
certificate, and phase W --apply-kubeconfig, the deployer-apply certificate.
Nothing is applied yet: every run ends with an error saying nothing was applied,
including a plan and approval that pass.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTownClusterApply(cmd.Context(), realRunFunc(), planDir, approvalPath, kubeconfigs, applyRecordVerifier(time.Now))
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
