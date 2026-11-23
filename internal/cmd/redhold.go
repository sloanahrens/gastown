package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/workspace"
)

// newRedHoldCmd builds `gt red-hold`; a test builds a fresh one so its flag state is its own.
func newRedHoldCmd() *cobra.Command {
	return newRedHoldCmdWith(workspace.FindFromCwdOrError)
}

// newRedHoldCmdWith takes the town finder as a seam so a test drives its failure.
func newRedHoldCmdWith(findTownRoot func() (string, error)) *cobra.Command {
	var townRoot string
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Print why automatic dispatch is held, one line per reason",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := townRoot
			if root == "" {
				var err error
				if root, err = findTownRoot(); err != nil {
					return err
				}
			}
			rec, err := dispatch.ReadRedHold(root)
			if err != nil {
				return fmt.Errorf("reading the red hold: %w", err)
			}
			if asJSON {
				if rec.Reasons == nil {
					rec.Reasons = []dispatch.RedHoldReason{}
				}
				data, err := json.MarshalIndent(rec, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return err
			}
			if len(rec.Reasons) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "red hold: open")
				return err
			}
			for _, r := range rec.Reasons {
				line := r.Kind + " since " + r.Since.Format(time.RFC3339)
				if r.Detail != "" {
					line += " " + r.Detail
				}
				if len(r.CureBeads) > 0 {
					line += " cure: " + strings.Join(r.CureBeads, " ")
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
					return err
				}
			}
			return nil
		},
	}
	status.Flags().StringVar(&townRoot, "town-root", "", "Town root to read (default: the town containing the working directory)")
	status.Flags().BoolVar(&asJSON, "json", false, "Print the record as indented JSON")
	c := &cobra.Command{
		Use:     "red-hold",
		GroupID: GroupDiag,
		Short:   "Inspect the red hold that parks automatic dispatch",
		RunE:    requireSubcommand,
	}
	c.AddCommand(status)
	return c
}

func init() {
	rootCmd.AddCommand(newRedHoldCmd())
}
