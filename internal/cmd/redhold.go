package cmd

import (
	"encoding/json"
	"fmt"
	"os"
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

// newRedHoldCmdWith takes the town finder and environment as seams for a test.
func newRedHoldCmdWith(findTownRoot func() (string, error), getenv ...func(string) string) *cobra.Command {
	env := os.Getenv
	if len(getenv) > 0 {
		env = getenv[0]
	}
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
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rec)
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
	var reason string
	lift := &cobra.Command{
		Use:          "lift",
		Short:        "Ask the daemon to lift the red hold",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(reason) == "" {
				return fmt.Errorf("--reason is required: say what was fixed")
			}
			root, err := findTownRoot()
			if err != nil {
				return err
			}
			rec, err := dispatch.ReadRedHold(root)
			if err != nil {
				return fmt.Errorf("reading the red hold: %w", err)
			}
			if len(rec.Reasons) == 0 {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), "nothing to lift"); err != nil {
					return err
				}
				cmd.SilenceErrors = true // no error text: the line above is the answer
				return NewSilentExit(1)
			}
			by := env("BEADS_ACTOR")
			if err := dispatch.WriteLift(root, dispatch.LiftRequest{Reason: reason, By: by, At: time.Now()}); err != nil {
				return fmt.Errorf("writing the lift request: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "lift requested; the daemon decides on its next heartbeat; see gt red-hold status")
			return err
		},
	}
	lift.Flags().StringVar(&reason, "reason", "", "What was fixed, for the daemon's record (required)")
	c := &cobra.Command{
		Use:     "red-hold",
		GroupID: GroupDiag,
		Short:   "Inspect the red hold that parks automatic dispatch",
		RunE:    requireSubcommand,
	}
	c.AddCommand(status, lift)
	return c
}

func init() {
	rootCmd.AddCommand(newRedHoldCmd())
}
