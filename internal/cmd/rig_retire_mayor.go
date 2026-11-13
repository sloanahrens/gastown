package cmd

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var rigRetireMayorApply bool

var rigRetireMayorCmd = &cobra.Command{
	Use:   "retire-mayor <rig>",
	Short: "Rename a rig's mayor/rig to mayor/rig.retired-<date> (a dry run unless --apply)",
	Long: `Rename a rig's mayor/rig to mayor/rig.retired-<date>, so that anything still
using it fails in plain sight for a while before the directory is deleted.

This is a dry run unless --apply is given. The dry run prints the rename and the
mv that undoes it, and changes nothing. With --apply the directory is renamed,
and nothing else is written.

The command refuses while anything still points at the old path, and it names
each one:
  - a beads redirect in the rig's .beads, or in a crew or polecat worktree's
    .beads, whose target resolves into the rig's mayor/rig
  - a routes.jsonl line whose path resolves to the rig's mayor/rig
  - the rig's .beads directory without metadata.json (its beads have not moved)
  - a destination that already exists

A rig with no mayor/rig directory is already retired, and that is not an error.
Deleting the renamed directory is a separate step for the operator.

Examples:
  gt rig retire-mayor gastown
  gt rig retire-mayor gastown --apply`,
	Args: cobra.ExactArgs(1),
	RunE: runRigRetireMayor,
}

func init() {
	rigRetireMayorCmd.Flags().BoolVar(&rigRetireMayorApply, "apply", false, "Rename the directory (without it, print the plan and change nothing)")
	rigCmd.AddCommand(rigRetireMayorCmd)
}

func runRigRetireMayor(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return retireMayorRig(os.Stdout, rig.OSRetireFS{}, townRoot, args[0], rigRetireMayorApply, time.Now)
}

// retireMayorRig prints the retire-mayor plan for rigName. It renames the rig's
// mayor/rig only when apply is set and the plan has no refusals.
func retireMayorRig(w io.Writer, fsys rig.RetireFS, townRoot, rigName string, apply bool, clock func() time.Time) error {
	plan, err := rig.PlanRetireMayor(fsys, townRoot, rigName, clock)
	if err != nil {
		return fmt.Errorf("planning retire-mayor for rig %s: %w", rigName, err)
	}
	if plan.AlreadyRetired {
		fmt.Fprintf(w, "%s %s is absent: rig %s is already retired\n", style.Dim.Render("○"), plan.From, rigName)
		return nil
	}
	if len(plan.Refusals) > 0 {
		for _, refusal := range plan.Refusals {
			fmt.Fprintf(w, "%s %s\n", style.Error.Render("✗"), refusal)
		}
		return fmt.Errorf("refusing to rename %s: %d reason(s) above; nothing renamed", plan.From, len(plan.Refusals))
	}
	if !apply {
		fmt.Fprintf(w, "Would rename %s to %s\n", plan.From, plan.To)
		fmt.Fprintf(w, "  Undo: %s\n", plan.Undo())
		fmt.Fprintf(w, "Dry run: nothing renamed. Re-run with %s to rename.\n", style.Dim.Render("--apply"))
		return nil
	}
	if err := plan.Apply(fsys); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s Renamed %s to %s\n", style.Success.Render("✓"), plan.From, plan.To)
	fmt.Fprintf(w, "  Undo: %s\n", plan.Undo())
	return nil
}
