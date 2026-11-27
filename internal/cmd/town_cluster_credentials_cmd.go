package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// newTownClusterCredentialsCmd builds `gt town cluster credentials` with the --cluster, --role and --out
// string flags, each empty by default, and no argument. The command is not registered, and its RunE
// refuses until the run is wired, so a call writes nothing.
func newTownClusterCredentialsCmd() *cobra.Command {
	var cluster, role, out string
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "Issue a deployer credential for one role on a cluster (not wired yet)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("credentials is not wired yet")
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "the cluster the credential is for")
	cmd.Flags().StringVar(&role, "role", "", "the deployer role: read, policy or apply")
	cmd.Flags().StringVar(&out, "out", "", "the kubeconfig path to write")
	return cmd
}
