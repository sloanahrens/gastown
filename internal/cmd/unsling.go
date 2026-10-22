package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var unslingCmd = &cobra.Command{
	Use:     "unsling [bead-id] [target]",
	Aliases: []string{"unhook"},
	GroupID: GroupWork,
	Short:   "Remove work from an agent's hook",
	Long: `Remove work from an agent's hook (the inverse of sling/hook).

With no arguments, clears your own hook. With a bead ID, only unslings
if that specific bead is currently hooked. With a target, operates on
another agent's hook.

Examples:
  gt unsling                        # Clear my hook (whatever's there)
  gt unsling gt-abc                 # Only unsling if gt-abc is hooked
  gt unsling greenplace/joe            # Clear joe's hook
  gt unsling gt-abc greenplace/joe     # Unsling gt-abc from joe

The bead's status changes from 'hooked' back to 'open'.

Related commands:
  gt sling <bead>    # Hook + start (inverse of unsling)
  gt hook <bead>     # Hook without starting
  gt hook      # See what's on your hook`,
	Args: cobra.MaximumNArgs(2),
	RunE: runUnsling,
}

var (
	unslingDryRun bool
	unslingForce  bool
)

func init() {
	unslingCmd.Flags().BoolVarP(&unslingDryRun, "dry-run", "n", false, "Show what would be done")
	unslingCmd.Flags().BoolVarP(&unslingForce, "force", "f", false, "Unsling even if work is incomplete")
	rootCmd.AddCommand(unslingCmd)
}

func runUnsling(cmd *cobra.Command, args []string) error {
	return runUnslingWith(cmd, args, unslingDryRun, unslingForce)
}

func runUnslingWith(cmd *cobra.Command, args []string, dryRun, force bool) error {
	var targetBeadID string
	var targetAgent string

	// Parse args: [bead-id] [target]
	switch len(args) {
	case 0:
		// No args - unsling self, whatever is hooked
	case 1:
		// Could be bead ID or target agent
		// If it contains "/" or is a known role, treat as target
		if isAgentTarget(args[0]) {
			targetAgent = args[0]
		} else {
			targetBeadID = args[0]
		}
	case 2:
		targetBeadID = args[0]
		targetAgent = args[1]
	}

	// Resolve target agent (default: self)
	var agentID string
	var err error
	if targetAgent != "" {
		agentID, _, _, err = resolveTargetAgent(targetAgent)
		if err != nil {
			return fmt.Errorf("resolving target agent: %w", err)
		}
	} else {
		agentID, _, _, err = resolveSelfTarget()
		if err != nil {
			return fmt.Errorf("detecting agent identity: %w", err)
		}
	}

	// Find town root and rig path for agent beads
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Convert agent ID to agent bead ID first, so we can use prefix-based routing
	agentBeadID := agentIDToBeadID(agentID, townRoot)
	if agentBeadID == "" {
		return fmt.Errorf("could not convert agent ID %s to bead ID", agentID)
	}

	// Resolve the correct beads directory using prefix-based routing.
	// Town-level agents (deacon) fall back to townRoot since their beads use
	// the hq- prefix stored at town level.
	rigName := strings.Split(agentID, "/")[0]
	var fallbackPath string
	if rigName == "deacon" {
		fallbackPath = townRoot
	} else {
		fallbackPath = filepath.Join(townRoot, rigName)
	}
	beadsPath := beads.ResolveHookDir(townRoot, agentBeadID, fallbackPath)

	b := beads.New(beadsPath)

	// Find hooked bead by querying status+assignee directly (hq-l6mm5).
	// The work bead itself is the authoritative source — no need to read
	// the agent bead's hook_bead slot.
	// The status set is the one gt hook shows (activeWorkStatuses), so an
	// in_progress bead assigned to the agent is selected too.
	hookedBeadID, err := firstActiveWorkID(b, agentID)
	if err != nil {
		return fmt.Errorf("looking up work hooked by %s: %w", agentID, err)
	}

	// Town-level fallback: rig-level agents (polecats, crew) may have hooked
	// HQ beads (hq-* prefix) stored in townRoot/.beads, not the rig's database.
	// Without this, gt unsling can't find or clear these beads, even though
	// gt mol status / gt hook (display) can see them. (gt-dtq7)
	if hookedBeadID == "" && townRoot != "" {
		hookedBeadID, err = firstTownActiveWorkID(openBeads(filepath.Join(townRoot, ".beads")), agentID)
		if err != nil {
			return err
		}
	}

	if hookedBeadID == "" {
		// Nothing was selected above, but the selection and this sweep are separate
		// queries: a bead that appears between them still leaves work held by this
		// agent. Reopen whatever the sweep finds so gt hook and gt unsling agree.
		cleaned, err := cleanStaleHookedBeads(cmd, b, openBeads, agentID, targetBeadID, townRoot, beadsPath, dryRun)
		if err != nil {
			return err
		}
		if !cleaned {
			if targetAgent != "" {
				fmt.Printf("%s No work hooked for %s\n", style.Dim.Render("ℹ"), agentID)
			} else {
				fmt.Printf("%s Nothing on your hook\n", style.Dim.Render("ℹ"))
			}
		}
		return nil
	}

	// If specific bead requested, verify it matches
	if targetBeadID != "" && hookedBeadID != targetBeadID {
		return fmt.Errorf("bead %s is not hooked (current hook: %s)", targetBeadID, hookedBeadID)
	}

	// Get the hooked bead to check completion and show title.
	// The hooked bead may be in a different database than the agent bead
	// (e.g., agent in rig db, hooked bead in town db), so resolve its path separately.
	hookedBeadPath := beads.ResolveHookDir(townRoot, hookedBeadID, beadsPath)
	hookedB := b
	if hookedBeadPath != beadsPath {
		hookedB = beads.New(hookedBeadPath)
	}
	hookedBead, err := hookedB.Show(hookedBeadID)
	if err != nil {
		// Bead might be deleted - still allow unsling with --force
		if !force {
			return fmt.Errorf("getting hooked bead %s: %w\n  Use --force to unsling anyway", hookedBeadID, err)
		}
		// Force mode - proceed without the bead details
		hookedBead = &beads.Issue{ID: hookedBeadID, Title: "(unknown)"}
	}

	// Check if work is complete (warn if not, unless --force)
	isComplete := hookedBead.Status == "closed"
	if !isComplete && !force {
		return fmt.Errorf("hooked work %s is incomplete (%s)\n  Use --force to unsling anyway",
			hookedBeadID, hookedBead.Title)
	}

	if targetAgent != "" {
		fmt.Printf("%s Unslinging %s from %s...\n", style.Bold.Render("🪝"), hookedBeadID, agentID)
	} else {
		fmt.Printf("%s Unslinging %s...\n", style.Bold.Render("🪝"), hookedBeadID)
	}

	if dryRun {
		fmt.Printf("Would unsling hooked bead from %s\n", agentID)
		return nil
	}

	// No ClearHookBead call needed — agent bead hook slot is no longer maintained (hq-l6mm5).

	// Update hooked bead status from "hooked" (or "in_progress") back to "open".
	// Previously, only the agent's hook slot was cleared but the bead itself stayed
	// in "hooked" status forever. Now we update the bead to match the documented
	// behavior: "The bead's status changes from 'hooked' back to 'open'."
	if hookedBead.Status == beads.StatusHooked || hookedBead.Status == string(beads.StatusInProgress) {
		if err := reopenBead(hookedB, hookedBeadID); err != nil {
			// Non-fatal: warn but don't fail the unsling. The hook slot is already
			// cleared, so the agent is unblocked. The bead status is a bookkeeping
			// issue that can be fixed manually.
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: couldn't update bead %s status: %v\n", hookedBeadID, err)
		}
	}

	// Log unhook event
	_ = events.LogFeed(events.TypeUnhook, agentID, events.UnhookPayload(hookedBeadID))

	fmt.Printf("%s Work removed from hook\n", style.Bold.Render("✓"))
	fmt.Printf("  Agent %s hook cleared (was: %s)\n", agentID, hookedBeadID)

	return nil
}

// firstActiveWorkID returns the id of the bead gt unsling clears for assignee:
// the same hooked-then-in_progress pick gt hook shows, or "" when there is none.
// A failed lookup is an error, never an empty answer: gt unsling aborts on it
// rather than reporting "nothing hooked" while the agent still holds work.
func firstActiveWorkID(b beads.Client, assignee string) (string, error) {
	active, err := listAssignedActiveWork(b, assignee)
	if err != nil {
		return "", err
	}
	if len(active) == 0 {
		return "", nil
	}
	return active[0].ID, nil
}

// beadsOpener opens the beads database at a path. cleanStaleHookedBeads takes
// one so tests can put a stub behind the town and per-bead databases it reaches.
type beadsOpener func(path string) beads.Client

// openBeads is the beadsOpener unsling runs with.
func openBeads(path string) beads.Client { return beads.New(path) }

// firstTownActiveWorkID is firstActiveWorkID against the town database, which
// holds the hq-* beads hooked to rig-level agents. It tries the exact agent ID,
// then the normalized identity mail beads use.
func firstTownActiveWorkID(townB beads.Client, agentID string) (string, error) {
	id, err := firstActiveWorkID(townB, agentID)
	if err != nil {
		return "", fmt.Errorf("looking up town work hooked by %s: %w", agentID, err)
	}
	if id != "" {
		return id, nil
	}
	normalizedID := mailNormalizedAgentID(agentID)
	if normalizedID == agentID {
		return "", nil
	}
	id, err = firstActiveWorkID(townB, normalizedID)
	if err != nil {
		return "", fmt.Errorf("looking up town work hooked by %s: %w", normalizedID, err)
	}
	return id, nil
}

// reopenBead returns a hooked or in_progress bead to "open" and clears its assignee.
// Force is needed because a plain update refuses to clear the assignee of an
// in_progress bead, and unsling is the explicit release of that claim. Before
// gt unsling selected in_progress beads (gt-qbmzu) only hooked ones reached
// this, so the refusal never showed.
func reopenBead(b beads.Client, id string) error {
	openStatus := "open"
	emptyAssignee := ""
	return b.Update(id, beads.UpdateOptions{
		Status:   &openStatus,
		Assignee: &emptyAssignee,
		Force:    true,
	})
}

// cleanStaleHookedBeads reopens every bead in a status gt hook shows as work
// (activeWorkStatuses: hooked and in_progress) that is assigned to agentID, in the
// rig database and in the town database, honoring targetBeadID and dryRun. It
// runs when the selection in runUnslingWith found nothing, so a bead that
// selection missed is not left assigned to the agent. Returns true if any bead
// matched. A failed lookup is returned as an error; a failed update is only
// warned about on stderr.
func cleanStaleHookedBeads(cmd *cobra.Command, b beads.Client, open beadsOpener, agentID, targetBeadID, townRoot, beadsPath string, dryRun bool) (bool, error) {
	// Collect stale beads from local rig beads
	staleBeads, err := listAssignedActiveWorkAcrossStatuses(b, agentID)
	if err != nil {
		return false, fmt.Errorf("listing work assigned to %s: %w", agentID, err)
	}

	// Also search town-level beads for rig-level agents (gt-dtq7).
	// HQ beads (hq-* prefix) live in townRoot/.beads, not the rig database.
	// Without this, stale HQ beads hooked to crew/polecats are invisible to cleanup.
	if townRoot != "" {
		townBeadsPath := filepath.Join(townRoot, ".beads")
		if townBeadsPath != beadsPath {
			townB := open(townBeadsPath)
			// Search with exact agent ID
			townStale, err := listAssignedActiveWorkAcrossStatuses(townB, agentID)
			if err != nil {
				return false, fmt.Errorf("listing town work assigned to %s: %w", agentID, err)
			}
			staleBeads = append(staleBeads, townStale...)
			// Also search with normalized identity (mail beads use normalized form)
			normalizedID := mailNormalizedAgentID(agentID)
			if normalizedID != agentID {
				townStale, err := listAssignedActiveWorkAcrossStatuses(townB, normalizedID)
				if err != nil {
					return false, fmt.Errorf("listing town work assigned to %s: %w", normalizedID, err)
				}
				staleBeads = append(staleBeads, townStale...)
			}
		}
	}

	if len(staleBeads) == 0 {
		return false, nil
	}

	// If a specific bead was requested, filter to only that one
	if targetBeadID != "" {
		var filtered []*beads.Issue
		for _, sb := range staleBeads {
			if sb.ID == targetBeadID {
				filtered = append(filtered, sb)
			}
		}
		if len(filtered) == 0 {
			return false, nil
		}
		staleBeads = filtered
	}

	if dryRun {
		for _, sb := range staleBeads {
			fmt.Printf("Would clean up stale hooked bead %s (%s)\n", sb.ID, sb.Title)
		}
		return true, nil
	}

	// Clean up each stale hooked bead
	for _, sb := range staleBeads {
		fmt.Printf("%s Cleaning up stale hooked bead %s...\n", style.Bold.Render("🪝"), sb.ID)

		// Resolve the correct beads directory for this bead
		stalePath := beads.ResolveHookDir(townRoot, sb.ID, beadsPath)
		staleB := b
		if stalePath != beadsPath {
			staleB = open(stalePath)
		}

		if err := reopenBead(staleB, sb.ID); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: couldn't clean up stale bead %s: %v\n", sb.ID, err)
			continue
		}
		fmt.Printf("%s Cleaned up stale bead %s (was %s, now open)\n", style.Bold.Render("✓"), sb.ID, sb.Status)
	}
	return true, nil
}

// mailNormalizedAgentID normalizes crew/polecats path to the canonical form
// used by sendHandoffMail (via mail.AddressToIdentity).
// "rig/crew/name" → "rig/name", "rig/polecats/name" → "rig/name".
func mailNormalizedAgentID(agentID string) string {
	parts := strings.Split(agentID, "/")
	if len(parts) == 3 && (parts[1] == "crew" || parts[1] == "polecats") {
		return parts[0] + "/" + parts[2]
	}
	return agentID
}

// isAgentTarget checks if a string looks like an agent target rather than a bead ID.
// Agent targets contain "/" or are known role names.
func isAgentTarget(s string) bool {
	// Contains "/" means it's a path like "greenplace/joe"
	for _, c := range s {
		if c == '/' {
			return true
		}
	}

	// Known role names
	switch s {
	case constants.RoleCrew:
		return true
	}

	return false
}
