package cmd

import (
	"context"

	"github.com/spf13/cobra"
)

// newTownClusterCredentialsCmd builds `gt town cluster credentials` with the --cluster, --role and --out
// string flags, each empty by default, and no argument. RunE issues the credential through run: it
// passes the command's context, its stdout and the three flags to runClusterCredentials and returns
// its error unchanged.
func newTownClusterCredentialsCmd(run func(ctx context.Context, argv []string) ([]byte, error)) *cobra.Command {
	var cluster, role, out string
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "Issue a deployer credential for one role on a cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterCredentials(cmd.Context(), run, cmd.OutOrStdout(), cluster, role, out)
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "the cluster the credential is for")
	cmd.Flags().StringVar(&role, "role", "", "the deployer role: read, policy or apply")
	cmd.Flags().StringVar(&out, "out", "", "the kubeconfig path to write")
	return cmd
}
