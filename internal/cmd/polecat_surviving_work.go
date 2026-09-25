package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/workspace"
)

var polecatSurvivingWorkCmd = &cobra.Command{
	Use:   "surviving-work <bead-id>",
	Short: "Print the polecat branch that still carries a bead's unmerged work",
	Long: `Print the newest polecat branch for a bead that still carries a commit whose
patch is on neither the rig's default branch nor an integration branch (local
in the rig repo or on origin).

This is the check to run before resetting a hooked bead whose polecat is gone:
if it prints a branch, keep the bead hooked and resume the work with
  gt sling <bead-id> <rig> --branch <branch>

Exit codes:
  0  work survives; the branch is printed
  3  no surviving work (nothing printed); the bead may be reset
  2  cannot tell (for example, origin unreachable); keep the bead hooked
Treat any exit other than 0 or 3 as "cannot tell".`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatSurvivingWork,
}

func init() {
	polecatCmd.AddCommand(polecatSurvivingWorkCmd)
}

func runPolecatSurvivingWork(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "surviving-work: %v\n", err)
		return NewSilentExit(2)
	}
	return reportSurvivingWork(cmd.OutOrStdout(), cmd.ErrOrStderr(), townRoot, args[0])
}

// Exit codes of gt polecat surviving-work besides 0 (work survives). 1 is left
// out on purpose: cobra and flag errors exit 1, and callers must read those as
// "cannot tell", never as "nothing to protect".
const (
	survivingWorkUnknown = 2
	survivingWorkNone    = 3
)

// reportSurvivingWork prints the surviving branch and maps the answer to the
// command's exit contract.
func reportSurvivingWork(stdout, stderr io.Writer, townRoot, beadID string) error {
	branch, err := survivingWorkForBeadFn(townRoot, beadID)
	switch {
	case err != nil && noRepoToProtect(err):
		return NewSilentExit(survivingWorkNone) // no git repo: no branch to protect
	case err != nil:
		fmt.Fprintf(stderr, "surviving-work: cannot tell for %s: %v\n", beadID, err)
		return NewSilentExit(survivingWorkUnknown)
	case branch == "":
		return NewSilentExit(survivingWorkNone)
	}
	fmt.Fprintln(stdout, branch)
	return nil
}
