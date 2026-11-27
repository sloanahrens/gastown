package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// townClusterApplyCmd is `gt town cluster apply`, registered under
// townClusterCmd by this file's init. It does not read or apply anything yet: a
// call with the right flags returns an error saying apply is not wired, reads
// no cluster and applies nothing (gt-30ewaj wires it to runTownClusterApply).
var townClusterApplyCmd = newTownClusterApplyCmd()

// newTownClusterApplyCmd builds `gt town cluster apply`: the plan directory and
// the approval record it will read, and each phase's kubeconfig.
func newTownClusterApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a plan under its approval record (not wired yet)",
		Long: `Apply the plan a plan directory holds, under the approval record that
authorizes it. Each phase takes its kubeconfig from its own flag: phase P
--policy-kubeconfig, the deployer-policy certificate, and phase W
--apply-kubeconfig, the deployer-apply certificate. This command is not wired
yet: a call with the right flags returns an error saying apply is not wired, so
nothing was applied; it reads no cluster.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("gt town cluster apply: apply is not wired yet, so nothing was applied")
		},
	}
	cmd.Flags().String("plan", "", "the plan directory holding plan.json and its files (required)")
	cmd.Flags().String("approval", "", "the approval record file that authorizes this plan (required)")
	cmd.Flags().String("policy-kubeconfig", "", "phase P's kubeconfig, the deployer-policy certificate's: required when phase P has files")
	cmd.Flags().String("apply-kubeconfig", "", "phase W's kubeconfig, the deployer-apply certificate's: required when phase W has files")
	// Each flag is defined just above, and MarkFlagRequired's only error is an
	// unknown name, so marking them cannot fail.
	_ = cmd.MarkFlagRequired("plan")
	_ = cmd.MarkFlagRequired("approval")
	return cmd
}

func init() {
	townClusterCmd.AddCommand(townClusterApplyCmd)
}
