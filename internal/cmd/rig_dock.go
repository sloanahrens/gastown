package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

// RigDockedLabel is the label set on rig identity beads when docked.
const RigDockedLabel = "status:docked"

var rigDockCmd = &cobra.Command{
	Use:   "dock <rig>",
	Short: "Dock a rig (global, persistent shutdown)",
	Long: `Dock a rig to persistently disable it across all clones.

Docking a rig:
  - Stops the refinery if running
  - Stops all polecat sessions if running
  - Sets status:docked label on the rig identity bead
  - Syncs via git so all clones see the docked status

This is a Level 2 (global/persistent) operation:
  - Affects all clones of this rig (via git sync)
  - Persists until explicitly undocked
  - The daemon respects this status and won't auto-restart agents

Use 'gt rig undock' to resume normal operation.

Examples:
  gt rig dock gastown
  gt rig dock beads`,
	Args: cobra.ExactArgs(1),
	RunE: runRigDock,
}

var rigUndockCmd = &cobra.Command{
	Use:   "undock <rig>",
	Short: "Undock a rig (remove global docked status)",
	Long: `Undock a rig to remove the persistent docked status.

Undocking a rig:
  - Removes the status:docked label from the rig identity bead
  - Syncs via git so all clones see the undocked status
  - Allows the daemon to auto-restart agents
  - Does NOT automatically start agents (use 'gt rig start' for that)

Examples:
  gt rig undock gastown
  gt rig undock beads`,
	Args: cobra.ExactArgs(1),
	RunE: runRigUndock,
}

func init() {
	rigCmd.AddCommand(rigDockCmd)
	rigCmd.AddCommand(rigUndockCmd)
}

func runRigDock(cmd *cobra.Command, args []string) error {
	rigName := args[0]

	// Check we're on main branch - docking on other branches won't persist
	branchCmd := exec.Command("git", "branch", "--show-current")
	branchOutput, err := branchCmd.Output()
	if err == nil {
		currentBranch := strings.TrimSpace(string(branchOutput))
		if currentBranch != "main" && currentBranch != "master" {
			return fmt.Errorf("cannot dock: must be on main branch (currently on %s)\n"+
				"Docking on other branches won't persist. Run: git checkout main", currentBranch)
		}
	}

	// Get rig
	townRoot, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	// Get rig prefix for bead ID
	prefix := "gt" // default
	if r.Config != nil && r.Config.Prefix != "" {
		prefix = r.Config.Prefix
	}

	// Find or create the rig identity bead (idempotent; handles duplicates
	// and Dolt query hiccups gracefully — gt-d8681).
	bd := beads.New(r.BeadsPath())
	rigBead, err := beads.EnsureRigBead(bd, rigName, &beads.RigFields{
		Repo:   r.GitURL,
		Prefix: prefix,
		State:  beads.RigStateActive,
	})
	if err != nil {
		return fmt.Errorf("ensuring rig identity bead: %w", err)
	}

	// Check if already docked
	for _, label := range rigBead.Labels {
		if label == RigDockedLabel {
			fmt.Printf("%s Rig %s is already docked\n", style.Dim.Render("•"), rigName)
			return nil
		}
	}

	fmt.Printf("Docking rig %s...\n", style.Bold.Render(rigName))

	t := tmux.NewTmux()

	// Stop polecat sessions if any: an operator stop, the supervisor's Stop,
	// logged (gt-4k3fj.4.1).
	polecatMgr := supervisedPolecatSessions(t, r, "rig dock", operatorActor("gt rig dock"))
	stoppedAgents, err := stopAndDockRig(townRoot, rigName, bd, rigBead.ID, polecatMgr)
	if err != nil {
		return err
	}

	// Output
	fmt.Printf("%s Rig %s docked (global)\n", style.Success.Render("✓"), rigName)
	fmt.Printf("  Label added: %s\n", RigDockedLabel)
	for _, msg := range stoppedAgents {
		fmt.Printf("  %s\n", msg)
	}
	fmt.Printf("  Beads changes persisted via Dolt\n")

	return nil
}

// dockPolecatSessions is the slice of polecat.SessionManager that gt rig dock
// drives.
type dockPolecatSessions interface {
	List() ([]polecat.SessionInfo, error)
	StopAll(force bool) error
}

// stopAndDockRig stops the rig's polecat sessions, sets the docked label on its
// identity bead, and stamps the rig (internal/rig.DockStamp) so a running
// daemon drops its memoized bead read. The label is written only after the stop
// succeeded: a rig labeled docked while its sessions still run is one the
// daemon will not restart and nobody is watching.
func stopAndDockRig(townRoot, rigName string, bd beads.Client, rigBeadID string, m dockPolecatSessions) ([]string, error) {
	stopped, err := stopDockedPolecats(m)
	if err != nil {
		return nil, err
	}
	if err := bd.Update(rigBeadID, beads.UpdateOptions{
		AddLabels: []string{RigDockedLabel},
	}); err != nil {
		return nil, fmt.Errorf("setting docked label: %w", err)
	}
	if err := stampRigDockState(townRoot, rigName); err != nil {
		return nil, err
	}
	return stopped, nil
}

// stampRigDockState tells a running daemon that the rig's dock state changed
// (internal/rig.DockStamp). A failure is reported rather than swallowed: the
// dock itself stands, and the daemon keeps its old verdict until its window
// expires.
func stampRigDockState(townRoot, rigName string) error {
	if err := rig.TouchDockStamp(townRoot, rigName, time.Now()); err != nil {
		return fmt.Errorf("%s's dock state is recorded, but the daemon will not see it until it re-reads the bead: %w", rigName, err)
	}
	return nil
}

// stopDockedPolecats stops the polecat sessions of a rig being docked and
// returns the summary lines for the output. A session list that cannot be read
// or a stop that fails is an error: continuing would write the docked label
// while the sessions still run.
func stopDockedPolecats(m dockPolecatSessions) ([]string, error) {
	infos, err := m.List()
	if err != nil {
		return nil, fmt.Errorf("listing polecat sessions: %w", err)
	}
	if len(infos) == 0 {
		return nil, nil
	}
	fmt.Printf("  Stopping %d polecat session(s)...\n", len(infos))
	if err := m.StopAll(false); err != nil {
		return nil, fmt.Errorf("stopping polecat sessions: %w", err)
	}
	return []string{fmt.Sprintf("%d polecat session(s) stopped", len(infos))}, nil
}

func runRigUndock(cmd *cobra.Command, args []string) error {
	rigName := args[0]

	// Check we're on main branch - undocking on other branches won't persist
	branchCmd := exec.Command("git", "branch", "--show-current")
	branchOutput, err := branchCmd.Output()
	if err == nil {
		currentBranch := strings.TrimSpace(string(branchOutput))
		if currentBranch != "main" && currentBranch != "master" {
			return fmt.Errorf("cannot undock: must be on main branch (currently on %s)\n"+
				"Undocking on other branches won't persist. Run: git checkout main", currentBranch)
		}
	}

	// Get rig and town root
	townRoot, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	// Get rig prefix for bead ID
	prefix := "gt" // default
	if r.Config != nil && r.Config.Prefix != "" {
		prefix = r.Config.Prefix
	}

	// Find the rig identity bead
	rigBeadID := beads.RigBeadIDWithPrefix(prefix, rigName)
	return undockRig(townRoot, beads.New(r.BeadsPath()), rigBeadID, rigName)
}

// undockRig removes the docked label from the rig identity bead, and stamps the
// rig (internal/rig.DockStamp) so a running daemon drops its memoized read. Only
// beads.ErrNotFound means there is no identity bead to be docked. Any other read
// failure is returned: the rig's state is unknown, and reporting "not docked"
// would leave the daemon treating it as docked.
func undockRig(townRoot string, bd beads.Client, rigBeadID, rigName string) error {
	rigBead, err := bd.Show(rigBeadID)
	if errors.Is(err, beads.ErrNotFound) {
		// Rig identity bead doesn't exist (legacy rig) - can't be docked
		fmt.Printf("%s Rig %s has no identity bead and is not docked\n", style.Dim.Render("•"), rigName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading rig identity bead %s: %w", rigBeadID, err)
	}

	// Check if actually docked
	isDocked := false
	for _, label := range rigBead.Labels {
		if label == RigDockedLabel {
			isDocked = true
			break
		}
	}
	if !isDocked {
		fmt.Printf("%s Rig %s is not docked\n", style.Dim.Render("•"), rigName)
		return nil
	}

	// Remove docked label from rig identity bead
	if err := bd.Update(rigBeadID, beads.UpdateOptions{
		RemoveLabels: []string{RigDockedLabel},
	}); err != nil {
		return fmt.Errorf("removing docked label: %w", err)
	}
	if err := stampRigDockState(townRoot, rigName); err != nil {
		return err
	}

	fmt.Printf("%s Rig %s undocked\n", style.Success.Render("✓"), rigName)
	fmt.Printf("  Label removed: %s\n", RigDockedLabel)
	fmt.Printf("  Daemon can now auto-restart agents\n")
	fmt.Printf("  Use '%s' to start agents immediately\n", style.Dim.Render("gt rig start "+rigName))

	return nil
}

// IsRigDocked checks if a rig is docked by checking for the status:docked label
// on the rig identity bead. A missing identity bead (beads.ErrNotFound) means
// not docked. Any other read failure is returned: the rig's state is unknown,
// and answering false would let a docked rig be started.
func IsRigDocked(townRoot, rigName, prefix string) (bool, error) {
	// Construct the rig beads path
	rigPath := filepath.Join(townRoot, rigName)
	beadsPath := filepath.Join(rigPath, "mayor", "rig")
	if _, err := os.Stat(beadsPath); err != nil {
		beadsPath = rigPath
	}

	return rigBeadDocked(beads.New(beadsPath), beads.RigBeadIDWithPrefix(prefix, rigName))
}

// rigBeadDocked reports whether the rig identity bead carries the docked label.
func rigBeadDocked(bd beads.Client, rigBeadID string) (bool, error) {
	rigBead, err := bd.Show(rigBeadID)
	if errors.Is(err, beads.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading rig identity bead %s: %w", rigBeadID, err)
	}

	return slices.Contains(rigBead.Labels, RigDockedLabel), nil
}
