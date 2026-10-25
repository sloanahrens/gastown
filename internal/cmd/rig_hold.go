package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/townconfig"
	"github.com/steveyegge/gastown/internal/workspace"
)

var rigHoldReason string

var rigHoldCmd = &cobra.Command{
	Use:   "hold <rig>...",
	Short: "Hold new automatic dispatches to rigs (running work and auto-start continue)",
	Long: `Hold rigs out of new automatic dispatches.

Holding a rig writes a "dispatch_hold" record {since, by, reason} into the
rig's entry in mayor/town.json (registry). The spec dispatcher and the
daemon's scheduled slings skip the rig until 'gt rig release', while its
running polecats keep working, the daemon keeps restarting them, and the
rig's park record is not touched. A 'gt sling' the operator types still
runs: like the town-wide hold file, this hold stops the automatic
dispatchers and leaves the operator's own decision alone.

Use the hold where parking is too broad: parking also stops the rig's agents
and the daemon's auto-start, so a rig that only needs its new dispatches
stopped — while a landing backlog drains and running work finishes — is held.

A rig that is already held keeps its original record.

gt rig hold and gt rig release are the only writers of that record.

Examples:
  gt rig hold gastown --reason "drain the landing backlog"
  gt rig hold gastown beads
  gt rig release gastown`,
	Args: cobra.MinimumNArgs(1),
	RunE: runRigHold,
}

var rigReleaseCmd = &cobra.Command{
	Use:   "release <rig>...",
	Short: "Release rigs from the dispatch hold (automatic dispatch resumes)",
	Long: `Release rigs from the dispatch hold.

Releasing a rig removes its "dispatch_hold" record from mayor/town.json
(registry), which lets the automatic dispatchers see it again. It starts no
agents and changes no other rig: 'gt rig unpark' is the command for a parked
rig.

Examples:
  gt rig release gastown
  gt rig release gastown beads`,
	Args: cobra.MinimumNArgs(1),
	RunE: runRigRelease,
}

func init() {
	rigHoldCmd.Flags().StringVar(&rigHoldReason, "reason", "", "Why the rigs are held, recorded in mayor/town.json (registry)")
	rigCmd.AddCommand(rigHoldCmd)
	rigCmd.AddCommand(rigReleaseCmd)
}

func runRigHold(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rec := config.RigDispatchHold{Since: time.Now().UTC(), By: detectActor(), Reason: rigHoldReason}
	return holdRigs(cmd.OutOrStdout(), townRoot, args, rec)
}

func runRigRelease(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return releaseRigs(cmd.OutOrStdout(), townRoot, args)
}

// holdRigs writes rec as each rig's dispatch-hold record.
func holdRigs(w io.Writer, townRoot string, rigNames []string, rec config.RigDispatchHold) error {
	failed := 0
	for _, rigName := range rigNames {
		kept, err := townconfig.Hold(townRoot, rigName, rec)
		if err != nil {
			fmt.Fprintf(w, "%s %s: %v\n", style.Error.Render("✗"), rigName, err)
			failed++
			continue
		}
		fmt.Fprintf(w, "%s Rig %s %s\n", style.Success.Render("✓"), rigName, townconfig.DescribeHold(&kept))
	}
	if failed > 0 {
		return fmt.Errorf("failed to hold %d rig(s)", failed)
	}
	return nil
}

// releaseRigs clears each rig's dispatch-hold record.
func releaseRigs(w io.Writer, townRoot string, rigNames []string) error {
	failed := 0
	for _, rigName := range rigNames {
		was, err := townconfig.Release(townRoot, rigName)
		if err != nil {
			fmt.Fprintf(w, "%s %s: %v\n", style.Error.Render("✗"), rigName, err)
			failed++
			continue
		}
		if !was {
			fmt.Fprintf(w, "%s Rig %s was not held\n", style.Dim.Render("○"), rigName)
			continue
		}
		fmt.Fprintf(w, "%s Rig %s released; new automatic dispatches may run again\n", style.Success.Render("✓"), rigName)
	}
	if failed > 0 {
		return fmt.Errorf("failed to release %d rig(s)", failed)
	}
	return nil
}
