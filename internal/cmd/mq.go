package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
)

// MQ command flags
var (
	// Submit flags
	mqSubmitBranch    string
	mqSubmitIssue     string
	mqSubmitEpic      string
	mqSubmitPriority  int
	mqSubmitNoCleanup bool
	mqSubmitSkipDeps  bool
	mqSubmitResubmit  bool

	// Retry flags
	mqRetryNow bool

	// Reject flags
	mqRejectReason    string
	mqRejectNotify    bool
	mqRejectStdin     bool // Read reason from stdin
	mqRejectNoRecover bool // Skip dead-worker recovery of the source bead

	// List command flags
	mqListReady  bool
	mqListStatus string
	mqListWorker string
	mqListEpic   string
	mqListJSON   bool
	mqListVerify bool

	// Status command flags
	mqStatusJSON bool

	// Integration land flags
	mqIntegrationLandForce     bool
	mqIntegrationLandSkipTests bool
	mqIntegrationLandDryRun    bool

	// Integration status flags
	mqIntegrationStatusJSON bool

	// Integration create flags
	mqIntegrationCreateBranch     string
	mqIntegrationCreateBaseBranch string
	mqIntegrationCreateForce      bool
)

var mqCmd = &cobra.Command{
	Use:     "mq",
	Aliases: []string{"mr"},
	GroupID: GroupWork,
	Short:   "Merge queue operations",
	RunE:    requireSubcommand,
	Long: `Manage merge requests and the merge queue for a rig.

Alias: 'gt mr' is equivalent to 'gt mq' (merge request vs merge queue).

The merge queue tracks work branches from polecats waiting to be merged.
Use these commands to view, submit, retry, and manage merge requests.`,
}

var mqSubmitCmd = &cobra.Command{
	Use:   "submit",
	Short: "Submit current branch to the merge queue",
	Long: `Submit the current branch to the merge queue.

Creates a merge-request bead that will be processed by the Refinery.

Auto-detection:
  - Branch: current git branch
  - Issue: parsed from branch name (e.g., polecat/Nux/gp-xyz → gt-xyz)
  - Worker: parsed from branch name
  - Rig: detected from current directory
  - Target: automatically determined (see below)
  - Priority: inherited from source issue

Target branch auto-detection:
  1. If --epic is specified: target the integration branch for <epic> (using configured template)
  2. If source issue has a parent epic with an integration branch: target it
  3. Otherwise: target main

This ensures batch work on epics automatically flows to integration branches.

Polecat auto-cleanup:
  When run from a polecat work branch (polecat/<worker>/<issue>), this command
  automatically triggers polecat shutdown after submitting the MR. The polecat
  sends a lifecycle request to its Witness and waits for termination.

  Use --no-cleanup to disable this behavior (e.g., if you want to submit
  multiple MRs or continue working).

Examples:
  gt mq submit                           # Auto-detect everything + auto-cleanup
  gt mq submit --issue gp-abc            # Explicit issue
  gt mq submit --epic gt-xyz             # Target integration branch explicitly
  gt mq submit --priority 0              # Override priority (P0)
  gt mq submit --no-cleanup              # Submit without auto-cleanup`,
	RunE: runMqSubmit,
}

var mqRetryCmd = &cobra.Command{
	Use:   "retry <rig> <mr-id>",
	Short: "Retry a failed merge request",
	Long: `Retry a failed merge request.

Resets a failed MR so it can be processed again by the refinery.
The MR must be in a failed state (open with an error).

Examples:
  gt mq retry greenplace gp-mr-abc123
  gt mq retry greenplace gp-mr-abc123 --now`,
	Args: cobra.ExactArgs(2),
	RunE: runMQRetry,
}

var mqListCmd = &cobra.Command{
	Use:   "list <rig>",
	Short: "Show the merge queue",
	Long: `Show the merge queue for a rig.

Lists all pending merge requests waiting to be processed.

Output format:
  ID          STATUS       PRIORITY  BRANCH                    WORKER  AGE
  gt-mr-001   ready        P0        polecat/Nux/gp-xyz        Nux     5m
  gt-mr-002   in_progress  P1        polecat/Toast/gt-abc      Toast   12m
  gt-mr-003   blocked      P1        polecat/Capable/gt-def    Capable 8m
              (waiting on gt-mr-001)

Examples:
  gt mq list greenplace
  gt mq list greenplace --ready
  gt mq list greenplace --status=open
  gt mq list greenplace --worker=Nux`,
	Args: cobra.ExactArgs(1),
	RunE: runMQList,
}

var mqRejectCmd = &cobra.Command{
	Use:   "reject <rig> <mr-id-or-branch>",
	Short: "Reject a merge request",
	Long: `Manually reject a merge request.

This closes the MR with a 'rejected' status without merging.
The source issue is NOT closed (work is not done); when the worker can no
longer act on it, its source bead is reopened for redispatch — unless the
bead's own close_reason marks it deliberately canceled/superseded/already
merged, in which case reopening is skipped automatically.

For an MR whose source work is superseded, duplicate, or already landed via
another MR, pass --no-recover so the source bead is left untouched no matter
what its close_reason says.

Examples:
  gt mq reject greenplace polecat/Nux/gp-xyz --reason "Does not meet requirements"
  gt mq reject greenplace mr-Nux-12345 --reason "Superseded by gt-me9t" --no-recover`,
	Args: cobra.ExactArgs(2),
	RunE: runMQReject,
}

// Post-merge flags
var mqPostMergeSkipBranchDelete bool
var mqPostMergeLandedCommit string

var mqPostMergeCmd = &cobra.Command{
	Use:   "post-merge <rig> <mr-id-or-branch>",
	Short: "Run post-merge cleanup (close MR, delete branch)",
	Long: `Perform post-merge cleanup after a successful merge.

This command consolidates post-merge steps into a single atomic operation:
	 1. Verify the target branch contains the submitted source head
	 2. Close the MR bead (status: merged)
	 3. Close the source issue
	 4. Delete the remote polecat branch at the submitted head (unless --skip-branch-delete)

Designed for use by the refinery formula after a successful merge to main.
The branch name is read from the MR bead, so the MR id alone addresses the branch.

When <ref> resolves to no MR bead — the MR was closed and purged, taking the
only record of its branch with it — <ref> is read as a branch name and the
branch is cleaned on its own. Branch-only cleanup deletes nothing else, and
only when the current remote tip is preserved on the default branch, no PR is
open on the branch, and the delete is lease-guarded at the tip it verified. It
refuses --skip-branch-delete and --landed-commit, which shape MR cleanup and
have nothing to act on without an MR. Only a polecat/ work branch is a
candidate; a ref that names neither an MR nor an existing polecat branch is an
error, not a no-op.

The default proof checks the submitted commit_sha for reachability from the
target (exact tip, ancestor, or a content-preserving rebase via patch-id).
A rebase that required conflict resolution legitimately changes patch-ids, so
that proof cannot hold even though the content landed correctly. For that
case, pass --landed-commit with the SHA that was actually pushed to the
target; it is still verified as reachable from the target (so a wrong or
stale SHA is rejected) and bound to the submitted MR by file (every file the
submitted branch touched must also appear in the attested commit's own
diff), so an unrelated on-target commit is rejected too.

Examples:
  gt mq post-merge gastown gt-mr-abc123
  gt mq post-merge gastown gt-mr-abc123 --skip-branch-delete
  gt mq post-merge gastown gt-mr-abc123 --landed-commit 2bb0bf7f
  gt mq post-merge gastown polecat/Nux/gt-xyz    # orphaned branch, MR bead gone`,
	Args: cobra.ExactArgs(2),
	RunE: runMQPostMerge,
}

type mqPostMergeManager interface {
	FindMRForPostMerge(idOrBranch string) (*refinery.MergeRequest, error)
	PostMergeMR(mr *refinery.MergeRequest) (*refinery.PostMergeResult, error)
}

type mqPostMergeGit interface {
	VerifyPushedCommitReachableFromPushTarget(remote, branch, commit string) error
	PushRemoteBranchTip(remote, branch string) (string, error)
	PushRemoteRefTargetStatus(remote string, ref git.RemoteRef, target string) (git.BranchPreservationStatus, error)
	RemoteDefaultBranch() string
	CleanDefaultBranchBaseRef(remote, defaultBranch string) string
	CleanBaseRef(remote, defaultBranch, target string) string
	FetchPrune(remote string) error
	HasOpenPullRequest(ref git.PullRequestRef) bool
	Rev(ref string) (string, error)
	MergeBase(a, b string) (string, error)
	DiffNameOnly(base, head string) ([]string, error)
	DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error
	DeleteBranch(branch string, force bool) error
}

type mqPostMergeBranchCleanup struct {
	Branch        string
	Target        string // orphan cleanup: ref the branch's work had to be preserved on
	NoBranch      bool
	Skipped       bool
	Disabled      bool
	OpenPR        bool
	AlreadyGone   bool
	RemoteDeleted bool
	LocalDeleted  bool
}

var mqConflictCmd = &cobra.Command{
	Use:   "record-conflict <rig> <mr-id>",
	Short: "Record a merge conflict against an MR and file a resolution task",
	Long: `Record a merge conflict against a merge request.

This command consolidates conflict-handling into a single atomic operation:
	 1. Create a dispatchable conflict-resolution task
	 2. Block the MR on that task (it re-enters the queue once the task closes)
	 3. Record conflict_task_id, last_conflict_sha, and retry_count on the MR
	 4. Clear any pre_verified metadata, which is now stale (the target moved)

Designed for use by the refinery formula immediately after aborting a
conflicted merge rehearsal, so the MR wisp's bookkeeping stays in sync with
the conflict-resolution task it depends on.

Examples:
  gt mq record-conflict gastown gt-mr-abc123`,
	Args: cobra.ExactArgs(2),
	RunE: runMQConflict,
}

var mqStatusCmd = &cobra.Command{
	Use:   "status <id>",
	Short: "Show detailed merge request status",
	Long: `Display detailed information about a merge request.

Shows all MR fields, current status with timestamps, dependencies,
blockers, and processing history.

Example:
  gt mq status gp-mr-abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runMqStatus,
}

var mqIntegrationCmd = &cobra.Command{
	Use:   "integration",
	Short: "Manage integration branches for epics",
	RunE:  requireSubcommand,
	Long: `Manage integration branches for batch work on epics.

Integration branches allow multiple MRs for an epic to target a shared
branch instead of main. After all epic work is complete, the integration
branch is landed to main as a single atomic unit.

Commands:
  create  Create an integration branch for an epic
  land    Merge integration branch to main
  status  Show integration branch status`,
}

var mqIntegrationCreateCmd = &cobra.Command{
	Use:   "create <epic-id>",
	Short: "Create an integration branch for an epic",
	Long: `Create an integration branch for batch work on an epic.

Creates a branch from main and pushes it to origin. Future MRs for this
epic's children can target this branch.

Branch naming:
  Default: integration/<sanitized-title> (e.g., integration/add-user-auth)
  Config:  Set merge_queue.integration_branch_template in rig settings
  Override: Use --branch flag for one-off customization

Template variables:
  {title}  - Sanitized epic title (e.g., "add-user-authentication")
  {epic}   - Full epic ID (e.g., "RA-123")
  {prefix} - Epic prefix before first hyphen (e.g., "RA")
  {user}   - Git user.name (e.g., "klauern")

If two epics produce the same branch name, a numeric suffix from the
epic ID is appended automatically (e.g., integration/add-auth-123).

Actions:
  1. Verify epic exists
  2. Create branch from main (using template or --branch)
  3. Push to origin
  4. Store actual branch name in epic metadata

Examples:
  gt mq integration create gt-auth-epic
  # Creates integration/add-user-authentication (from epic title)

  gt mq integration create RA-123 --branch "klauern/PROJ-1234/{epic}"
  # Creates klauern/PROJ-1234/RA-123`,
	Args: cobra.ExactArgs(1),
	RunE: runMqIntegrationCreate,
}

var mqIntegrationLandCmd = &cobra.Command{
	Use:   "land <epic-id>",
	Short: "Merge integration branch to main",
	Long: `Merge an epic's integration branch to main.

Lands all work for an epic by merging its integration branch to main
as a single atomic merge commit.

Actions:
  1. Verify all MRs targeting integration/<epic> are merged
  2. Verify integration branch exists
  3. Merge integration/<epic> to main (--no-ff)
  4. Run tests on main
  5. Push to origin
  6. Delete integration branch
  7. Update epic status

Options:
  --force       Land even if some MRs still open
  --skip-tests  Skip test run
  --dry-run     Preview only, make no changes

Examples:
  gt mq integration land gt-auth-epic
  gt mq integration land gt-auth-epic --dry-run
  gt mq integration land gt-auth-epic --force --skip-tests`,
	Args: cobra.ExactArgs(1),
	RunE: runMqIntegrationLand,
}

var mqIntegrationStatusCmd = &cobra.Command{
	Use:   "status <epic-id>",
	Short: "Show integration branch status for an epic",
	Long: `Display the status of an integration branch.

Shows:
  - Integration branch name and creation date
  - Number of commits ahead of main
  - Merged MRs (closed, targeting integration branch)
  - Pending MRs (open, targeting integration branch)

Example:
  gt mq integration status gt-auth-epic`,
	Args: cobra.ExactArgs(1),
	RunE: runMqIntegrationStatus,
}

func init() {
	// Submit flags
	mqSubmitCmd.Flags().StringVar(&mqSubmitBranch, "branch", "", "Source branch (default: current branch)")
	mqSubmitCmd.Flags().StringVar(&mqSubmitIssue, "issue", "", "Source issue ID (default: parse from branch name)")
	mqSubmitCmd.Flags().StringVar(&mqSubmitEpic, "epic", "", "Target epic's integration branch instead of main")
	mqSubmitCmd.Flags().IntVarP(&mqSubmitPriority, "priority", "p", -1, "Override priority (0-4, default: inherit from issue)")
	mqSubmitCmd.Flags().BoolVar(&mqSubmitNoCleanup, "no-cleanup", false, "Don't auto-cleanup after submit (for polecats)")
	mqSubmitCmd.Flags().BoolVar(&mqSubmitSkipDeps, "skip-deps", false, "Skip molecule step dependency check")
	mqSubmitCmd.Flags().BoolVar(&mqSubmitResubmit, "resubmit", false, "Resubmit after a fix (skips dependency check)")

	// Retry flags
	mqRetryCmd.Flags().BoolVar(&mqRetryNow, "now", false, "Immediately process instead of waiting for refinery loop")

	// List flags
	mqListCmd.Flags().BoolVar(&mqListReady, "ready", false, "Show only ready-to-merge (no blockers)")
	mqListCmd.Flags().StringVar(&mqListStatus, "status", "", "Filter by status (open, in_progress, closed)")
	mqListCmd.Flags().StringVar(&mqListWorker, "worker", "", "Filter by worker name")
	mqListCmd.Flags().StringVar(&mqListEpic, "epic", "", "Show MRs targeting integration/<epic>")
	mqListCmd.Flags().BoolVar(&mqListJSON, "json", false, "Output as JSON")
	mqListCmd.Flags().BoolVar(&mqListVerify, "verify", false, "Verify branches exist in git (shows MISSING for deleted branches)")

	// Reject flags
	mqRejectCmd.Flags().StringVarP(&mqRejectReason, "reason", "r", "", "Reason for rejection (required unless --stdin)")
	mqRejectCmd.Flags().BoolVar(&mqRejectNotify, "notify", false, "Send mail notification to worker")
	mqRejectCmd.Flags().BoolVar(&mqRejectStdin, "stdin", false, "Read reason from stdin (avoids shell quoting issues)")
	mqRejectCmd.Flags().BoolVar(&mqRejectNoRecover, "no-recover", false, "Do not reopen the source bead for redispatch (use for superseded/duplicate/already-merged work)")

	// Status flags
	mqStatusCmd.Flags().BoolVar(&mqStatusJSON, "json", false, "Output as JSON")

	// Post-merge flags
	mqPostMergeCmd.Flags().BoolVar(&mqPostMergeSkipBranchDelete, "skip-branch-delete", false, "Skip remote branch deletion")
	mqPostMergeCmd.Flags().StringVar(&mqPostMergeLandedCommit, "landed-commit", "", "Attest the actual SHA pushed to the target when it differs from the submitted commit_sha (e.g. a conflict-resolved rebase)")

	// Add subcommands
	mqCmd.AddCommand(mqSubmitCmd)
	mqCmd.AddCommand(mqRetryCmd)
	mqCmd.AddCommand(mqListCmd)
	mqCmd.AddCommand(mqRejectCmd)
	mqCmd.AddCommand(mqStatusCmd)
	mqCmd.AddCommand(mqPostMergeCmd)
	mqCmd.AddCommand(mqConflictCmd)

	// Integration branch subcommands
	mqIntegrationCreateCmd.Flags().StringVar(&mqIntegrationCreateBranch, "branch", "", "Override branch name template (supports {title}, {epic}, {prefix}, {user})")
	mqIntegrationCreateCmd.Flags().StringVar(&mqIntegrationCreateBaseBranch, "base-branch", "", "Create integration branch from this branch instead of main")
	mqIntegrationCreateCmd.Flags().BoolVar(&mqIntegrationCreateForce, "force", false, "Recreate integration branch even if one already exists")
	mqIntegrationCmd.AddCommand(mqIntegrationCreateCmd)

	// Integration land flags
	mqIntegrationLandCmd.Flags().BoolVar(&mqIntegrationLandForce, "force", false, "Land even if some MRs still open")
	mqIntegrationLandCmd.Flags().BoolVar(&mqIntegrationLandSkipTests, "skip-tests", false, "Skip test run")
	mqIntegrationLandCmd.Flags().BoolVar(&mqIntegrationLandDryRun, "dry-run", false, "Preview only, make no changes")
	mqIntegrationCmd.AddCommand(mqIntegrationLandCmd)

	// Integration status flags
	mqIntegrationStatusCmd.Flags().BoolVar(&mqIntegrationStatusJSON, "json", false, "Output as JSON")
	mqIntegrationCmd.AddCommand(mqIntegrationStatusCmd)

	mqCmd.AddCommand(mqIntegrationCmd)

	rootCmd.AddCommand(mqCmd)
}

// findCurrentRig determines the current rig from the working directory.
// Returns the rig name and rig object, or an error if not in a rig.
func findCurrentRig(townRoot string) (string, *rig.Rig, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, fmt.Errorf("getting current directory: %w", err)
	}

	// Get relative path from town root to cwd
	relPath, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return "", nil, fmt.Errorf("computing relative path: %w", err)
	}

	// The first component of the relative path should be the rig name
	parts := strings.Split(relPath, string(filepath.Separator))
	rigName := ""
	if len(parts) > 0 && parts[0] != "" && parts[0] != "." {
		rigName = parts[0]
	}

	// When gt is invoked via shell alias (cd ~/gt && gt), cwd is the town
	// root and relPath is ".". Fall back to GT_RIG env var.
	if rigName == "" {
		rigName = os.Getenv("GT_RIG")
	}
	if rigName == "" {
		return "", nil, fmt.Errorf("not inside a rig directory (and GT_RIG not set)")
	}

	// Load rig manager and get the rig
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		return "", nil, fmt.Errorf("rig '%s' not found: %w", rigName, err)
	}

	return rigName, r, nil
}

func runMQRetry(cmd *cobra.Command, args []string) error {
	rigName := args[0]
	mrID := args[1]

	mgr, _, _, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}

	// Get the MR first to show info
	mr, err := mgr.GetMR(mrID)
	if err != nil {
		if err == refinery.ErrMRNotFound {
			return fmt.Errorf("merge request '%s' not found in rig '%s'", mrID, rigName)
		}
		return fmt.Errorf("getting merge request: %w", err)
	}

	// Show what we're retrying
	fmt.Printf("Retrying merge request: %s\n", mrID)
	fmt.Printf("  Branch: %s\n", mr.Branch)
	fmt.Printf("  Worker: %s\n", mr.Worker)
	if mr.Error != "" {
		fmt.Printf("  Previous error: %s\n", style.Dim.Render(mr.Error))
	}

	// Perform the retry
	if err := mgr.Retry(mrID, mqRetryNow); err != nil {
		if err == refinery.ErrMRNotFailed {
			return fmt.Errorf("merge request '%s' has not failed (status: %s)", mrID, mr.Status)
		}
		return fmt.Errorf("retrying merge request: %w", err)
	}

	if mqRetryNow {
		fmt.Printf("%s Merge request processed\n", style.Bold.Render("✓"))
	} else {
		fmt.Printf("%s Merge request queued for retry\n", style.Bold.Render("✓"))
		fmt.Printf("  %s\n", style.Dim.Render("Will be processed on next refinery cycle"))
	}

	return nil
}

func runMQReject(cmd *cobra.Command, args []string) error {
	// Handle --stdin: read reason from stdin (avoids shell quoting issues)
	if mqRejectStdin {
		if mqRejectReason != "" {
			return fmt.Errorf("cannot use --stdin with --reason/-r")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		mqRejectReason = strings.TrimRight(string(data), "\n")
	}

	// Require reason via --reason or --stdin
	if mqRejectReason == "" {
		return fmt.Errorf("required flag \"reason\" not set (use --reason/-r or --stdin)")
	}

	rigName := args[0]
	mrIDOrBranch := args[1]

	mgr, _, _, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}

	result, err := mgr.RejectMR(mrIDOrBranch, mqRejectReason, mqRejectNotify, mqRejectNoRecover)
	if err != nil {
		return fmt.Errorf("rejecting MR: %w", err)
	}

	fmt.Printf("%s Rejected: %s\n", style.Bold.Render("✗"), result.Branch)
	fmt.Printf("  Worker: %s\n", result.Worker)
	fmt.Printf("  Reason: %s\n", mqRejectReason)

	if result.IssueID != "" {
		statusNote := "status unknown"
		if result.SourceIssueStatus != "" {
			statusNote = "status: " + result.SourceIssueStatus
		}
		fmt.Printf("  Issue:  %s %s\n", result.IssueID, style.Dim.Render("("+statusNote+")"))
	}

	if mqRejectNotify {
		fmt.Printf("  %s\n", style.Dim.Render("Worker notified via mail"))
	}

	return nil
}

func runMQConflict(_ *cobra.Command, args []string) error {
	rigName := args[0]
	mrID := args[1]

	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	eng := refinery.NewEngineer(r)
	taskID, err := eng.RecordConflict(mrID)
	if err != nil {
		return fmt.Errorf("recording conflict for %s: %w", mrID, err)
	}

	if taskID == "" {
		fmt.Printf("%s Conflict resolution deferred for %s (merge slot busy, will retry)\n", style.Dim.Render("○"), mrID)
		return nil
	}

	fmt.Printf("%s Recorded conflict for %s\n", style.Bold.Render("✓"), mrID)
	fmt.Printf("  Conflict task: %s\n", taskID)
	fmt.Printf("  %s\n", style.Dim.Render("MR blocked until the task closes"))
	return nil
}

func runMQPostMerge(_ *cobra.Command, args []string) error {
	rigName := args[0]
	ref := args[1]

	mgr, r, _, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	rigGit, err := getRigGit(r.Path)
	if err != nil {
		return fmt.Errorf("post-merge proof: %w", err)
	}

	result, branchCleanup, orphan, err := resolveMQPostMerge(mgr, r.Path, rigGit, ref, mqPostMergeSkipBranchDelete, mqPostMergeLandedCommit)
	if err != nil {
		return err
	}
	if orphan {
		printMQPostMergeOrphan(branchCleanup)
		return nil
	}

	printMQPostMergeResult(result, branchCleanup)
	return nil
}

// resolveMQPostMerge runs the MR-driven cleanup and, when ref resolves to no MR
// bead, cleans the branch it names on its own (gt-qjp2). Only a missing MR bead
// takes the orphan path: every other failure is a refusal to act on a real MR
// and stays one.
func resolveMQPostMerge(mgr mqPostMergeManager, rigPath string, rigGit mqPostMergeGit, ref string, skipBranchDelete bool, landedCommit string) (*refinery.PostMergeResult, mqPostMergeBranchCleanup, bool, error) {
	result, cleanup, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, ref, skipBranchDelete, landedCommit)
	if err == nil {
		return result, cleanup, false, nil
	}
	if !errors.Is(err, refinery.ErrMRNotFound) {
		return nil, mqPostMergeBranchCleanup{}, false, fmt.Errorf("post-merge cleanup: %w", err)
	}

	orphanCleanup, orphanErr := runOrphanMQPostMerge(rigPath, rigGit, ref, skipBranchDelete, landedCommit)
	if orphanErr != nil {
		return nil, mqPostMergeBranchCleanup{}, false, fmt.Errorf("post-merge cleanup: %w; orphaned-branch cleanup for %q: %v", err, ref, orphanErr)
	}
	return nil, orphanCleanup, true, nil
}

func printMQPostMergeResult(result *refinery.PostMergeResult, branchCleanup mqPostMergeBranchCleanup) {
	mr := result.MR
	fmt.Printf("%s Post-merge: %s\n", style.Bold.Render("✓"), mr.ID)
	fmt.Printf("  Branch: %s\n", mr.Branch)
	fmt.Printf("  Worker: %s\n", mr.Worker)

	if result.MRClosed {
		fmt.Printf("  %s MR closed (merged)\n", style.Success.Render("✓"))
	}
	if result.SourceIssueClosed {
		fmt.Printf("  %s Source issue closed: %s\n", style.Success.Render("✓"), result.SourceIssueID)
	} else if result.SourceIssueNotFound {
		fmt.Printf("  %s Source issue: %s %s\n", style.Dim.Render("○"), result.SourceIssueID, style.Dim.Render("(already closed or not found)"))
	}

	if branchCleanup.NoBranch {
		fmt.Printf("  %s No branch name in MR (skipping branch delete)\n", style.Dim.Render("○"))
	} else if branchCleanup.Skipped {
		fmt.Printf("  %s Branch delete skipped (--skip-branch-delete)\n", style.Dim.Render("○"))
	} else if branchCleanup.Disabled {
		fmt.Printf("  %s Branch delete disabled by config\n", style.Dim.Render("○"))
	} else if branchCleanup.OpenPR {
		fmt.Printf("  %s Skipping remote branch delete for %s: open PR exists (gas-fk4)\n", style.Dim.Render("○"), mr.Branch)
	} else if branchCleanup.AlreadyGone {
		fmt.Printf("  %s Remote branch already absent: %s\n", style.Dim.Render("○"), mr.Branch)
	} else if branchCleanup.RemoteDeleted {
		fmt.Printf("  %s Deleted remote branch: %s\n", style.Success.Render("✓"), mr.Branch)
	}

	if branchCleanup.LocalDeleted {
		fmt.Printf("  %s Deleted local branch: %s\n", style.Success.Render("✓"), mr.Branch)
	}
}

func printMQPostMergeOrphan(cleanup mqPostMergeBranchCleanup) {
	fmt.Printf("%s Orphaned branch cleanup: %s\n", style.Bold.Render("✓"), cleanup.Branch)
	fmt.Printf("  %s\n", style.Dim.Render("(no MR bead; branch-only cleanup)"))

	switch {
	case cleanup.Disabled:
		fmt.Printf("  %s Branch delete disabled by config\n", style.Dim.Render("○"))
	case cleanup.AlreadyGone:
		fmt.Printf("  %s Remote branch already absent\n", style.Dim.Render("○"))
	case cleanup.OpenPR:
		fmt.Printf("  %s Skipping remote branch delete for %s: open PR exists (gas-fk4)\n", style.Dim.Render("○"), cleanup.Branch)
	case cleanup.RemoteDeleted:
		fmt.Printf("  %s Deleted remote branch: %s (preserved on %s)\n", style.Success.Render("✓"), cleanup.Branch, cleanup.Target)
	}

	if cleanup.LocalDeleted {
		fmt.Printf("  %s Deleted local branch: %s\n", style.Success.Render("✓"), cleanup.Branch)
	}
}

// runOrphanMQPostMerge deletes a branch whose MR bead is gone, the one case the
// MR-driven path cannot name (gt-qjp2). It adds no new safety reasoning: the
// branch's current remote tip must be preserved on the default branch by the
// same comparison post-merge's merge proof is built on, an open PR still
// protects the branch (gas-fk4), and the delete is a lease delete at the tip
// that was verified.
func runOrphanMQPostMerge(rigPath string, rigGit mqPostMergeGit, branchRef string, skipBranchDelete bool, landedCommit string) (mqPostMergeBranchCleanup, error) {
	branch := normalizeMQBranchName(branchRef)
	cleanup := mqPostMergeBranchCleanup{Branch: branch}
	if branch == "" {
		return cleanup, fmt.Errorf("empty branch name")
	}
	if skipBranchDelete {
		return cleanup, fmt.Errorf("branch %s: --skip-branch-delete leaves nothing to clean without an MR bead", branch)
	}
	if strings.TrimSpace(landedCommit) != "" {
		return cleanup, fmt.Errorf("branch %s: --landed-commit attests to an MR, and there is none", branch)
	}

	defaultBranch := rigGit.RemoteDefaultBranch()
	target := rigGit.CleanDefaultBranchBaseRef("origin", defaultBranch)
	cleanup.Target = target
	// Post-merge's own source/target guard, widened to the branches a merge
	// target is known by: any of them would pass the preservation proof
	// trivially, because the target is preserved on itself.
	for _, protected := range []string{defaultBranch, "main", "master", "HEAD"} {
		if branch == protected {
			return cleanup, fmt.Errorf("branch %s is a merge target, not a work branch; refusing to delete it", branch)
		}
	}
	// Post-merge cleans polecat work branches. Anything else the operator names
	// — an integration branch, a release branch whose tip is an ancestor of the
	// target and so trivially preserving — is out of the command's remit, and
	// the MR path never had to say so because a MR records a work branch.
	if !strings.HasPrefix(branch, constants.BranchPolecatPrefix) {
		return cleanup, fmt.Errorf("post-merge cleans %s branches; %s is not one", constants.BranchPolecatPrefix, branch)
	}

	if !mqDeleteMergedBranchesEnabled(rigPath) {
		cleanup.Disabled = true
		return cleanup, nil
	}

	// The comparison ref has to be current: a stale origin/main refuses a
	// branch whose work did land, which reads as a bug in this command rather
	// than as the safety rail it is.
	if targetRemote := git.RemoteForRef(target); targetRemote != "" {
		if err := rigGit.FetchPrune(targetRemote); err != nil {
			return cleanup, fmt.Errorf("refreshing %s to compare %s against %s: %w", targetRemote, branch, target, err)
		}
	}

	tip, err := rigGit.PushRemoteBranchTip("origin", branch)
	if err != nil {
		return cleanup, fmt.Errorf("read remote tip of %s: %w", branch, err)
	}
	if tip == "" {
		// The MR path reports an absent branch as success because the MR records
		// that the branch existed. Nothing here does: an argument naming neither
		// an MR nor a branch on the remote is a typo, not a cleanup.
		return cleanup, fmt.Errorf("no branch %q on the push remote; nothing to clean", branch)
	}

	// The fetch inside PushRemoteRefTargetStatus is what makes a remote-only
	// branch comparable at all; its hash check is what keeps the tip this
	// command verified and the tip it deletes the same one.
	status, err := rigGit.PushRemoteRefTargetStatus("origin", git.RemoteRef{Name: "refs/heads/" + branch, Hash: tip}, target)
	if err != nil {
		return cleanup, err
	}
	if !status.Preserved {
		return cleanup, fmt.Errorf("branch %s at %s is not preserved on %s (%d patch-unique commits); refusing to delete it",
			branch, tip, target, status.UnpreservedPatchCount)
	}

	cleaned, err := cleanupMQPostMergeBranch(rigPath, rigGit, &refinery.MergeRequest{Branch: branch, CommitSHA: tip}, false)
	cleaned.Target = target
	return cleaned, err
}

// normalizeMQBranchName reduces an operator-supplied ref to the branch name
// post-merge deletes branches by.
func normalizeMQBranchName(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/remotes/")
	if remote := git.RemoteForRef(ref); remote != "" {
		ref = strings.TrimPrefix(ref, remote+"/")
	}
	return strings.TrimSpace(ref)
}

func runVerifiedMQPostMerge(mgr mqPostMergeManager, rigPath string, rigGit mqPostMergeGit, mrID string, skipBranchDelete bool, landedCommit string) (*refinery.PostMergeResult, mqPostMergeBranchCleanup, error) {
	mr, err := mgr.FindMRForPostMerge(mrID)
	if err != nil {
		return nil, mqPostMergeBranchCleanup{}, err
	}
	mergeCommit, err := verifyMQPostMergeProof(rigGit, mr, landedCommit)
	if err != nil {
		return nil, mqPostMergeBranchCleanup{}, err
	}
	// Recorded here rather than inside the verifier: the proof itself must stay
	// a pure read of the repository, and only the caller that actually goes on
	// to close the MR should mutate the snapshot it hands to PostMergeMR
	// (gt-mlla).
	if mergeCommit != "" {
		mr.MergeCommit = mergeCommit
	}

	result, err := mgr.PostMergeMR(mr)
	if err != nil {
		return result, mqPostMergeBranchCleanup{}, err
	}

	branchCleanup, err := cleanupMQPostMergeBranch(rigPath, rigGit, result.MR, skipBranchDelete)
	return result, branchCleanup, err
}

// verifyMQPostMergeProof proves that mr's work actually landed on its target
// branch. The default proof requires the submitted commit_sha to be reachable
// from target — exact tip, ancestor, or content-preserved (same patch-id) —
// which holds for a plain rebase-merge. It cannot hold when the merge queue's
// sequential-rebase protocol needed to resolve a real conflict, because
// resolving the conflict legitimately changes the patch-id of the affected
// commit even though the work correctly landed. landedCommit is an explicit
// attestation for that case: the caller (the refinery formula that performed
// the rebase) names the SHA it actually pushed to target. It still must be
// reachable from target — a wrong or stale attestation is rejected — and it
// must be bound to the MR it claims to satisfy rather than being compared
// against the submitted commit_sha, whose patch-id a conflict resolution
// legitimately changes (see verifyLandedCommitMatchesSubmitted).
//
// The return value is the commit that should be recorded as the MR's
// merge_commit, or "" when the proof does not override it. It is returned
// rather than assigned onto mr so this stays a read-only check: mutating the
// caller's snapshot as a side effect of verifying it made the proof both
// harder to test and impossible to call twice (gt-mlla).
func verifyMQPostMergeProof(rigGit mqPostMergeGit, mr *refinery.MergeRequest, landedCommit string) (string, error) {
	if mr == nil {
		return "", fmt.Errorf("merge proof failed: merge request is missing")
	}
	target := strings.TrimSpace(mr.TargetBranch)
	if target == "" {
		return "", fmt.Errorf("merge proof failed for MR %s: missing target branch", mr.ID)
	}
	if source := strings.TrimSpace(mr.Branch); source != "" && source == target {
		return "", fmt.Errorf("merge proof failed for MR %s: source branch %s matches target branch", mr.ID, source)
	}
	commit := strings.TrimSpace(mr.CommitSHA)
	if commit == "" {
		return "", fmt.Errorf("merge proof failed for MR %s: missing submitted commit_sha", mr.ID)
	}
	if landedCommit = strings.TrimSpace(landedCommit); landedCommit != "" {
		if err := rigGit.VerifyPushedCommitReachableFromPushTarget("origin", target, landedCommit); err != nil {
			return "", fmt.Errorf("merge proof failed for MR %s: attested landed commit %s not reachable from %s: %w", mr.ID, landedCommit, target, err)
		}
		// Normalize before anything compares or persists it. The attestation is
		// typed by hand at the moment a conflict has just been resolved, which is
		// exactly when an abbreviated SHA is easiest to paste and hardest to
		// double-check — and an abbreviated MergeCommit on the closed MR bead
		// cannot be resolved back to a commit once the branch is deleted.
		resolvedLanded := resolveMQPostMergeCommit(rigGit, landedCommit)
		submittedCommit := liveMQPostMergeBranchHead(rigGit, mr.Branch, commit)
		recorded, err := verifyLandedCommitMatchesSubmitted(rigGit, target, submittedCommit, resolvedLanded)
		if err != nil {
			return "", fmt.Errorf("merge proof failed for MR %s: attestation does not match MR: %w", mr.ID, err)
		}
		return recorded, nil
	}
	if err := rigGit.VerifyPushedCommitReachableFromPushTarget("origin", target, commit); err != nil {
		return "", fmt.Errorf("merge proof failed for MR %s: target %s does not contain submitted head %s: %w", mr.ID, target, commit, err)
	}
	return "", nil
}

// resolveMQPostMergeCommit normalizes an attested landed commit to its full
// SHA, so an abbreviated attestation (gt mq post-merge --landed-commit 2bb0bf7)
// is not persisted verbatim as the MR's merge_commit (gt-mlla). Resolution is
// best-effort: the attestation has already been proven reachable from target by
// the caller, so a repository that cannot resolve the shorthand is a cosmetic
// miss, not a reason to reject a merge whose work did land.
func resolveMQPostMergeCommit(rigGit mqPostMergeGit, commit string) string {
	resolved, err := rigGit.Rev(commit + "^{commit}")
	if err != nil || strings.TrimSpace(resolved) == "" {
		return commit
	}
	return strings.TrimSpace(resolved)
}

// liveMQPostMergeBranchHead prefers the source branch's live remote tip over
// the commit_sha recorded on the MR bead. A conflict-resolution push
// legitimately advances the source branch after submission (the queue's
// sequential-rebase protocol requires it), but nothing updates the MR bead's
// commit_sha when that happens, so it goes stale (gt-lk6g). Binding the
// --landed-commit attestation to the stale value can compare it against
// content that no longer reflects the branch's real, currently-landed work —
// in the reported incident the stale commit had already become an ancestor
// of target by the time post-merge ran, so its own diff was empty and the
// binding check rejected a merge that had, in fact, correctly landed.
// Falling back to commit_sha when the branch is already gone (a retry after
// the branch was already deleted) keeps that case working exactly as before.
func liveMQPostMergeBranchHead(rigGit mqPostMergeGit, branch, commit string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return commit
	}
	tip, err := rigGit.PushRemoteBranchTip("origin", branch)
	if err != nil || strings.TrimSpace(tip) == "" {
		return commit
	}
	return strings.TrimSpace(tip)
}

// verifyLandedCommitMatchesSubmitted binds a --landed-commit attestation to
// the MR it claims to satisfy, so an attester (or a bug) cannot substitute
// any other commit that merely happens to already be reachable from target.
//
// Reachability alone proves nothing about content: essentially every commit
// in target's history is "reachable from target". The refinery's sequential
// rebase protocol squash-merges each MR (git.MergeSquash), so landedCommit is
// a single commit whose sole parent is target's tip at merge time — its own
// diff against that parent is exactly the change-set that landed. This
// requires every file the submitted branch touched (relative to where it
// diverged from target) to also appear changed in landedCommit's diff.
//
// This is a file-level binding, not a byte-exact one: a conflict-resolved
// rebase legitimately changes hunks within those files (that's the entire
// reason --landed-commit exists — see verifyMQPostMergeProof), so exact
// patch-id equality would reject every real conflict resolution along with
// the forgeries. Requiring the submitted file set to survive into the landed
// diff still rejects an unrelated on-target commit, whose changed files are
// essentially never a superset of the submitted branch's.
//
// It returns the commit to record as the MR's merge_commit. That is normally
// landedCommit, but when the submitted head is already reachable from target
// there is no diverged range to bind against and the submitted head is the
// commit that demonstrably landed — recording the attestation instead would
// persist a caller-supplied value the binding never checked.
func verifyLandedCommitMatchesSubmitted(rigGit mqPostMergeGit, target, submittedCommit, landedCommit string) (string, error) {
	submittedBase, err := rigGit.MergeBase(target, submittedCommit)
	if err != nil {
		return "", fmt.Errorf("merge-base of %s and submitted head %s: %w", target, submittedCommit, err)
	}
	if strings.TrimSpace(submittedBase) == strings.TrimSpace(submittedCommit) {
		// The submitted head is already an ancestor of target, so the MR's work
		// landed with its SHA intact: a fast-forward target, or a retry after an
		// earlier cycle already fast-forwarded it. There is nothing left for the
		// attestation to disambiguate — the default proof would have accepted
		// this MR with no flag at all — so the flag is redundant, not wrong.
		// Rejecting the empty diff here turned a correct merge into a permanent
		// post-merge failure that no retry could clear (gt-mlla).
		return submittedCommit, nil
	}
	submittedFiles, err := rigGit.DiffNameOnly(submittedBase, submittedCommit)
	if err != nil {
		return "", fmt.Errorf("changed files of submitted head %s: %w", submittedCommit, err)
	}
	if len(submittedFiles) == 0 {
		return "", fmt.Errorf("submitted head %s has no changed files to bind the attestation to", submittedCommit)
	}
	landedParent, err := rigGit.Rev(landedCommit + "^")
	if err != nil {
		return "", fmt.Errorf("resolve parent of attested commit %s: %w", landedCommit, err)
	}
	landedFiles, err := rigGit.DiffNameOnly(landedParent, landedCommit)
	if err != nil {
		return "", fmt.Errorf("changed files of attested commit %s: %w", landedCommit, err)
	}
	landedSet := make(map[string]bool, len(landedFiles))
	for _, f := range landedFiles {
		landedSet[f] = true
	}
	for _, f := range submittedFiles {
		if !landedSet[f] {
			return "", fmt.Errorf("submitted file %q not present among attested commit %s's changed files", f, landedCommit)
		}
	}
	return landedCommit, nil
}

func cleanupMQPostMergeBranch(rigPath string, rigGit mqPostMergeGit, mr *refinery.MergeRequest, skipBranchDelete bool) (mqPostMergeBranchCleanup, error) {
	cleanup := mqPostMergeBranchCleanup{}
	if mr == nil {
		return cleanup, fmt.Errorf("remote branch delete: merge request is missing")
	}

	cleanup.Branch = strings.TrimSpace(mr.Branch)
	if cleanup.Branch == "" {
		cleanup.NoBranch = true
		return cleanup, nil
	}
	if skipBranchDelete {
		cleanup.Skipped = true
		return cleanup, nil
	}
	if !mqDeleteMergedBranchesEnabled(rigPath) {
		cleanup.Disabled = true
		return cleanup, nil
	}

	expectedHead := strings.TrimSpace(mr.CommitSHA)
	if expectedHead == "" {
		return cleanup, fmt.Errorf("remote branch delete %s: missing submitted commit_sha", cleanup.Branch)
	}
	deleteAt := expectedHead

	// Deleting a branch with an open PR causes GitHub to auto-close the PR as
	// "closed" (not "merged"), destroying the PR audit trail. (gas-fk4)
	if rigGit.HasOpenPullRequest(git.PullRequestRef{URL: mr.PRURL, Number: mr.PRNumber, Branch: cleanup.Branch, HeadSHA: expectedHead}) {
		cleanup.OpenPR = true
	} else {
		remoteTip, err := rigGit.PushRemoteBranchTip("origin", cleanup.Branch)
		if err != nil {
			return cleanup, fmt.Errorf("remote branch delete %s: read remote branch tip: %w", cleanup.Branch, err)
		}
		remoteTip = strings.TrimSpace(remoteTip)
		if remoteTip == "" {
			cleanup.AlreadyGone = true
		} else {
			if remoteTip != expectedHead {
				preservedAt, err := resolveMovedMQPostMergeBranchTip(rigGit, mr, cleanup.Branch, expectedHead, remoteTip)
				if err != nil {
					return cleanup, err
				}
				deleteAt = preservedAt
			}
			if err := rigGit.DeleteRemoteBranchIfAt("origin", cleanup.Branch, deleteAt); err != nil {
				return cleanup, fmt.Errorf("remote branch delete %s at %s: %w", cleanup.Branch, deleteAt, err)
			}
			cleanup.RemoteDeleted = true
		}
	}

	if deleteMQPostMergeLocalBranchIfAt(rigGit, cleanup.Branch, deleteAt) {
		cleanup.LocalDeleted = true
	}
	return cleanup, nil
}

// resolveMovedMQPostMergeBranchTip authorizes a CAS branch-delete to pin to
// the branch's live remote tip instead of the submitted commit_sha recorded
// on the MR bead, for the same reason liveMQPostMergeBranchHead adopts the
// live tip for attestation binding: a conflict-resolution push legitimately
// advances the source branch after submission without ever updating the MR
// bead's commit_sha, so the delete's expected value (git push
// --force-with-lease=<ref>:<expectedHash>) is pinned to a hash the remote no
// longer has — rejected as "stale info" even though the branch's current
// content already landed (gt-lk6g).
//
// The live tip is trusted only once every commit it carries beyond
// expectedHead is proven preserved on the merge target, the same
// content-preservation proof the orphan-branch cleanup path (gt-qjp2) uses to
// authorize deleting a branch with no MR bead at all: a branch that moved for
// any other reason is still refused rather than deleted out from under it.
func resolveMovedMQPostMergeBranchTip(rigGit mqPostMergeGit, mr *refinery.MergeRequest, branch, expectedHead, remoteTip string) (string, error) {
	defaultBranch := rigGit.RemoteDefaultBranch()
	target := rigGit.CleanBaseRef("origin", defaultBranch, mr.TargetBranch)
	if targetRemote := git.RemoteForRef(target); targetRemote != "" {
		if err := rigGit.FetchPrune(targetRemote); err != nil {
			return "", fmt.Errorf("remote branch delete %s: refreshing %s to compare against %s: %w", branch, targetRemote, target, err)
		}
	}
	status, err := rigGit.PushRemoteRefTargetStatus("origin", git.RemoteRef{Name: "refs/heads/" + branch, Hash: remoteTip}, target)
	if err != nil {
		return "", fmt.Errorf("remote branch delete %s: checking live tip %s against %s: %w", branch, remoteTip, target, err)
	}
	if !status.Preserved {
		return "", fmt.Errorf("remote branch delete %s: live tip %s has moved past submitted head %s and is not preserved on %s (%d patch-unique commits); refusing to delete it",
			branch, remoteTip, expectedHead, target, status.UnpreservedPatchCount)
	}
	return remoteTip, nil
}

func deleteMQPostMergeLocalBranchIfAt(rigGit mqPostMergeGit, branch, expectedHead string) bool {
	localHead, err := rigGit.Rev("refs/heads/" + branch + "^{commit}")
	if err != nil || strings.TrimSpace(localHead) != strings.TrimSpace(expectedHead) {
		return false
	}
	return rigGit.DeleteBranch(branch, false) == nil
}

func mqDeleteMergedBranchesEnabled(rigPath string) bool {
	mq := rig.ResolveMergeQueueConfig(filepath.Dir(rigPath), filepath.Base(rigPath))
	if mq == nil {
		return true
	}
	return mq.IsDeleteMergedBranchesEnabled()
}
