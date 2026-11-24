package cmd

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(seatCmd)
}

// seatCmd groups the commands a polecat seat runs about itself. Its first
// subcommand, ship, posts the seat's record as one bead comment and lets the
// sandbox exit (gt-7h8s5r).
var seatCmd = &cobra.Command{
	Use:   "seat",
	Short: "Commands a polecat seat runs about itself",
	Long: `Commands a polecat seat runs about itself.

A seat's sandbox used to wait after its agent ended so the daemon could pull the
seat's record out by executing into it. The seat's own commands post that record
instead, so the sandbox can exit.`,
	RunE: requireSubcommand,
}
