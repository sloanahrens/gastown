package cmd

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	convoyops "github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// generateShortID generates a collision-resistant convoy ID suffix using base36.
// 5 chars of base36 gives ~60M possible values (36^5 = 60,466,176).
// Birthday paradox: ~1% collision at ~1,100 IDs — safe for convoy volumes. (#2063)
func generateShortID() string {
	return generateShortIDFromReader(rand.Reader)
}

func generateShortIDFromReader(r io.Reader) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 5)
	_, _ = io.ReadFull(r, b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// looksLikeIssueID checks if a string looks like a beads issue ID against the
// prefixes reg holds. Issue IDs have the format: prefix-id (e.g., gt-abc,
// bd-xyz, hq-123).
func looksLikeIssueID(reg *session.PrefixRegistry, s string) bool {
	if reg.HasKnownPrefix(s) {
		return true
	}
	// Pattern check: 2-3 lowercase letters followed by hyphen.
	// Covers unregistered short rig prefixes (e.g., nx, rpk).
	// Longer prefixes (4+ chars like nrpk) are caught by HasKnownPrefix
	// via the registry — no need to heuristic-match them here.
	hyphenIdx := strings.Index(s, "-")
	if hyphenIdx >= 2 && hyphenIdx <= 3 && len(s) > hyphenIdx+1 {
		prefix := s[:hyphenIdx]
		for _, c := range prefix {
			if c < 'a' || c > 'z' {
				return false
			}
		}
		return true
	}
	return false
}

// Convoy command flags
var (
	convoyMolecule     string
	convoyNotify       string
	convoyOwner        string
	convoyOwned        bool
	convoyMerge        string
	convoyBaseBranch   string
	convoyStatusJSON   bool
	convoyListJSON     bool
	convoyListStatus   string
	convoyListAll      bool
	convoyListTree     bool
	convoyStrandedJSON bool
	convoyCloseReason  string
	convoyCloseNotify  string
	convoyCloseForce   bool
	convoyCheckDryRun  bool
	convoyFromEpic     string
)

const (
	convoyStatusOpen           = convoyops.StatusOpen
	convoyStatusClosed         = convoyops.StatusClosed
	convoyStatusStagedReady    = convoyops.StatusStagedReady
	convoyStatusStagedWarnings = convoyops.StatusStagedWarnings
	trackedStatusUnknown       = convoyops.TrackedStatusUnknown
)

func normalizeConvoyStatus(status string) string { return convoyops.NormalizeStatus(status) }

func ensureKnownConvoyStatus(status string) error { return convoyops.EnsureKnownStatus(status) }

// isStagedStatus reports whether the given normalized status is a staged status.
func isStagedStatus(status string) bool {
	return strings.HasPrefix(status, "staged_")
}

func validateConvoyStatusTransition(currentStatus, targetStatus string) error {
	current := normalizeConvoyStatus(currentStatus)
	target := normalizeConvoyStatus(targetStatus)

	if err := ensureKnownConvoyStatus(current); err != nil {
		return err
	}
	if err := ensureKnownConvoyStatus(target); err != nil {
		return err
	}
	if current == target {
		return nil
	}

	// Original open ↔ closed transitions.
	if (current == convoyStatusOpen && target == convoyStatusClosed) ||
		(current == convoyStatusClosed && target == convoyStatusOpen) {
		return nil
	}

	// Staged → open (launch) and staged → closed (cancel) are allowed.
	if isStagedStatus(current) && (target == convoyStatusOpen || target == convoyStatusClosed) {
		return nil
	}

	// Staged ↔ staged transitions (re-stage with different result).
	if isStagedStatus(current) && isStagedStatus(target) {
		return nil
	}

	// REJECT: open → staged_* and closed → staged_* are not allowed.
	// (Falls through to the error below.)

	return fmt.Errorf("illegal convoy status transition %q -> %q", currentStatus, targetStatus)
}

var convoyCmd = &cobra.Command{
	Use:     "convoy",
	GroupID: GroupWork,
	Short:   "Track batches of work across rigs",
	RunE: func(cmd *cobra.Command, args []string) error {
		return requireSubcommand(cmd, args)
	},
	Long: `Manage convoys - the primary unit for tracking batched work.

A convoy is a persistent tracking unit that monitors related issues across
rigs. When you kick off work (even a single issue), a convoy tracks it so
you can see when it lands and what was included.

WHAT IS A CONVOY:
  - Persistent tracking unit with an ID (hq-*)
  - Tracks issues across rigs (frontend+backend, beads+gastown, etc.)
  - Auto-closes when all tracked issues complete → notifies subscribers
  - Can be reopened by adding more issues

WHAT IS A SWARM:
  - Ephemeral: "the workers currently assigned to a convoy's issues"
  - No separate ID - uses the convoy ID
  - Dissolves when work completes

TRACKING SEMANTICS:
  - 'tracks' relation is non-blocking (tracked issues don't block convoy)
  - Cross-prefix capable (convoy in hq-* tracks issues in gt-*, bd-*)
  - Landed: all tracked issues closed → notification sent to subscribers

COMMANDS:
  create    Create a convoy tracking specified issues
  add       Add issues to an existing convoy (reopens if closed)
  status    Show convoy progress, tracked issues, and active workers
  list      List convoys
  check     Auto-close convoys whose tracked issues are all done
  stranded  Find ready work with no active workers
  close     Close a convoy (verifies all items done, or use --force)`,
}

var convoyCreateCmd = &cobra.Command{
	Use:   "create <name> [issues...]",
	Short: "Create a new convoy",
	Long: `Create a new convoy that tracks the specified issues.

The convoy is created in town-level beads (hq-* prefix) and can track
issues across any rig.

The --owner flag specifies who requested the convoy (receives completion
notification by default). If not specified, defaults to created_by.
The --notify flag adds additional subscribers beyond the owner.

The --merge flag sets the merge strategy for all work in the convoy:
  mr      Create merge-request bead, refinery processes (default)
  local   Keep on feature branch (for upstream PRs, human review)

Examples:
  gt convoy create "Deploy v2.0" gt-abc bd-xyz
  gt convoy create "Release prep" gt-abc --notify           # defaults to mayor/
  gt convoy create "Release prep" gt-abc --notify ops/      # notify ops/
  gt convoy create "Feature rollout" gt-a gt-b --owner mayor/ --notify ops/
  gt convoy create "Feature rollout" gt-a gt-b gt-c --molecule mol-release
  gt convoy create --owned "Manual deploy" gt-abc           # caller-managed lifecycle

  # Auto-discover issues from an epic's children:
  gt convoy create --from-epic gt-epic-abc
  gt convoy create --from-epic gt-epic-abc --owned --merge=local`,
	Args:         cobra.ArbitraryArgs,
	SilenceUsage: true,
	RunE:         runConvoyCreate,
}

var convoyStatusCmd = &cobra.Command{
	Use:   "status [convoy-id]",
	Short: "Show convoy status",
	Long: `Show detailed status for a convoy.

Displays convoy metadata, tracked issues, and completion progress.
Without an ID, shows status of all active convoys.`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE:         runConvoyStatus,
}

var convoyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List convoys",
	Long: `List convoys, showing open convoys by default.

Examples:
  gt convoy list              # Open convoys only (default)
  gt convoy list --all        # All convoys (open + closed)
  gt convoy list --status=closed  # Recently landed
  gt convoy list --tree       # Show convoy + child status tree
  gt convoy list --json`,
	SilenceUsage: true,
	RunE:         runConvoyList,
}

var convoyAddCmd = &cobra.Command{
	Use:   "add <convoy-id> <issue-id> [issue-id...]",
	Short: "Add issues to an existing convoy",
	Long: `Add issues to an existing convoy.

If the convoy is closed, it will be automatically reopened.

Examples:
  gt convoy add hq-cv-abc gt-new-issue
  gt convoy add hq-cv-abc gt-issue1 gt-issue2 gt-issue3`,
	Args:         cobra.MinimumNArgs(2),
	SilenceUsage: true,
	RunE:         runConvoyAdd,
}

var convoyCheckCmd = &cobra.Command{
	Use:   "check [convoy-id]",
	Short: "Check and auto-close completed convoys",
	Long: `Check convoys and auto-close any where all tracked issues are complete.

Without arguments, checks all open convoys. With a convoy ID, checks only that convoy.

This handles cross-rig convoy completion: convoys in town beads tracking issues
in rig beads won't auto-close via bd close alone. This command bridges that gap.

Can be run manually or by deacon patrol to ensure convoys close promptly.

Examples:
  gt convoy check              # Check all open convoys
  gt convoy check hq-cv-abc    # Check specific convoy
  gt convoy check --dry-run    # Preview what would close without acting`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE:         runConvoyCheck,
}

var convoyStrandedCmd = &cobra.Command{
	Use:   "stranded",
	Short: "Find stranded convoys (ready work, stuck, or empty) needing attention",
	Long: `Find convoys that have ready issues but no workers processing them,
stuck convoys (tracked issues but none ready), or empty convoys that need cleanup.

A convoy is "stranded" when:
- Convoy is open AND either:
  - Has tracked issues that are ready but unassigned, OR
  - Has tracked issues but none are ready (stuck — waiting on dependencies/workers), OR
  - Has 0 tracked issues (empty — needs auto-close via convoy check)

Use this to detect convoys that need feeding or cleanup. The daemon's convoy
manager runs the same scan and feeds ready issues to their rigs.

Examples:
  gt convoy stranded              # Show stranded convoys
  gt convoy stranded --json       # Machine-readable output for automation`,
	SilenceUsage: true,
	RunE:         runConvoyStranded,
}

var convoyCloseCmd = &cobra.Command{
	Use:   "close <convoy-id>",
	Short: "Close a convoy",
	Long: `Close a convoy, optionally with a reason.

By default, verifies that all tracked issues are closed before allowing the
close. Use --force to close regardless of tracked issue status.

The close is idempotent - closing an already-closed convoy is a no-op.

Examples:
  gt convoy close hq-cv-abc                           # Close (all items must be done)
  gt convoy close hq-cv-abc --force                   # Force close abandoned convoy
  gt convoy close hq-cv-abc --reason="no longer needed" --force
  gt convoy close hq-cv-xyz --notify mayor/`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runConvoyClose,
}

func init() {
	// Create flags
	convoyCreateCmd.Flags().StringVar(&convoyMolecule, "molecule", "", "Associated molecule ID")
	convoyCreateCmd.Flags().StringVar(&convoyOwner, "owner", "", "Owner who requested convoy (gets completion notification)")
	convoyCreateCmd.Flags().StringVar(&convoyNotify, "notify", "", "Additional address to notify on completion (default: mayor/ if flag used without value)")
	convoyCreateCmd.Flags().Lookup("notify").NoOptDefVal = "mayor/"
	convoyCreateCmd.Flags().BoolVar(&convoyOwned, "owned", false, "Mark convoy as caller-managed lifecycle (no automatic witness/refinery registration)")
	convoyCreateCmd.Flags().StringVar(&convoyMerge, "merge", "", "Merge strategy: mr (merge queue, default), local (keep on branch)")
	convoyCreateCmd.Flags().StringVar(&convoyBaseBranch, "base-branch", "", "Target branch for polecats (e.g., 'feat/extraction-review')")
	convoyCreateCmd.Flags().StringVar(&convoyFromEpic, "from-epic", "", "Auto-discover tracked issues from an epic's slingable children")

	// Status flags
	convoyStatusCmd.Flags().BoolVar(&convoyStatusJSON, "json", false, "Output as JSON")

	// List flags
	convoyListCmd.Flags().BoolVar(&convoyListJSON, "json", false, "Output as JSON")
	convoyListCmd.Flags().StringVar(&convoyListStatus, "status", "", "Filter by status (open, closed)")
	convoyListCmd.Flags().BoolVar(&convoyListAll, "all", false, "Show all convoys (open and closed)")
	convoyListCmd.Flags().BoolVar(&convoyListTree, "tree", false, "Show convoy + child status tree")

	// Check flags
	convoyCheckCmd.Flags().BoolVar(&convoyCheckDryRun, "dry-run", false, "Preview what would close without acting")

	// Stranded flags
	convoyStrandedCmd.Flags().BoolVar(&convoyStrandedJSON, "json", false, "Output as JSON")

	// Close flags
	convoyCloseCmd.Flags().StringVar(&convoyCloseReason, "reason", "", "Reason for closing the convoy")
	convoyCloseCmd.Flags().StringVar(&convoyCloseNotify, "notify", "", "Agent to notify on close (e.g., mayor/)")
	convoyCloseCmd.Flags().BoolVarP(&convoyCloseForce, "force", "f", false, "Close even if tracked issues are still open")

	// Add subcommands
	convoyCmd.AddCommand(convoyCreateCmd)
	convoyCmd.AddCommand(convoyStatusCmd)
	convoyCmd.AddCommand(convoyListCmd)
	convoyCmd.AddCommand(convoyAddCmd)
	convoyCmd.AddCommand(convoyCheckCmd)
	convoyCmd.AddCommand(convoyStrandedCmd)
	convoyCmd.AddCommand(convoyCloseCmd)

	rootCmd.AddCommand(convoyCmd)
}

// getTownBeadsDir returns the town root directory for bd commands.
// Convoy commands run bd from town root (not .beads/) so bd discovers
// the correct database via its own workspace detection.
func getTownBeadsDir() (string, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return townRoot, nil
}

// collectEpicChildren does a BFS walk of an epic's parent-child hierarchy and
// returns all slingable leaf descendants (task, bug, feature, chore).
func collectEpicChildren(epicID string) ([]string, error) {
	epic, err := bdShow(epicID)
	if err != nil {
		return nil, fmt.Errorf("epic '%s' not found: %w", epicID, err)
	}
	if epic.IssueType != "epic" {
		return nil, fmt.Errorf("'%s' is not an epic (type: %s); --from-epic only works with epic beads", epicID, epic.IssueType)
	}

	var issueIDs []string
	visited := make(map[string]bool)
	queue := []string{epicID}
	visited[epicID] = true

	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		children, err := bdListChildren(parentID)
		if err != nil {
			style.PrintWarning("couldn't list children of %s: %v", parentID, err)
			continue
		}

		for _, child := range children {
			if visited[child.ID] {
				continue
			}
			visited[child.ID] = true

			if convoyops.IsSlingableType(child.IssueType) {
				issueIDs = append(issueIDs, child.ID)
			} else {
				// Non-slingable types (sub-epics, decisions) — recurse to find slingable descendants
				queue = append(queue, child.ID)
			}
		}
	}

	if len(issueIDs) == 0 {
		return nil, fmt.Errorf("epic '%s' has no slingable children (task, bug, feature, chore)", epicID)
	}
	return issueIDs, nil
}

// convoyCLI is what the gt convoy create, add, status and list commands
// reach outside the process: the town, the bd that answers for it and the
// terminal. realConvoyCLI finds the town from the cwd and uses the bd on
// PATH; a unit test builds one over a temp town and an in-process bd, so the
// command's decisions run with nothing spawned and no global swapped.
type convoyCLI struct {
	townRoot func() (string, error)
	// townDB opens the town database at townBeads (the town root); nil is
	// bd pinned to its .beads.
	townDB      func(townBeads string) convoyops.Store
	out, warn   io.Writer
	entropy     io.Reader                   // convoy ID suffixes
	sender      func() string               // the default convoy owner
	ensureTypes func(beadsDir string) error // registers the convoy types and statuses
}

// realConvoyCLI is the running gt's convoy collaborators.
func realConvoyCLI() convoyCLI {
	return convoyCLI{
		townRoot:    getTownBeadsDir,
		out:         os.Stdout,
		warn:        os.Stderr,
		entropy:     rand.Reader,
		sender:      detectSender,
		ensureTypes: ensureConvoyTypes,
	}
}

// db is the town database at townBeads.
func (c convoyCLI) db(townBeads string) convoyops.Store {
	if c.townDB != nil {
		return c.townDB(townBeads)
	}
	return beads.NewPinned(beads.ResolveBeadsDir(townBeads))
}

// town is the convoy package's view of c's town.
func (c convoyCLI) town() (convoyops.Town, error) {
	root, err := c.townRoot()
	if err != nil {
		return convoyops.Town{}, err
	}
	return c.townAt(root), nil
}

// townAt is the convoy package's view of the town at root.
func (c convoyCLI) townAt(root string) convoyops.Town {
	town := convoyops.Town{Root: root, Out: c.out, Warn: c.warn}
	if c.townDB != nil {
		town.Open = c.townDB
		town.Issues = c.townDB(root)
	}
	return town
}

// ensureConvoyTypes registers the custom types (including 'convoy') and
// statuses (staged_ready, staged_warnings) in the beads dir.
func ensureConvoyTypes(beadsDir string) error {
	if err := beads.EnsureCustomTypes(beadsDir); err != nil {
		return fmt.Errorf("ensuring custom types: %w", err)
	}
	if err := beads.EnsureCustomStatuses(beadsDir); err != nil {
		return fmt.Errorf("ensuring custom statuses: %w", err)
	}
	return nil
}

// convoyCreateOptions are gt convoy create's flags.
type convoyCreateOptions struct {
	molecule   string
	notify     string
	owner      string
	owned      bool
	merge      string
	baseBranch string
	fromEpic   string
}

// convoyCreateOptionsFromFlags is the options the cobra flags hold.
func convoyCreateOptionsFromFlags() convoyCreateOptions {
	return convoyCreateOptions{
		molecule:   convoyMolecule,
		notify:     convoyNotify,
		owner:      convoyOwner,
		owned:      convoyOwned,
		merge:      convoyMerge,
		baseBranch: convoyBaseBranch,
		fromEpic:   convoyFromEpic,
	}
}

func runConvoyCreate(cmd *cobra.Command, args []string) error {
	return realConvoyCLI().create(convoyCreateOptionsFromFlags(), args)
}

// create is gt convoy create with opts in c's town.
func (c convoyCLI) create(opts convoyCreateOptions, args []string) error {
	// Validate --merge flag if provided
	if err := validateConvoyMergeFlag(opts.merge); err != nil {
		return err
	}

	var name string
	var trackedIssues []string

	if opts.fromEpic != "" {
		// --from-epic mode: auto-discover children
		epicIssues, err := collectEpicChildren(opts.fromEpic)
		if err != nil {
			return err
		}
		trackedIssues = epicIssues

		// Use epic title as convoy name unless a name arg was provided
		if len(args) > 0 {
			name = args[0]
		} else {
			if epic, err := bdShow(opts.fromEpic); err == nil {
				name = epic.Title
			} else {
				name = fmt.Sprintf("From epic %s", opts.fromEpic)
			}
		}
	} else {
		// Standard mode: explicit issue list
		if len(args) == 0 {
			return fmt.Errorf("at least one argument is required\nUsage: gt convoy create <name> <issue-id> [issue-id...]\n       gt convoy create --from-epic <epic-id>")
		}
		name = args[0]
		trackedIssues = args[1:]

		// If first arg looks like an issue ID (has beads prefix), treat all args as issues
		// and auto-generate a name from the first issue's title.
		//
		// looksLikeIssueID only inspects the first token, so it also fires on a
		// human-readable name that opens with a short lowercase word ("om-gate
		// coverage: om"). The shape check keeps such a name in the name
		// position; folding it into the tracked set recorded the convoy's own
		// title as a phantom issue (gt-gsky).
		if looksLikeIssueID(townRegistry(), name) && beads.IsBeadIDToken(name) {
			trackedIssues = args
			name = fmt.Sprintf("Tracking %s", args[0])
			if town, townErr := c.town(); townErr == nil {
				if details := town.IssueDetails(args[0]); details != nil && details.Title != "" {
					name = details.Title
				}
			}
		}

		if len(trackedIssues) == 0 {
			return fmt.Errorf("at least one issue ID is required\nUsage: gt convoy create <name> <issue-id> [issue-id...]")
		}
	}

	// Refuse before creating anything: an edge to a non-ID target can never be
	// resolved, so the convoy would report it unknown on every `convoy check`
	// and never auto-close (gt-gsky).
	if err := validateTrackingTargets(trackedIssues); err != nil {
		return fmt.Errorf("convoy create: %w", err)
	}

	townBeads, err := c.townRoot()
	if err != nil {
		return err
	}

	// Resolve the actual .beads directory (follows redirects) before calling
	// EnsureCustomTypes/Statuses, which expect a .beads path, not a workspace root.
	resolvedBeads := beads.ResolveBeadsDir(townBeads)

	// Ensure custom types (including 'convoy') and statuses (staged_ready,
	// staged_warnings) are registered in town beads. This handles cases where
	// install didn't complete or beads was initialized manually.
	if err := c.ensureTypes(resolvedBeads); err != nil {
		return err
	}

	// Create convoy issue in town beads
	description := fmt.Sprintf("Convoy tracking %d issues", len(trackedIssues))

	// Default owner to creator identity if not specified
	owner := opts.owner
	if owner == "" {
		owner = c.sender()
	}
	convoyFieldValues := &beads.ConvoyFields{
		Owner:      owner,
		Notify:     opts.notify,
		Merge:      opts.merge,
		Molecule:   opts.molecule,
		BaseBranch: opts.baseBranch,
	}
	description = beads.SetConvoyFields(&beads.Issue{Description: description}, convoyFieldValues)

	// Guard against flag-like convoy names (gt-e0kx5)
	if beads.IsFlagLikeTitle(name) {
		return fmt.Errorf("refusing to create convoy: name %q looks like a CLI flag", name)
	}

	// Generate convoy ID with cv- prefix
	convoyID := fmt.Sprintf("hq-cv-%s", generateShortIDFromReader(c.entropy))

	if _, err := c.db(townBeads).Create(beads.CreateOptions{
		ID:          convoyID,
		Title:       name,
		Description: description,
		Labels:      convoyLabels(opts.owned),
		Priority:    -1,
	}); err != nil {
		return fmt.Errorf("creating convoy: %w", err)
	}

	// Notify address is stored in description (line 166-168) and read from there

	// Add 'tracks' relations for each tracked issue
	trackedCount := 0
	for _, issueID := range trackedIssues {
		if err := addTrackingRelationWith(c.db(townBeads), townBeads, convoyID, issueID); err != nil {
			style.FprintWarning(c.warn, "couldn't track %s: %s", issueID, err)
		} else {
			trackedCount++
		}
	}

	// Output
	fmt.Fprintf(c.out, "%s Created convoy 🚚 %s\n\n", style.Bold.Render("✓"), convoyID)
	fmt.Fprintf(c.out, "  Name:     %s\n", name)
	if opts.fromEpic != "" {
		fmt.Fprintf(c.out, "  Epic:     %s\n", opts.fromEpic)
	}
	fmt.Fprintf(c.out, "  Tracking: %d issues\n", trackedCount)
	if opts.fromEpic == "" && len(trackedIssues) > 0 {
		fmt.Fprintf(c.out, "  Issues:   %s\n", strings.Join(trackedIssues, ", "))
	}
	if owner != "" {
		fmt.Fprintf(c.out, "  Owner:    %s\n", owner)
	}
	if opts.notify != "" {
		fmt.Fprintf(c.out, "  Notify:   %s\n", opts.notify)
	}
	if opts.merge != "" {
		fmt.Fprintf(c.out, "  Merge:    %s\n", opts.merge)
	}
	if opts.molecule != "" {
		fmt.Fprintf(c.out, "  Molecule: %s\n", opts.molecule)
	}
	if opts.baseBranch != "" {
		fmt.Fprintf(c.out, "  Base:     %s\n", opts.baseBranch)
	}
	if opts.owned {
		fmt.Fprintf(c.out, "  Owned:    %s\n", style.Warning.Render("caller-managed lifecycle"))
	}

	if opts.owned {
		fmt.Fprintf(c.out, "\n  %s\n", style.Dim.Render("Owned convoy: caller manages lifecycle via gt convoy close"))
	} else {
		fmt.Fprintf(c.out, "\n  %s\n", style.Dim.Render("Convoy auto-closes when all tracked issues complete"))
	}

	return nil
}

func runConvoyAdd(cmd *cobra.Command, args []string) error {
	return realConvoyCLI().add(args)
}

// add is gt convoy add in c's town.
func (c convoyCLI) add(args []string) error {
	convoyID := args[0]
	issuesToAdd := args[1:]

	// Same gate as create, and for the same reason: reject non-ID targets
	// before this command reopens or touches the convoy (gt-gsky).
	if err := validateTrackingTargets(issuesToAdd); err != nil {
		return fmt.Errorf("convoy add: %w", err)
	}

	townBeads, err := c.townRoot()
	if err != nil {
		return err
	}

	// Validate convoy exists and get its status
	db := c.db(townBeads)
	convoy, err := db.Show(convoyID)
	if err != nil {
		return fmt.Errorf("convoy '%s' not found", convoyID)
	}

	// Verify it's actually a convoy type
	if !convoyops.IsConvoyIssue(convoy.Type, convoy.Labels) {
		return fmt.Errorf("'%s' is not a convoy (type: %s)", convoyID, convoy.Type)
	}
	if err := ensureKnownConvoyStatus(convoy.Status); err != nil {
		return fmt.Errorf("convoy '%s' has invalid lifecycle state: %w", convoyID, err)
	}

	// If convoy is closed, reopen it
	reopened := false
	if normalizeConvoyStatus(convoy.Status) == convoyStatusClosed {
		// closed→open is always valid; ensureKnownConvoyStatus above guarantees
		// the current status is known, so no additional transition check needed.
		open := "open"
		if err := db.Update(convoyID, beads.UpdateOptions{Status: &open}); err != nil {
			return fmt.Errorf("couldn't reopen convoy: %w", err)
		}
		if fields := beads.ParseConvoyFields(convoy); fields != nil && fields.CompletionNotifiedAt != "" {
			fields.CompletionNotifiedAt = ""
			newDesc := beads.SetConvoyFields(&beads.Issue{Description: convoy.Description}, fields)
			if err := db.Update(convoyID, beads.UpdateOptions{Description: &newDesc}); err != nil {
				return fmt.Errorf("couldn't clear convoy completion notification state: %w", err)
			}
		}
		if err := c.townAt(townBeads).PersistJSONL(); err != nil {
			return fmt.Errorf("couldn't persist reopened convoy to JSONL: %w", err)
		}
		reopened = true
		fmt.Fprintf(c.out, "%s Reopened convoy %s\n", style.Bold.Render("↺"), convoyID)
	}

	// Add 'tracks' relations for each issue
	var added []string
	for _, issueID := range issuesToAdd {
		if err := addTrackingRelationWith(c.db(townBeads), townBeads, convoyID, issueID); err != nil {
			style.FprintWarning(c.warn, "couldn't add %s: %s", issueID, err)
		} else {
			added = append(added, issueID)
		}
	}

	// Output
	if reopened {
		fmt.Fprintln(c.out)
	}
	fmt.Fprintf(c.out, "%s Added %d issue(s) to convoy 🚚 %s\n", style.Bold.Render("✓"), len(added), convoyID)
	if len(added) > 0 {
		fmt.Fprintf(c.out, "  Issues: %s\n", strings.Join(added, ", "))
	}

	return nil
}

func runConvoyCheck(cmd *cobra.Command, args []string) error {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}

	town := convoyops.StdTown(townBeads)

	// If a specific convoy ID is provided, check only that convoy
	if len(args) == 1 {
		convoyID := args[0]
		return town.CheckOne(convoyID, convoyCheckDryRun)
	}

	// Check all open convoys
	closed, err := town.CheckAll(context.Background(), convoyCheckDryRun)
	if err != nil {
		return err
	}

	if len(closed) == 0 {
		fmt.Println("No convoys ready to close.")
	} else {
		if convoyCheckDryRun {
			fmt.Printf("%s Would auto-close %d convoy(s):\n", style.Warning.Render("⚠"), len(closed))
		} else {
			fmt.Printf("%s Auto-closed %d convoy(s):\n", style.Bold.Render("✓"), len(closed))
		}
		for _, c := range closed {
			fmt.Printf("  🚚 %s: %s\n", c.ID, c.Title)
		}
	}

	return nil
}

func runConvoyClose(cmd *cobra.Command, args []string) error {
	convoyID := args[0]

	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}

	stdout, err := beads.RunBdJSON(townBeads, "show", convoyID, "--json")
	if err != nil {
		return fmt.Errorf("convoy '%s' not found", convoyID)
	}

	var convoys []struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Status      string   `json:"status"`
		Type        string   `json:"issue_type"`
		Description string   `json:"description"`
		Labels      []string `json:"labels"`
	}
	if err := json.Unmarshal(stdout, &convoys); err != nil {
		return fmt.Errorf("parsing convoy data: %w", err)
	}

	if len(convoys) == 0 {
		return fmt.Errorf("convoy '%s' not found", convoyID)
	}

	convoy := convoys[0]

	// Verify it's actually a convoy type
	if !convoyops.IsConvoyIssue(convoy.Type, convoy.Labels) {
		return fmt.Errorf("'%s' is not a convoy (type: %s)", convoyID, convoy.Type)
	}
	if err := ensureKnownConvoyStatus(convoy.Status); err != nil {
		return fmt.Errorf("convoy '%s' has invalid lifecycle state: %w", convoyID, err)
	}

	// Idempotent: if already closed, just report it
	if normalizeConvoyStatus(convoy.Status) == convoyStatusClosed {
		fmt.Printf("%s Convoy %s is already closed\n", style.Dim.Render("○"), convoyID)
		return convoyops.StdTown(townBeads).PersistAndNotify(convoyID, convoy.Title)
	}
	if err := validateConvoyStatusTransition(convoy.Status, convoyStatusClosed); err != nil {
		return fmt.Errorf("can't close convoy '%s': %w", convoyID, err)
	}

	// Verify all tracked issues are done (unless --force)
	tracked, err := convoyops.StdTown(townBeads).TrackedIssues(convoyID)
	if err != nil {
		// If we can't check tracked issues, require --force
		if !convoyCloseForce {
			return fmt.Errorf("couldn't verify tracked issues: %w\n  Use --force to close anyway", err)
		}
		style.PrintWarning("couldn't verify tracked issues: %v", err)
	}

	if len(tracked) > 0 && !convoyCloseForce {
		var openIssues []convoyops.TrackedIssue
		for _, t := range tracked {
			if t.Status != "closed" && t.Status != "tombstone" {
				openIssues = append(openIssues, t)
			}
		}

		if len(openIssues) > 0 {
			fmt.Printf("%s Convoy %s has %d open issue(s):\n\n", style.Warning.Render("⚠"), convoyID, len(openIssues))
			for _, t := range openIssues {
				status := "○"
				if t.Status == "in_progress" || t.Status == "hooked" {
					status = "▶"
				}
				fmt.Printf("    %s %s: %s [%s]\n", status, t.ID, t.Title, t.Status)
			}
			fmt.Printf("\n  Use %s to close anyway.\n", style.Bold.Render("--force"))
			return fmt.Errorf("convoy has %d open issue(s)", len(openIssues))
		}
	}

	// Build close reason
	reason := convoyCloseReason
	if reason == "" {
		if convoyCloseForce {
			reason = "Force closed"
		} else {
			reason = "All tracked issues completed"
		}
	}

	// Close the convoy
	if err := convoyops.StdTown(townBeads).CloseAndExport(convoyID, reason); err != nil {
		return fmt.Errorf("closing convoy: %w", err)
	}

	fmt.Printf("%s Closed convoy 🚚 %s: %s\n", style.Bold.Render("✓"), convoyID, convoy.Title)
	if convoyCloseReason != "" {
		fmt.Printf("  Reason: %s\n", convoyCloseReason)
	}

	// Report cleanup summary
	if len(tracked) > 0 {
		closedCount := 0
		openCount := 0
		for _, t := range tracked {
			if t.Status == "closed" || t.Status == "tombstone" {
				closedCount++
			} else {
				openCount++
			}
		}
		fmt.Printf("  Tracked: %d issue(s) (%d closed", len(tracked), closedCount)
		if openCount > 0 {
			fmt.Printf(", %d still open", openCount)
		}
		fmt.Println(")")
	}

	// Report molecule if present
	convoyFields := beads.ParseConvoyFields(&beads.Issue{Description: convoy.Description})
	if convoyFields != nil && convoyFields.Molecule != "" {
		fmt.Printf("  Molecule: %s (not auto-detached)\n", convoyFields.Molecule)
	}

	// Send notification if --notify flag provided
	if convoyCloseNotify != "" {
		convoyops.StdTown(townBeads).NotifyClosed(convoyCloseNotify, convoyID, convoy.Title, reason)
	} else {
		// Check if convoy has a notify address in description
		convoyops.StdTown(townBeads).NotifyCompletion(convoyID, convoy.Title)
	}

	return nil
}

func runConvoyStranded(cmd *cobra.Command, args []string) error {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}

	town := convoyops.StdTown(townBeads)
	town.Prefixes = townRegistry()
	stranded, err := town.FindStranded(context.Background())
	if err != nil {
		return err
	}

	if convoyStrandedJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(stranded)
	}

	if len(stranded) == 0 {
		fmt.Println("No stranded convoys found.")
		return nil
	}

	fmt.Printf("%s Found %d stranded convoy(s):\n\n", style.Warning.Render("⚠"), len(stranded))
	for _, s := range stranded {
		fmt.Printf("  🚚 %s: %s\n", s.ID, s.Title)
		if s.ReadyCount == 0 && s.TrackedCount == 0 {
			fmt.Printf("     Empty convoy (0 tracked issues) — needs cleanup\n")
		} else if s.ReadyCount == 0 && s.TrackedCount > 0 {
			fmt.Printf("     %d tracked issues, 0 ready — needs agent review\n", s.TrackedCount)
		} else {
			fmt.Printf("     Ready issues: %d (of %d tracked)\n", s.ReadyCount, s.TrackedCount)
			for _, issueID := range s.ReadyIssues {
				fmt.Printf("       • %s\n", issueID)
			}
		}
		fmt.Println()
	}

	// Separate feed advice, needs-attention convoys, and cleanup advice.
	var feedable, needsAttention, empty []convoyops.StrandedConvoy
	for _, s := range stranded {
		if s.ReadyCount > 0 {
			feedable = append(feedable, s)
		} else if s.TrackedCount > 0 {
			needsAttention = append(needsAttention, s)
		} else {
			empty = append(empty, s)
		}
	}

	if len(feedable) > 0 {
		fmt.Println("The daemon's convoy manager feeds these; to dispatch a ready issue now, run:")
		for _, s := range feedable {
			fmt.Printf("  gt sling <ready-issue> <rig>   # convoy %s\n", s.ID)
		}
	}
	if len(needsAttention) > 0 {
		if len(feedable) > 0 {
			fmt.Println()
		}
		fmt.Println("Needs agent review (tracked issues exist but none are ready):")
		for _, s := range needsAttention {
			fmt.Printf("  🚚 %s (%d tracked, 0 ready)\n", s.ID, s.TrackedCount)
		}
	}
	if len(empty) > 0 {
		if len(feedable) > 0 || len(needsAttention) > 0 {
			fmt.Println()
		}
		fmt.Println("To close empty convoys, run:")
		for _, s := range empty {
			fmt.Printf("  gt convoy check %s\n", s.ID)
		}
	}
	return nil
}

func runConvoyStatus(cmd *cobra.Command, args []string) error {
	return realConvoyCLI().status(convoyStatusJSON, args)
}

// status is gt convoy status in c's town; asJSON is --json.
func (c convoyCLI) status(asJSON bool, args []string) error {
	townBeads, err := c.townRoot()
	if err != nil {
		return err
	}

	// If no ID provided, show all active convoys
	if len(args) == 0 {
		return c.showAllStatus(townBeads, asJSON)
	}

	convoyID := args[0]

	// Check if it's a numeric shortcut (e.g., "1" instead of "hq-cv-xyz")
	if n, err := strconv.Atoi(convoyID); err == nil && n > 0 {
		resolved, err := c.resolveConvoyNumber(townBeads, n)
		if err != nil {
			return err
		}
		convoyID = resolved
	}

	// Get convoy details
	convoy, err := c.db(townBeads).Show(convoyID)
	if err != nil {
		return fmt.Errorf("convoy '%s' not found", convoyID)
	}

	// Check if convoy is owned (caller-managed lifecycle)
	isOwned := convoyops.HasLabel(convoy.Labels, "gt:owned")

	tracked, err := c.townAt(townBeads).TrackedIssues(convoyID)
	if err != nil {
		return fmt.Errorf("getting tracked issues for %s: %w", convoyID, err)
	}

	// Count completed
	completed := 0
	for _, t := range tracked {
		if t.Status == "closed" {
			completed++
		}
	}

	if asJSON {
		lifecycle := "system-managed"
		if isOwned {
			lifecycle = "caller-managed"
		}
		type jsonStatus struct {
			ID            string                   `json:"id"`
			Title         string                   `json:"title"`
			Status        string                   `json:"status"`
			Owned         bool                     `json:"owned"`
			Lifecycle     string                   `json:"lifecycle"`
			MergeStrategy string                   `json:"merge_strategy,omitempty"`
			Agent         string                   `json:"agent,omitempty"`
			Tracked       []convoyops.TrackedIssue `json:"tracked"`
			Completed     int                      `json:"completed"`
			Total         int                      `json:"total"`
		}
		out := jsonStatus{
			ID:            convoy.ID,
			Title:         convoy.Title,
			Status:        convoy.Status,
			Owned:         isOwned,
			Lifecycle:     lifecycle,
			MergeStrategy: convoyMergeFromFields(convoy.Description),
			Agent:         convoyops.AgentFromConvoyDescription(convoy.Description),
			Tracked:       tracked,
			Completed:     completed,
			Total:         len(tracked),
		}
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	// Human-readable output
	fmt.Fprintf(c.out, "🚚 %s %s\n\n", style.Bold.Render(convoy.ID+":"), convoy.Title)
	fmt.Fprintf(c.out, "  Status:    %s\n", formatConvoyStatus(convoy.Status))
	fmt.Fprintf(c.out, "  Owned:     %s\n", formatYesNo(isOwned))
	if isOwned {
		fmt.Fprintf(c.out, "  Lifecycle: %s\n", style.Warning.Render("caller-managed"))
	} else {
		fmt.Fprintf(c.out, "  Lifecycle: %s\n", "system-managed")
	}
	merge := convoyMergeFromFields(convoy.Description)
	if merge != "" {
		fmt.Fprintf(c.out, "  Merge:     %s\n", merge)
	}
	if agent := convoyops.AgentFromConvoyDescription(convoy.Description); agent != "" {
		fmt.Fprintf(c.out, "  Agent:     %s\n", agent)
	}
	fmt.Fprintf(c.out, "  Progress:  %d/%d completed\n", completed, len(tracked))
	fmt.Fprintf(c.out, "  Created:   %s\n", convoy.CreatedAt)
	if convoy.ClosedAt != "" {
		fmt.Fprintf(c.out, "  Closed:    %s\n", convoy.ClosedAt)
	}

	if len(tracked) > 0 {
		fmt.Fprintf(c.out, "\n  %s\n", style.Bold.Render("Tracked Issues:"))
		for _, t := range tracked {
			// Status symbol: ✓ closed, ▶ in_progress/hooked, ? unknown (cross-rig unreachable), ○ other
			status := "○"
			switch t.Status {
			case "closed":
				status = "✓"
			case "in_progress", "hooked":
				status = "▶"
			case trackedStatusUnknown:
				status = "?"
			}

			// Show assignee in brackets (extract short name from path like gastown/polecats/goose -> goose)
			bracketContent := t.IssueType
			if t.Assignee != "" {
				parts := strings.Split(t.Assignee, "/")
				bracketContent = parts[len(parts)-1] // Last part of path
			} else if bracketContent == "" {
				bracketContent = "unassigned"
			}

			line := fmt.Sprintf("    %s %s: %s [%s]", status, t.ID, t.Title, bracketContent)
			if t.Worker != "" {
				workerDisplay := "@" + t.Worker
				if t.WorkerAge != "" {
					workerDisplay += fmt.Sprintf(" (%s)", t.WorkerAge)
				}
				line += fmt.Sprintf("  %s", style.Dim.Render(workerDisplay))
			}
			fmt.Fprintln(c.out, line)
		}
	}

	// Hint for owned convoys when all issues are complete
	if isOwned && completed == len(tracked) && len(tracked) > 0 && normalizeConvoyStatus(convoy.Status) == convoyStatusOpen {
		fmt.Fprintf(c.out, "\n  %s\n", style.Dim.Render("All issues complete. Close with: gt convoy close "+convoyID))
	}

	return nil
}

func (c convoyCLI) showAllStatus(townBeads string, asJSON bool) error {
	convoys, err := c.townAt(townBeads).ListConvoys("open", false)
	if err != nil {
		return fmt.Errorf("listing convoys: %w", err)
	}

	if len(convoys) == 0 {
		fmt.Fprintln(c.out, "No active convoys.")
		fmt.Fprintln(c.out, "Create a convoy with: gt convoy create <name> [issues...]")
		return nil
	}

	if asJSON {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(convoys)
	}

	fmt.Fprintf(c.out, "%s\n\n", style.Bold.Render("Active Convoys"))
	for _, cv := range convoys {
		ownedTag := ""
		if convoyops.HasLabel(cv.Labels, "gt:owned") {
			ownedTag = " " + style.Warning.Render("[owned]")
		}
		fmt.Fprintf(c.out, "  🚚 %s: %s%s\n", cv.ID, cv.Title, ownedTag)
	}
	fmt.Fprintf(c.out, "\nUse 'gt convoy status <id>' for detailed status.\n")

	return nil
}

func runConvoyList(cmd *cobra.Command, args []string) error {
	return realConvoyCLI().list(convoyListOptions{
		json:   convoyListJSON,
		status: convoyListStatus,
		all:    convoyListAll,
		tree:   convoyListTree,
	})
}

// convoyListOptions are gt convoy list's flags.
type convoyListOptions struct {
	json   bool
	status string
	all    bool
	tree   bool
}

// list is gt convoy list with opts in c's town.
func (c convoyCLI) list(opts convoyListOptions) error {
	townBeads, err := c.townRoot()
	if err != nil {
		return err
	}

	convoys, err := c.townAt(townBeads).ListConvoys(opts.status, opts.all)
	if err != nil {
		return fmt.Errorf("listing convoys: %w", err)
	}

	if opts.json {
		// Enrich each convoy with tracked issues and completion counts
		type convoyListEntry struct {
			ID        string                   `json:"id"`
			Title     string                   `json:"title"`
			Status    string                   `json:"status"`
			CreatedAt string                   `json:"created_at"`
			Tracked   []convoyops.TrackedIssue `json:"tracked"`
			Completed int                      `json:"completed"`
			Total     int                      `json:"total"`
		}
		enriched := make([]convoyListEntry, 0, len(convoys))
		for _, cv := range convoys {
			tracked, err := c.townAt(townBeads).TrackedIssues(cv.ID)
			if err != nil {
				style.FprintWarning(c.warn, "skipping convoy %s: %v", cv.ID, err)
				continue
			}
			if tracked == nil {
				tracked = []convoyops.TrackedIssue{} // Ensure JSON [] not null
			}
			completed := 0
			for _, t := range tracked {
				if t.Status == "closed" {
					completed++
				}
			}
			enriched = append(enriched, convoyListEntry{
				ID:        cv.ID,
				Title:     cv.Title,
				Status:    cv.Status,
				CreatedAt: cv.CreatedAt,
				Tracked:   tracked,
				Completed: completed,
				Total:     len(tracked),
			})
		}
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(enriched)
	}

	if len(convoys) == 0 {
		fmt.Fprintln(c.out, "No convoys found.")
		fmt.Fprintln(c.out, "Create a convoy with: gt convoy create <name> [issues...]")
		return nil
	}

	// Tree view: show convoys with their child issues
	if opts.tree {
		return c.printConvoyTree(townBeads, convoys)
	}

	fmt.Fprintf(c.out, "%s\n\n", style.Bold.Render("Convoys"))
	for i, cv := range convoys {
		status := formatConvoyStatus(cv.Status)
		ownedTag := ""
		if convoyops.HasLabel(cv.Labels, "gt:owned") {
			ownedTag = " " + style.Warning.Render("[owned]")
		}
		fmt.Fprintf(c.out, "  %d. 🚚 %s: %s %s%s\n", i+1, cv.ID, cv.Title, status, ownedTag)
	}
	fmt.Fprintf(c.out, "\nUse 'gt convoy status <id>' or 'gt convoy status <n>' for detailed view.\n")

	return nil
}

// printConvoyTree displays convoys with their child issues in a tree format.
func (c convoyCLI) printConvoyTree(townBeads string, convoys []convoyops.ListedConvoy) error {
	for _, cv := range convoys {
		// Get tracked issues for this convoy
		tracked, err := c.townAt(townBeads).TrackedIssues(cv.ID)
		if err != nil {
			style.FprintWarning(c.warn, "skipping convoy %s: %v", cv.ID, err)
			continue
		}

		// Count completed
		completed := 0
		for _, t := range tracked {
			if t.Status == "closed" {
				completed++
			}
		}

		// Print convoy header with progress
		total := len(tracked)
		progress := ""
		if total > 0 {
			progress = fmt.Sprintf(" (%d/%d)", completed, total)
		}
		ownedTag := ""
		if convoyops.HasLabel(cv.Labels, "gt:owned") {
			ownedTag = " " + style.Warning.Render("[owned]")
		}
		fmt.Fprintf(c.out, "🚚 %s: %s%s%s\n", cv.ID, cv.Title, progress, ownedTag)

		// Print tracked issues as tree children
		for i, t := range tracked {
			// Determine tree connector
			isLast := i == len(tracked)-1
			connector := "├──"
			if isLast {
				connector = "└──"
			}

			// Status symbol: ✓ closed, ▶ in_progress/hooked, ○ other
			status := "○"
			switch t.Status {
			case "closed":
				status = "✓"
			case "in_progress", "hooked":
				status = "▶"
			}

			fmt.Fprintf(c.out, "%s %s %s: %s\n", connector, status, t.ID, t.Title)
		}

		// Add blank line between convoys
		fmt.Fprintln(c.out)
	}

	return nil
}

func convoyLabels(owned bool) []string {
	if owned {
		return []string{"gt:convoy", "gt:owned"}
	}
	return []string{"gt:convoy"}
}

// convoyMergeFromFields extracts the merge strategy from a convoy description
// using the typed ConvoyFields accessor.
// Returns the strategy string ("mr", "local") or empty string if not set.
func convoyMergeFromFields(description string) string {
	fields := beads.ParseConvoyFields(&beads.Issue{Description: description})
	if fields == nil {
		return ""
	}
	return fields.Merge
}

// formatYesNo returns "yes" or "no" for a boolean value.
func formatYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func formatConvoyStatus(status string) string {
	switch status {
	case "open":
		return style.Warning.Render("●")
	case "closed":
		return style.Success.Render("✓")
	case "in_progress":
		return style.Info.Render("→")
	default:
		return status
	}
}

// resolveConvoyNumber converts a numeric shortcut (1, 2, 3...) to a convoy ID.
// Numbers correspond to the order shown in 'gt convoy list'.
func (c convoyCLI) resolveConvoyNumber(townBeads string, n int) (string, error) {
	convoys, err := c.townAt(townBeads).ListConvoys("", false)
	if err != nil {
		return "", fmt.Errorf("listing convoys: %w", err)
	}

	if n < 1 || n > len(convoys) {
		return "", fmt.Errorf("convoy %d not found (have %d convoys)", n, len(convoys))
	}

	return convoys[n-1].ID, nil
}
