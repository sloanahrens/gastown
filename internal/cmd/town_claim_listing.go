package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/doctor"
	"github.com/steveyegge/gastown/internal/workspace"
)

var townClaimListingCmd = &cobra.Command{
	Use:   "claim-listing",
	Short: "Print the pod claim listing of the town, for the mac-town-state check",
	Long: `Print the claim listing of the town this command runs in, for the Mac-side
mac-town-state check. Stdout is the listing only; an error exits 1 with nothing on stdout.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true, // a refusal prints no usage, so stdout holds the listing alone
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runTownClaimListing(cmd.OutOrStdout(), workspace.FindFromCwdOrError)
	},
}

func init() {
	townCmd.AddCommand(townClaimListingCmd)
}

// runTownClaimListing writes the listing of the town find returns to out, and nothing on an error.
func runTownClaimListing(out io.Writer, find func() (string, error)) error {
	town, err := find()
	if err != nil {
		return fmt.Errorf("finding the town: %w", err)
	}
	data, err := doctor.PodTownListing(town)
	if err != nil {
		return fmt.Errorf("building the claim listing: %w", err)
	}
	if _, err := out.Write(data); err != nil {
		return fmt.Errorf("writing the claim listing: %w", err)
	}
	return nil
}
