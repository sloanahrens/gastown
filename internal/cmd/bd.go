package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/workspace"
)

// bd.go is `gt bd`, the commands over the town's bd gateway. The design is
// docs/design/bd-container.md, "The workspace".

func init() {
	rootCmd.AddCommand(bdCmd)
	bdCmd.AddCommand(bdWorkspaceCmd)
	bdWorkspaceCmd.AddCommand(bdWorkspacePushCmd)
}

var bdCmd = &cobra.Command{
	Use:     "bd",
	GroupID: GroupServices,
	Short:   "Manage what the bd gateway serves",
	Long:    `Commands over the bd gateway: the bd container reads a workspace tree the town pushes to it.`,
	RunE:    requireSubcommand,
}

var bdWorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Manage the gateway's workspace tree",
	Long:  `The push is the only writer: it replaces the gateway's tree in one request.`,
	RunE:  requireSubcommand,
}

// workspacePushDeps is what a push reads from the process: where the town is,
// the gateway's configuration, the HTTP client and where the answer goes. Every
// read is a field, so a test drives a push without a town, a gateway or a
// socket.
type workspacePushDeps struct {
	findTown func() (string, error)
	getenv   func(string) string
	doer     bdgateway.HTTPDoer
	out      io.Writer
}

// processWorkspacePushDeps is what a push reads on the real machine.
func processWorkspacePushDeps() workspacePushDeps {
	return workspacePushDeps{
		findTown: workspace.FindFromCwdOrError,
		getenv:   os.Getenv,
		out:      os.Stdout,
	}
}

// bdWorkspacePushCmd is built by newWorkspacePushCmd so a test can build its
// own with the seams it needs; a package-level variable would have to be
// swapped, and the unit tier forbids that (docs/testing.md, no-global-swap).
var bdWorkspacePushCmd = newWorkspacePushCmd(processWorkspacePushDeps())

func newWorkspacePushCmd(deps workspacePushDeps) *cobra.Command {
	var townRoot, gatewayURL, tokenFile string
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push the town's workspace to the bd gateway",
		Long: `Send the town copy to the bd gateway and print the manifest it stored.

A rig whose beads directory cannot be read is named and the push stops, because
the gateway would otherwise serve a tree that routes prefixes it cannot open.

  gt bd workspace push                       # the town containing this directory
  gt bd workspace push --gateway-url http://host.docker.internal:7440 \
      --token-file ~/.local/state/gt/bd/daemon.token`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkspacePush(cmd.Context(), deps, townRoot, gatewayURL, tokenFile)
		},
	}
	cmd.Flags().StringVar(&townRoot, "town-root", "", "Town root to read (default: the town containing the working directory)")
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway base URL (default: $"+bdgateway.WorkspaceURLEnv+")")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "File holding the bearer token (default: $"+bdgateway.WorkspaceTokenEnv+")")
	return cmd
}

// runWorkspacePush builds the document from the town and sends it, printing the
// manifest hash the gateway stored. Each setting falls back to its flag, then to
// the environment gt-wrap's bdgateway target reads.
func runWorkspacePush(ctx context.Context, deps workspacePushDeps, townRoot, gatewayURL, tokenFile string) error {
	if deps.out == nil {
		return errors.New("gt bd workspace push: no output writer is wired, so the manifest hash would be lost")
	}
	if townRoot == "" {
		if deps.findTown == nil {
			return errors.New("gt bd workspace push: no town finder is wired; pass --town-root")
		}
		found, err := deps.findTown()
		if err != nil {
			return fmt.Errorf("gt bd workspace push: finding the town: %w", err)
		}
		townRoot = found
	}
	url, token := gatewayURL, tokenFile
	if deps.getenv != nil {
		if url == "" {
			url = deps.getenv(bdgateway.WorkspaceURLEnv)
		}
		if token == "" {
			token = deps.getenv(bdgateway.WorkspaceTokenEnv)
		}
	}
	pusher := bdgateway.GatewayPusher{URL: url, TokenFile: token, HTTP: deps.doer}
	manifest, err := pusher.Push(ctx, townRoot)
	if err != nil {
		return fmt.Errorf("gt bd workspace push: %w", err)
	}
	fmt.Fprintf(deps.out, "workspace %s\n", manifest)
	return nil
}
