package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var hookCmd = &cobra.Command{
	Use:     "hook [bead-id] [target]",
	Aliases: []string{"work"},
	GroupID: GroupWork,
	Short:   "Show or attach work on a hook",
	Long: `Show what's on your hook, or attach new work.

With no arguments, shows your current hook status (alias for 'gt mol status').
With a bead ID, attaches that work to your hook.
With a bead ID and target, attaches work to another agent's hook.

The hook is the "durability primitive" - work on your hook survives session
restarts, context compaction, and handoffs. When you restart (via gt handoff),
your SessionStart hook finds the attached work and you continue from where
you left off.

Examples:
  gt hook                                    # Show what's on my hook
  gt hook status                             # Same as above
  gt hook gt-abc                             # Attach issue gt-abc to your hook
  gt hook gt-abc -s "Fix the bug"            # With subject for handoff mail
  gt hook gt-abc gastown/crew/max            # Attach gt-abc to max's hook

Related commands:
  gt sling <bead>    # Hook + start now (keep context)
  gt handoff <bead>  # Hook + restart (fresh context)
  gt unsling         # Remove work from hook`,
	Args: cobra.MaximumNArgs(2),
	RunE: runHookOrStatus,
}

// hookStatusCmd shows hook status (alias for mol status)
var hookStatusCmd = &cobra.Command{
	Use:   "status [target]",
	Short: "Show what's on your hook",
	Long: `Show what's slung on your hook.

This is an alias for 'gt mol status'. Shows what work is currently
attached to your hook, along with progress information.

Examples:
  gt hook status                    # Show my hook
  gt hook status greenplace/nux     # Show nux's hook`,
	Args: cobra.MaximumNArgs(1),
	RunE: runMoleculeStatus,
}

// hookShowCmd shows hook status in compact one-line format
var hookShowCmd = &cobra.Command{
	Use:   "show [agent]",
	Short: "Show what's on an agent's hook (compact)",
	Long: `Show what's on any agent's hook in compact one-line format.

With no argument, shows your own hook status (auto-detected from context).

Use cases:
- Witness checking polecat status
- Debugging coordination issues
- Quick status overview

Examples:
  gt hook show                         # What's on MY hook? (auto-detect)
  gt hook show gastown/polecats/nux    # What's nux working on?
  gt hook show gastown/witness         # What's the witness hooked to?

Output format (one line):
  gastown/polecats/nux: gt-abc123 'Fix the widget bug' [in_progress]`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHookShow,
}

// hookAttachCmd attaches a bead to a hook (alias for 'gt hook <bead-id>')
var hookAttachCmd = &cobra.Command{
	Use:   "attach <bead-id> [target]",
	Short: "Attach work to a hook",
	Long: `Attach a bead to your hook or another agent's hook.

With just a bead ID, attaches to your own hook (same as 'gt hook <bead-id>').
With a target, attaches to another agent's hook (for remote dispatch).

Examples:
  gt hook attach gt-abc                    # Attach to my hook
  gt hook attach gt-abc gastown/crew/max   # Attach to max's hook`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runHook(cmd, args)
	},
}

// hookDetachCmd detaches a bead from a hook (alias for 'gt hook clear')
var hookDetachCmd = &cobra.Command{
	Use:   "detach <bead-id> [target]",
	Short: "Detach work from a hook",
	Long: `Remove a specific bead from a hook (same as 'gt hook clear <bead-id>').

Examples:
  gt hook detach gt-abc               # Detach gt-abc from my hook
  gt hook detach gt-abc gastown/nux   # Detach gt-abc from nux's hook`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUnslingWith(cmd, args, hookDryRun, hookForce)
	},
}

// hookClearCmd clears the hook (alias for 'gt unhook')
var hookClearCmd = &cobra.Command{
	Use:   "clear [bead-id] [target]",
	Short: "Clear your hook (alias for 'gt unhook')",
	Long: `Remove work from your hook (alias for 'gt unhook').

With no arguments, clears your own hook. With a bead ID, only clears
if that specific bead is currently hooked. With a target, operates on
another agent's hook.

Examples:
  gt hook clear                       # Clear my hook (whatever's there)
  gt hook clear gt-abc                # Only clear if gt-abc is hooked
  gt hook clear greenplace/joe        # Clear joe's hook

Related commands:
  gt unhook           # Same as 'gt hook clear'
  gt unsling          # Same as 'gt hook clear'`,
	Args: cobra.MaximumNArgs(2),
	RunE: runHookClear,
}

var (
	hookSubject string
	hookMessage string
	hookDryRun  bool
	hookForce   bool
	hookClear   bool
)

func init() {
	// Flags for attaching work (gt hook <bead-id>)
	hookCmd.Flags().StringVarP(&hookSubject, "subject", "s", "", "Subject for handoff mail (optional)")
	hookCmd.Flags().StringVarP(&hookMessage, "message", "m", "", "Message for handoff mail (optional)")
	hookCmd.Flags().BoolVarP(&hookDryRun, "dry-run", "n", false, "Show what would be done")
	hookCmd.Flags().BoolVarP(&hookForce, "force", "f", false, "Replace existing incomplete hooked bead")
	hookCmd.Flags().BoolVar(&hookClear, "clear", false, "Clear your hook (alias for 'gt unhook')")

	// --json flag for status output (used when no args, i.e., gt hook --json)
	hookCmd.Flags().BoolVar(&moleculeJSON, "json", false, "Output as JSON (for status)")
	hookStatusCmd.Flags().BoolVar(&moleculeJSON, "json", false, "Output as JSON")
	hookShowCmd.Flags().BoolVar(&moleculeJSON, "json", false, "Output as JSON")

	// Flags for attach subcommand
	hookAttachCmd.Flags().BoolVarP(&hookForce, "force", "f", false, "Replace existing incomplete hooked bead")

	// Flags for detach subcommand (mirror unsling flags)
	hookDetachCmd.Flags().BoolVarP(&hookForce, "force", "f", false, "Detach even if work is incomplete")

	// Flags for clear subcommand (mirror unsling flags)
	hookClearCmd.Flags().BoolVarP(&hookDryRun, "dry-run", "n", false, "Show what would be done")
	hookClearCmd.Flags().BoolVarP(&hookForce, "force", "f", false, "Clear even if work is incomplete")

	hookCmd.AddCommand(hookStatusCmd)
	hookCmd.AddCommand(hookShowCmd)
	hookCmd.AddCommand(hookAttachCmd)
	hookCmd.AddCommand(hookDetachCmd)
	hookCmd.AddCommand(hookClearCmd)

	rootCmd.AddCommand(hookCmd)
}

// runHookOrStatus dispatches to status, clear, or hook based on args/flags
func runHookOrStatus(cmd *cobra.Command, args []string) error {
	// --clear flag is alias for 'gt unhook'
	if hookClear {
		return runUnslingWith(cmd, args, hookDryRun, hookForce)
	}
	if len(args) == 0 {
		// No args - show status
		return runMoleculeStatus(cmd, args)
	}
	// Has arg - attach work
	return runHook(cmd, args)
}

// runHookClear handles 'gt hook clear' - delegates to runUnsling
func runHookClear(cmd *cobra.Command, args []string) error {
	return runUnslingWith(cmd, args, hookDryRun, hookForce)
}

func runHook(_ *cobra.Command, args []string) error {
	beadID := args[0]
	if err := ensureCurrentHookWorktreeIntegrity(); err != nil {
		return err
	}

	// Reject non-bead-shaped first args before passing to bd show, which would
	// emit a confusing "bead 'set' not found" error. cobra has already failed to
	// match against a registered subcommand, so anything reaching here that
	// doesn't look like a bead ID is almost certainly a typo'd subcommand.
	// See GH#3701.
	if err := hookBeadArgError(beadID); err != nil {
		return err
	}

	// Parse optional target agent
	var targetAgent string
	if len(args) > 1 {
		targetAgent = args[1]
	}

	if err := hookPolecatRefusal(os.Getenv); err != nil {
		return err
	}

	// Verify the bead exists
	if err := verifyBeadExists(beadID); err != nil {
		return err
	}

	// Determine agent identity (target or self)
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

	// Find town root - needed for bd routing and agent bead updates
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")

	// Resolve the beads directory for the target agent.
	// For remote targets, resolve from the agent bead's prefix to find the
	// correct database. For self, use the local beads directory.
	var workDir string
	if targetAgent != "" {
		agentBeadID := agentIDToBeadID(agentID, townRoot)
		if agentBeadID == "" {
			return fmt.Errorf("could not convert agent ID %s to bead ID", agentID)
		}
		rigName := strings.Split(agentID, "/")[0]
		var fallbackPath string
		if rigName == "deacon" {
			fallbackPath = townRoot
		} else {
			fallbackPath = filepath.Join(townRoot, rigName)
		}
		workDir = beads.ResolveHookDir(townRoot, agentBeadID, fallbackPath)
	} else {
		workDir, err = findLocalBeadsDir()
		if err != nil {
			return fmt.Errorf("not in a beads workspace: %w", err)
		}
	}

	b := beads.New(workDir)

	// Check for existing hooked bead for this agent
	existingPinned, err := b.List(beads.ListOptions{
		Status:   beads.StatusHooked,
		Assignee: agentID,
		Priority: -1,
	})
	if err != nil {
		return fmt.Errorf("checking existing hooked beads: %w", err)
	}

	// If there's an existing hooked bead, decide how the new hook replaces it.
	// The old bead is left alone here: the new hook is written first and the
	// replacement only carried out once that write is committed, so a hook that
	// fails every retry leaves the previous hook exactly as it was (gt-34z9v).
	var displaced *beads.Issue
	var displacedByClose bool
	if len(existingPinned) > 0 {
		existing := existingPinned[0]

		// Skip if it's the same bead we're trying to pin
		if existing.ID == beadID {
			fmt.Printf("%s Already hooked: %s\n", style.Bold.Render("✓"), beadID)
			return nil
		}

		// Check if existing bead is complete
		isComplete, hasAttachment := checkPinnedBeadComplete(b, existing)

		switch {
		case isComplete:
			// Auto-replace completed bead: a molecule is closed, a naked bead
			// is only unpinned (it might still have value).
			fmt.Printf("%s Replacing completed bead %s...\n", style.Dim.Render("ℹ"), existing.ID)
			displaced, displacedByClose = existing, hasAttachment
		case hookForce:
			// Force replace incomplete bead
			fmt.Printf("%s Force-replacing incomplete bead %s...\n", style.Dim.Render("⚠"), existing.ID)
			displaced = existing
		default:
			// Existing incomplete bead blocks new hook
			return fmt.Errorf("existing hooked bead %s is incomplete (%s)\n  Use --force to replace, or complete the existing work first",
				existing.ID, existing.Title)
		}
	}

	if targetAgent != "" {
		fmt.Printf("%s Hooking %s for %s...\n", style.Bold.Render("🪝"), beadID, agentID)
	} else {
		fmt.Printf("%s Hooking %s...\n", style.Bold.Render("🪝"), beadID)
	}

	if hookDryRun {
		fmt.Printf("Would run: bd update %s --status=hooked --assignee=%s\n", beadID, agentID)
		if hookSubject != "" {
			fmt.Printf("  subject (for handoff mail): %s\n", hookSubject)
		}
		if hookMessage != "" {
			fmt.Printf("  context (for handoff mail): %s\n", hookMessage)
		}
		return nil
	}

	// Hook the new bead first, then release the bead it displaced. Ordering is
	// the point: a hook that fails every retry must leave the previously hooked
	// bead exactly as it was (gt-34z9v).
	if err := hookThenDisplace(displaced, func() error { return writeHookedBead(beadID, agentID) }, func(d *beads.Issue) error {
		if displacedByClose {
			return closeCompletedHookedMolecule(workDir, d.ID)
		}
		return releaseBeadToOpen(b, d.ID)
	}); err != nil {
		return err
	}

	if targetAgent != "" {
		fmt.Printf("%s Work attached to %s's hook\n", style.Bold.Render("✓"), agentID)
	} else {
		fmt.Printf("%s Work attached to hook (hooked bead)\n", style.Bold.Render("✓"))
	}

	// Update agent bead's hook_bead field (matches gt sling behavior)
	// This ensures gt hook / gt mol status can find hooked work via the agent bead
	updateAgentHookBead(agentID, beadID, workDir, townBeadsDir)

	if targetAgent != "" {
		fmt.Printf("  Use 'gt hook show %s' to verify\n", targetAgent)
	} else {
		fmt.Printf("  Use 'gt handoff' to restart with this work\n")
		fmt.Printf("  Use 'gt hook' to see hook status\n")
	}

	// Log hook event to activity feed (non-fatal)
	if err := events.LogFeed(events.TypeHook, agentID, events.HookPayload(beadID)); err != nil {
		fmt.Fprintf(os.Stderr, "%s Warning: failed to log hook event: %v\n", style.Dim.Render("⚠"), err)
	}

	return nil
}

func closeCompletedHookedMolecule(workDir, beadID string) error {
	return closeCompletedHookedMoleculeIn(pinnedBd(workDir), beadID)
}

// closeCompletedHookedMoleculeIn force-closes beadID in db, the replaced
// hook's database.
func closeCompletedHookedMoleculeIn(db beads.Client, beadID string) error {
	return db.ForceCloseWithReason("Auto-replaced by gt hook (molecule complete)", beadID)
}

// writeHookedBead commits beadID to agentID's hook. Dolt can fail with
// concurrency errors (HTTP 400) when multiple agents write simultaneously, so
// the write retries with exponential backoff, matching sling.go behavior.
func writeHookedBead(beadID, agentID string) error {
	const maxRetries = 5
	const baseBackoff = 500 * time.Millisecond
	const backoffMax = 10 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		hooked := beads.StatusHooked
		if err := pinnedBd(resolveBeadDir(beadID)).Update(beadID, beads.UpdateOptions{Status: &hooked, Assignee: &agentID}); err != nil {
			lastErr = err
			if attempt < maxRetries {
				backoff := slingBackoff(attempt, baseBackoff, backoffMax)
				fmt.Printf("%s Hook attempt %d failed, retrying in %v...\n", style.Warning.Render("⚠"), attempt, backoff)
				clockwork.NewRealClock().Sleep(backoff)
				continue
			}
			return fmt.Errorf("hooking bead after %d attempts: %w", maxRetries, lastErr)
		}
		break
	}
	return nil
}

// hookThenDisplace writes the new hook and, only once that write is committed,
// releases the bead it displaced. A displaced bead that cannot be released is
// reported, not fatal: the new hook is already live, so returning an error
// would tell the caller the hook failed when it did not. displaced is nil when
// nothing was hooked.
func hookThenDisplace(displaced *beads.Issue, write func() error, release func(*beads.Issue) error) error {
	if err := write(); err != nil {
		return err
	}
	if displaced == nil {
		return nil
	}
	if err := release(displaced); err != nil {
		style.PrintWarning("could not release replaced bead %s: %v", displaced.ID, err)
	}
	return nil
}

// releaseBeadToOpen sets id back to open with no assignee, the same release
// unhookFromPreviousOwner performs. The status and the assignee are cleared
// together so a released bead does not keep naming the agent whose hook it
// left (gt-34z9v).
func releaseBeadToOpen(db beads.Client, id string) error {
	open, unassigned := string(beads.StatusOpen), ""
	return db.Update(id, beads.UpdateOptions{Status: &open, Assignee: &unassigned})
}

// hookBeadArgError rejects a first argument that does not look like a bead
// ID: cobra fell through to the bead-id positional on a mistyped subcommand.
func hookBeadArgError(arg string) error {
	if isBeadID(arg) {
		return nil
	}
	return fmt.Errorf("%q is not a bead ID. See 'gt hook --help' for available subcommands and usage", arg)
}

// checkPinnedBeadComplete checks if a pinned bead's attached molecule is 100% complete.
// Returns (isComplete, hasAttachment):
// - isComplete=true if no molecule attached OR all molecule steps are closed
// - hasAttachment=true if there's an attached molecule
func checkPinnedBeadComplete(b *beads.Beads, issue *beads.Issue) (isComplete bool, hasAttachment bool) {
	// Check for attached molecule
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil || attachment.AttachedMolecule == "" {
		// No molecule attached - consider complete (naked bead)
		return true, false
	}

	// Get progress of attached molecule
	progress, err := getMoleculeProgressInfo(b, attachment.AttachedMolecule)
	if err != nil {
		// Can't determine progress - be conservative, treat as incomplete
		return false, true
	}

	if progress == nil {
		// No steps found - might be a simple issue, treat as complete
		return true, true
	}

	return progress.Complete, true
}

// runHookShow displays another agent's hook in compact one-line format.
func runHookShow(cmd *cobra.Command, args []string) error {
	if err := ensureCurrentHookWorktreeIntegrity(); err != nil {
		return err
	}

	var target string
	if len(args) > 0 {
		target = normalizeHookShowTarget(townRegistry(), args[0])
	} else {
		// Auto-detect current agent from context
		agentID, _, _, err := resolveSelfTarget()
		if err != nil {
			return fmt.Errorf("auto-detecting agent (use explicit argument): %w", err)
		}
		target = agentID
	}

	// Find beads directory.
	// For remote rig-level targets (e.g. "myndy_monorepo/refinery"), resolve the
	// rig's actual beads dir using the same rig-aware routing as runHook (attach).
	// Without this, gt hook show always queries whatever DB is local (typically HQ),
	// missing wisps stored in the target rig's database.
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return fmt.Errorf("not in a beads workspace: %w", err)
	}
	if len(args) > 0 {
		townRoot, townErr := workspace.FindFromCwd()
		if townErr == nil && townRoot != "" {
			workDir = resolveHookLookupWorkDir(workDir, target, townRoot)
		}
	}

	b := beads.New(workDir)
	hookedBeads, err := listAssignedActiveWork(b, target)
	if err != nil {
		return fmt.Errorf("listing active hook work: %w", err)
	}

	// If nothing found in local beads, also check town beads for hooked items.
	// Town beads (hq-*) are stored in ~/gt/.beads and can hold a hook.
	if len(hookedBeads) == 0 {
		townRoot, err := workspace.FindFromCwd()
		if err == nil && townRoot != "" {
			// Check town beads for hooked items
			townBeadsDir := filepath.Join(townRoot, ".beads")
			if _, err := os.Stat(townBeadsDir); err == nil {
				townBeads := beads.New(townBeadsDir)
				if townWork, err := listAssignedActiveWork(townBeads, target); err == nil && len(townWork) > 0 {
					hookedBeads = townWork
				}
			}
		}
	}

	// JSON output
	if moleculeJSON {
		type compactInfo struct {
			Agent  string `json:"agent"`
			BeadID string `json:"bead_id,omitempty"`
			Title  string `json:"title,omitempty"`
			Status string `json:"status"`
		}
		info := compactInfo{Agent: target}
		if len(hookedBeads) > 0 {
			info.BeadID = hookedBeads[0].ID
			info.Title = hookedBeads[0].Title
			info.Status = hookedBeads[0].Status
		} else {
			info.Status = "empty"
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(info)
	}

	// Compact one-line output
	if len(hookedBeads) == 0 {
		fmt.Printf("%s: (empty)\n", target)
		return nil
	}

	bead := hookedBeads[0]
	fmt.Printf("%s: %s '%s' [%s]\n", target, bead.ID, bead.Title, bead.Status)
	return nil
}

// hookPolecatRefusal refuses gt hook in a polecat session: polecats use gt
// done for lifecycle. GT_ROLE is checked first: coordinators (witness,
// etc.) may have a stale GT_POLECAT in their environment from spawning
// polecats, so only a role that parses as polecat blocks (compound forms like
// "gastown/polecats/Toast" included). With GT_ROLE unset, GT_POLECAT decides.
func hookPolecatRefusal(getenv func(string) string) error {
	if role := getenv("GT_ROLE"); role != "" {
		parsedRole, _, _ := parseRoleString(role)
		if parsedRole == RolePolecat {
			return fmt.Errorf("polecats cannot hook work (use gt done for handoff)")
		}
	} else if polecatName := getenv("GT_POLECAT"); polecatName != "" {
		return fmt.Errorf("polecats cannot hook work (use gt done for handoff)")
	}
	return nil
}

func ensureCurrentHookWorktreeIntegrity() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return nil
	}
	roleCtx := detectRole(cwd, townRoot)
	return ensureRoleWorktreeIntegrity(cwd, townRoot, roleCtx.Role)
}

// normalizeHookShowTarget resolves target aliases/shorthand to canonical agent IDs.
// Examples:
//   - "rig/polecat" -> "rig/polecats/polecat"
//
// If resolution fails, it returns the original target unchanged.
func normalizeHookShowTarget(reg *session.PrefixRegistry, target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return target
	}
	if target == "." || target == ".." || (strings.ContainsAny(target, `/\\`) && !safeAgentTargetPath(target)) {
		return target
	}

	// Use the same role/path resolver as dispatching commands, then convert
	// the resulting tmux session back to a canonical assignee address.
	// This keeps "hook show" target parsing aligned with sling/hook behavior.
	if sessionName, err := resolveRoleToSession(reg, target); err == nil && sessionName != "" {
		if addr, ok := sessionNameToCanonicalAddress(reg, sessionName, target); ok {
			return addr
		}
	}

	// Fallback for explicit/canonical addresses when resolver couldn't help.
	if identity, err := session.ParseAddressWithRegistry(target, reg); err == nil {
		return identity.Address()
	}

	// Direct shorthand expansion: rig/name → rig/polecats/name or rig/crew/name.
	// This handles the case where the session name roundtrip fails due to
	// uninitialized prefix registry. See GH#2371.
	parts := strings.Split(target, "/")
	if len(parts) == 2 && safeAgentPathSegment(parts[0]) && safeAgentPathSegment(parts[1]) {
		name := parts[1]
		// Check for known roles — don't expand those
		switch strings.ToLower(name) {
		case "witness", "deacon":
			// Already a valid canonical address
		default:
			// Check if it's a crew member by looking for the directory
			townRoot := detectTownRootFromCwd()
			if townRoot != "" {
				crewPath := filepath.Join(townRoot, parts[0], "crew", name)
				if info, statErr := os.Stat(crewPath); statErr == nil && info.IsDir() {
					return parts[0] + "/crew/" + name
				}
			}
			// Default to polecat
			return parts[0] + "/polecats/" + strings.ToLower(name)
		}
	}

	return target
}

// sessionNameToCanonicalAddress maps a tmux session name to a canonical agent
// assignee address (e.g., "gastown/polecats/toast").
//
// targetHint is the original user input and is used to seed a temporary
// prefix→rig mapping for deterministic parsing when reg does not know the
// target's rig (an uninitialized town registry, or a test's empty one).
func sessionNameToCanonicalAddress(reg *session.PrefixRegistry, sessionName, targetHint string) (string, bool) {
	if identity, err := session.ParseSessionNameWithRegistry(sessionName, reg); err == nil {
		return canonicalAssigneeAddress(identity), true
	}

	registry := session.NewPrefixRegistry()
	for rig, prefix := range reg.AllRigs() {
		registry.Register(prefix, rig)
	}
	parts := strings.Split(strings.TrimSpace(targetHint), "/")
	if len(parts) >= 2 && parts[0] != "" {
		rig := parts[0]
		registry.Register(reg.PrefixForRig(rig), rig)
	}

	identity, err := session.ParseSessionNameWithRegistry(sessionName, registry)
	if err != nil {
		return "", false
	}
	return canonicalAssigneeAddress(identity), true
}

// isBeadID checks if a string looks like a bead ID.
// Bead IDs have the format <prefix>-<id> where prefix starts with a
// lowercase letter and may contain underscores (e.g. gt-abc123,
// japanese_reader-id3a).
func isBeadID(s string) bool {
	dashIdx := strings.Index(s, "-")
	if dashIdx <= 0 || dashIdx >= len(s)-1 {
		return false
	}
	for i, c := range s[:dashIdx] {
		if i == 0 {
			if c < 'a' || c > 'z' {
				return false
			}
			continue
		}
		if !((c >= 'a' && c <= 'z') || c == '_') {
			return false
		}
	}
	return true
}
