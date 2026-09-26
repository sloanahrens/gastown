package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
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
	mqRejectFindings  string
	mqRejectAttempt   int    // Attempt this rejection IS, when the caller numbers them
	mqRejectFailure   string // Failure class of this rejection; empty records none

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
var mqPostMergeNoCycle bool

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
stale SHA is rejected) and bound to the submitted MR — by the commits ending
at the SHA it names, matched by patch-id, or by the files the submitted
branch touched appearing in its own diff — so an unrelated on-target commit
is rejected too.

An MR bead that records no commit_sha — the shape of one created directly
rather than by 'gt mq submit' or 'gt done', both of which write the field —
is recovered by reading the head off its recorded branch and proving it
against the target exactly like a submitted head, and the output says the
head was inferred. The recovered head is recorded on the MR bead when it
closes (commit_sha plus a commit_sha_inferred marker) so the record
distinguishes evidence recovered at close time from a head recorded at
submission. When the branch is already gone there is nothing left to
recover from, and the command refuses with the recovery step named.

After cleanup it does the per-MR chores (MERGED to the witness, MERGE_READY
archive, temp branch delete) and, on a rig with
merge_queue.cycle_session_after_merge on, may close the refinery's patrol
cycle and respawn its session. --no-cycle keeps the chores but never cycles:
batch recovery passes it so the session survives to recover every member.

Examples:
  gt mq post-merge gastown gt-mr-abc123
  gt mq post-merge gastown gt-mr-abc123 --skip-branch-delete
  gt mq post-merge gastown gt-mr-abc123 --skip-branch-delete --no-cycle
  gt mq post-merge gastown gt-mr-abc123 --landed-commit 2bb0bf7f
  gt mq post-merge gastown polecat/Nux/gt-xyz    # orphaned branch, MR bead gone`,
	Args: cobra.ExactArgs(2),
	// Every failure here is an operational refusal — a proof that did not hold,
	// a moved branch whose work is not on the target — and the usage block cobra
	// appends to it buries the one line the operator needs (gt-mkut).
	SilenceUsage: true,
	RunE:         runMQPostMerge,
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
	PullRequestProtection(ref git.PullRequestRef) (git.PRProtection, error)
	Rev(ref string) (string, error)
	MergeBase(a, b string) (string, error)
	DiffNameOnly(base, head string) ([]string, error)
	PatchID(base, head string) (string, error)
	PatchIDs(base, head string) ([]string, error)
	DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error
	DeleteBranch(branch string, force bool) error
}

type mqPostMergeBranchCleanup struct {
	Branch string
	Target string // orphan cleanup: ref the branch's work had to be preserved on

	// SubmittedHead is the head the merge proof bound against, and
	// SubmittedHeadInferred reports that it was read from the recorded branch
	// rather than submitted on the MR bead. Report-only: the output has to say
	// which evidence closed the merge (gt-6o1u).
	SubmittedHead         string
	SubmittedHeadInferred bool

	NoBranch      bool
	Skipped       bool
	Disabled      bool
	OpenPR        bool
	AlreadyGone   bool
	RemoteDeleted bool
	LocalDeleted  bool

	// PRUnknown reports that the open-PR guard's lookup itself failed or was
	// ambiguous rather than finding an open PR (gt-ghpk): the branch is still
	// left in place, but the operator needs to hear that the PR state
	// couldn't be determined, not that a PR exists. PRLookupErr is the
	// lookup failure.
	PRUnknown   bool
	PRLookupErr error

	// LeftForOwner reports a branch the refinery may not push to (anything
	// but polecat/* and integration/*), so its remote delete was never
	// attempted: the branch belongs to its owner (gt-qyf1t).
	LeftForOwner bool

	// Err is a branch-cleanup failure that happened after the merge landed
	// and the MR closed. It is reported and escalated, never a reason to skip
	// the rest of post-merge (gt-qyf1t).
	Err error
}

// mqPostMergeRemoteBranchDeletable reports whether post-merge may delete
// branch on the remote. The refinery clone's .githooks/pre-push allows the
// refinery to push — deletes included — only main, beads-sync, polecat/* and
// integration/*; of those only the work-branch prefixes are ever cleaned up.
// Every other branch (crew/*, a contributor's feature branch) belongs to its
// owner, and a delete attempt would only fail at the hook (gt-qyf1t).
func mqPostMergeRemoteBranchDeletable(branch string) bool {
	for _, prefix := range []string{constants.BranchPolecatPrefix, constants.BranchIntegrationPrefix} {
		if strings.HasPrefix(branch, prefix) && len(branch) > len(prefix) {
			return true
		}
	}
	return false
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
	mqRejectCmd.Flags().StringVar(&mqRejectFindings, "findings-json", "", "Path to a 'gt mq review --json' result (or - for stdin): its findings are recorded on the source bead's notes in the format the next attempt parses, even with --no-recover")
	mqRejectCmd.Flags().IntVar(&mqRejectAttempt, "attempt", 0, "Attempt number this rejection is, when the caller numbers them: the recorded note names it, so it matches the attempt in --reason")
	mqRejectCmd.Flags().StringVar(&mqRejectFailure, "failure-type", "", "What this rejection was actually about (tests|build|lint|typecheck|editorial): recorded in the source bead's note and the RECOVERED_BEAD mail, which carry no class at all when this is unset. Defaulted to editorial only by --findings-json, whose input is an om verdict")

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
	mqPostMergeCmd.Flags().BoolVar(&mqPostMergeNoCycle, "no-cycle", false, "Do the per-MR chores but never close the patrol cycle or respawn the refinery session (batch recovery)")
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

// readRejectFindings parses the om verdict named by --findings-json into the
// record a rejection carries onto the source bead. The file is a
// `gt mq review --json` result; a verdict that produced no note (an infra
// failure) contributes no findings, and an empty list is not an error — the
// rejection is still recorded, just with no finding lines.
func readRejectFindings(path string) (refinery.RejectionRecord, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return refinery.RejectionRecord{}, fmt.Errorf("reading findings: %w", err)
	}
	var result editorial.ReviewResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return refinery.RejectionRecord{}, fmt.Errorf("parsing findings %s (expected a 'gt mq review --json' result): %w", path, err)
	}
	if result.Note == nil {
		return refinery.RejectionRecord{}, nil
	}
	// The receipt rides along with the findings so a rejection made through
	// this flag feeds the deacon's convergence rule the same verdict the
	// batch path's automatic review does — without it, `gt deacon redispatch`
	// falls back to plain attempt-count redispatch for the formula path.
	return refinery.RejectionRecord{
		Findings: refinery.RejectionFindingsFromNote(result.Note.Findings),
		Receipt: &refinery.EditorialReceipt{
			Score:      result.Note.Score,
			Unresolved: result.Note.PriorFindings.Unresolved,
		},
	}, nil
}

// rejectRecord resolves the record a rejection carries from the two flags that
// describe one: --findings-json, an om verdict whose findings and receipt the
// next attempt reads back, and --failure-type, what actually broke. Both
// refusals — an unreadable findings file, a class outside the vocabulary —
// happen here, before anything is rejected, and are split out from runMQReject
// so they are testable without a rig, the way rejectReason is.
//
// --findings-json classifies its own rejection as editorial, its input being
// an om review verdict. --failure-type overrides that, for the caller who read
// the verdict and knows the branch's real defect was something the verdict
// only described in prose (gt-1jig).
func rejectRecord(findingsPath, failureType string, attempt int) (refinery.RejectionRecord, error) {
	var rec refinery.RejectionRecord
	if findingsPath != "" {
		var err error
		rec, err = readRejectFindings(findingsPath)
		if err != nil {
			return refinery.RejectionRecord{}, err
		}
		// A result carrying no note is an infra failure, not a verdict — it
		// says nothing about the branch, so it classifies nothing either.
		if rec.Receipt != nil || len(rec.Findings) > 0 {
			rec.FailureType = refinery.FailureTypeEditorial
		}
	}
	if failureType != "" {
		class, err := refinery.ClassifyRejectionFailureType(failureType)
		if err != nil {
			return refinery.RejectionRecord{}, fmt.Errorf("--failure-type: %w", err)
		}
		rec.FailureType = class
	}
	rec.Attempt = attempt
	return rec, nil
}

// rejectReason resolves the reject reason and rejects the flag combinations
// that would read it from two places at once. It runs before anything is
// rejected — and is split out from runMQReject so both refusals are testable
// without a rig.
func rejectReason(stdin bool, reason, findingsPath string, stdinReader io.Reader) (string, error) {
	// Handle --stdin: read reason from stdin (avoids shell quoting issues)
	if stdin {
		if reason != "" {
			return "", fmt.Errorf("cannot use --stdin with --reason/-r")
		}
		if findingsPath == "-" {
			return "", fmt.Errorf("cannot read both --stdin and --findings-json - from stdin")
		}
		data, err := io.ReadAll(stdinReader)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		reason = strings.TrimRight(string(data), "\n")
	}

	// Require reason via --reason or --stdin
	if reason == "" {
		return "", fmt.Errorf("required flag \"reason\" not set (use --reason/-r or --stdin)")
	}
	return reason, nil
}

func printRejectionSummary(result *refinery.MergeRequest, reason string, notify bool) {
	fmt.Printf("%s Rejected: %s\n", style.Bold.Render("✗"), result.Branch)
	fmt.Printf("  Worker: %s\n", result.Worker)
	fmt.Printf("  Reason: %s\n", reason)

	if result.IssueID != "" {
		statusNote := "status unknown"
		if result.SourceIssueStatus != "" {
			statusNote = "status: " + result.SourceIssueStatus
		}
		fmt.Printf("  Issue:  %s %s\n", result.IssueID, style.Dim.Render("("+statusNote+")"))
	}

	if notify {
		fmt.Printf("  %s\n", style.Dim.Render("Worker notified via mail"))
	}
}

func runMQReject(cmd *cobra.Command, args []string) error {
	reason, err := rejectReason(mqRejectStdin, mqRejectReason, mqRejectFindings, os.Stdin)
	if err != nil {
		return err
	}
	mqRejectReason = reason

	// Resolve the record before rejecting anything: a missing or malformed
	// findings file, or a failure class outside the vocabulary, must fail
	// while nothing has changed yet, not after the MR is closed and recovery
	// has run (gt-s4f6, gt-1jig).
	rec, err := rejectRecord(mqRejectFindings, mqRejectFailure, mqRejectAttempt)
	if err != nil {
		return err
	}

	rigName := args[0]
	mrIDOrBranch := args[1]

	mgr, _, _, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}

	// The verdict's findings, and the failure class, go onto the source bead's
	// notes as the lines the next attempt reads back. RejectMRRecording does
	// that whatever --no-recover says: a caller that redispatches the bead
	// itself leaves the record, and one that lets recovery run gets the
	// identical note from there (gt-s4f6).
	//
	// Either flag means this call has a record to carry, so either one routes
	// through RejectMRRecording — the route that reaches the note and the
	// RECOVERED_BEAD mail. A rejection whose note someone else writes passes
	// neither, so one rejection keeps exactly one writer of its note.
	var result *refinery.MergeRequest
	if mqRejectFindings != "" || mqRejectFailure != "" {
		result, err = mgr.RejectMRRecording(mrIDOrBranch, mqRejectReason, mqRejectNotify, mqRejectNoRecover, rec)
	} else {
		result, err = mgr.RejectMR(mrIDOrBranch, mqRejectReason, mqRejectNotify, mqRejectNoRecover)
	}

	// Printed whenever the MR was rejected, error or not: a rejection whose
	// durable record failed still rejected the MR, and the caller needs to
	// see both halves.
	if result != nil {
		printRejectionSummary(result, mqRejectReason, mqRejectNotify)
	}
	if err != nil {
		if result != nil {
			return fmt.Errorf("MR rejected, but its findings were not recorded on the source bead: %w", err)
		}
		return fmt.Errorf("rejecting MR: %w", err)
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

	townRoot := filepath.Dir(r.Path)
	mqCfg := rig.ResolveMergeQueueConfig(townRoot, r.Name)
	return runMQPostMergeWith(r.Name, mqPostMergeRunDeps{
		Resolve: func() (*refinery.PostMergeResult, mqPostMergeBranchCleanup, bool, error) {
			return resolveMQPostMerge(mgr, r.Path, rigGit, ref, mqPostMergeSkipBranchDelete, mqPostMergeLandedCommit)
		},
		RubricCheck: func(result *refinery.PostMergeResult, cleanup mqPostMergeBranchCleanup) {
			handlePostMergeRubricChange(r.Path, r.Name, rigGit, result.MR, cleanup.SubmittedHead)
		},
		PostMergeCommand: func(mr *refinery.MergeRequest) {
			runMRPostMergeCommand(townRoot, r.Name, r.Path, mqCfg, mr, os.Stdout)
		},
		UnitCycle: func(result *refinery.PostMergeResult) unitCycleReport {
			sess := refinerySessionFor(r.Name)
			workDir := refineryWorkDir(r.Path)
			return completeUnitAndCycle(unitCycleParams{
				Rig:             r.Name,
				Mode:            unitSingle,
				RefinerySession: sess,
				WorkDir:         workDir,
				MRs: []unitMR{{
					ID: result.MR.ID, Branch: result.MR.Branch, Worker: result.MR.Worker,
					SourceIssue: result.SourceIssueID, Target: result.MR.TargetBranch,
				}},
				MergeCommit:          resolvePostMergeSHA(workDir, result.MR.MergeCommit, result.MR.TargetBranch),
				LandedCommitAttested: mqPostMergeLandedCommit != "",
				CycleEnabled:         mqCfg != nil && mqCfg.CycleSessionAfterMerge,
				CycleSuppressed:      mqPostMergeNoCycle,
			}, defaultUnitCycleDeps(townRoot, r.BeadsPath(), workDir, sess, os.Stdout))
		},
		Escalate: func(fingerprint, severity, msg string) {
			runBoundedEscalate("post-merge-branch-cleanup", "refinery:post-merge", fingerprint, severity, msg)
		},
		Out: os.Stdout,
	})
}

// mqPostMergeRunDeps holds every side effect runMQPostMergeWith has, so tests
// drive the whole post-merge sequence without a rig, beads, git remotes,
// tmux or mail.
type mqPostMergeRunDeps struct {
	Resolve          func() (*refinery.PostMergeResult, mqPostMergeBranchCleanup, bool, error)
	RubricCheck      func(result *refinery.PostMergeResult, cleanup mqPostMergeBranchCleanup)
	PostMergeCommand func(mr *refinery.MergeRequest)
	UnitCycle        func(result *refinery.PostMergeResult) unitCycleReport
	Escalate         func(fingerprint, severity, msg string)
	Out              io.Writer
}

// runMQPostMergeWith is gt mq post-merge after its rig and git are resolved.
// An error from Resolve means the landing was not confirmed (no MR, merge not
// proven, MR close refused) and stops here: nothing after it may run for a
// merge that is not known to have landed. Once it has landed, everything runs
// — a branch-cleanup failure is reported ✗ and escalated, but the post-merge
// command and the unit cycle still run and the command exits 0, because a
// non-zero exit makes the refinery retry post-merge and duplicate its chores
// (gt-qyf1t).
func runMQPostMergeWith(rigName string, d mqPostMergeRunDeps) error {
	result, branchCleanup, orphan, err := d.Resolve()
	if err != nil {
		return err
	}
	if orphan {
		printMQPostMergeOrphan(d.Out, branchCleanup)
		return nil
	}

	d.RubricCheck(result, branchCleanup)

	printMQPostMergeResult(d.Out, result, branchCleanup)
	if branchCleanup.Err != nil {
		d.Escalate("post-merge-branch-cleanup:"+rigName, "medium",
			fmt.Sprintf("post-merge on %s: %s (branch %s) landed and closed, but its remote branch cleanup failed: %v; delete the branch by hand if it should go",
				rigName, result.MR.ID, result.MR.Branch, branchCleanup.Err))
	}

	d.PostMergeCommand(result.MR)

	// Last: the respawn it may do replaces this process, so everything the
	// post-merge command owes its caller (closes, ✓ lines, the install hook)
	// has already happened.
	rep := d.UnitCycle(result)
	if rep.SkipCause != "" {
		fmt.Fprintf(d.Out, "  %s session kept: %s\n", style.Dim.Render("○"), rep.SkipCause)
	}
	return nil
}

// detectRubricChangeAfterMerge reports whether the merge that produced head
// touched the rig's deployed rubric (.om.json), and the sha256 of what
// landed there. It never writes to the manifest: re-stamping stays a
// deliberate operator step; the caller escalates instead via
// escalateRubricChange (gt-7bvf).
//
// touched=false, nil means a rig with no manifest deployed or no rubric
// configured, or a merge that didn't touch it. touched=true with a non-nil
// err means this could not fully check the change (an unresolvable rubric
// path, an unreadable landed blob) and fails closed rather than being read
// as untouched. touched=false with a non-nil err means the landed range
// itself could not be reconstructed (ResolveLandedRange refuses a
// non-mergeable landing) — the caller surfaces that as a warning, never a
// reason to fail a merge that already landed and closed.
func detectRubricChangeAfterMerge(rigDir string, rigGit *git.Git, mr *refinery.MergeRequest, head string) (touched bool, rel, sha string, err error) {
	if mr == nil {
		return false, "", "", nil
	}
	manifest, err := editorial.LoadManifest(rigDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", "", nil
		}
		return false, "", "", fmt.Errorf("loading harness manifest: %w", err)
	}
	landedArg := strings.TrimSpace(mr.MergeCommit)
	if landedArg == "" {
		landedArg = strings.TrimSpace(head)
	}
	// repoRootDir only anchors the rigRoot math rubricRelPath does for an
	// absolute rubric path (two directories up from repoDir); it need not
	// exist on disk, and either clone yields the same rigRoot.
	repoRootDir := filepath.Join(rigDir, "refinery", "rig")
	return editorial.RubricChangeAfterMerge(rigGit, repoRootDir, manifest, landedArg, mr.TargetBranch)
}

// handlePostMergeRubricChange runs detectRubricChangeAfterMerge and, when it
// reports touched, escalates to the operator and records
// rubric_changed_escalated on the MR bead. touched can be true alongside a
// detection error (an unresolvable rubric path fails closed rather than
// being read as "untouched"), so escalation runs whenever touched is true,
// not only on a clean detection (gt-7bvf).
func handlePostMergeRubricChange(rigPath, rigName string, rigGit *git.Git, mr *refinery.MergeRequest, submittedHead string) {
	touched, rel, sha, checkErr := detectRubricChangeAfterMerge(rigPath, rigGit, mr, submittedHead)
	if checkErr != nil {
		style.PrintWarning("rubric change check: %v", checkErr)
	}
	if !touched {
		return
	}
	fmt.Printf("  %s Rubric changed by this merge; escalated to the operator (the manifest is not re-stamped automatically)\n", style.Warning.Render("⚠"))
	escalateRubricChange(rigName, mr.TargetBranch, rel, sha, mr.ID)
	if commentErr := beads.New(rigPath).AddComment(mr.ID, fmt.Sprintf("rubric_changed_escalated: %s sha256=%s", rel, sha)); commentErr != nil {
		style.PrintWarning("could not record rubric_changed_escalated comment on %s: %v", mr.ID, commentErr)
	}
}

// escalateRubricChange notifies the operator that a merge changed the rig's
// deployed rubric, without touching the harness manifest. Best-effort: a
// failed escalation is logged, not fatal — the merge already landed.
func escalateRubricChange(rigName, target, rubricPath, sha, mrID string) {
	msg := fmt.Sprintf("rubric changed on %s: re-stamp the harness manifest from main content (rig=%s rubric=%s sha256=%s MR=%s)", target, rigName, rubricPath, sha, mrID)
	cmd := exec.Command("gt", "escalate", "--severity", "medium", "--reason", "rubric-changed", msg)
	if err := cmd.Run(); err != nil {
		style.PrintWarning("rubric-change escalation failed: %v", err)
	}
}

// resolveMQPostMerge runs the MR-driven cleanup and, when ref resolves to no MR
// bead, cleans the branch it names on its own (gt-qjp2). Only a missing MR bead
// takes the orphan path: every other failure is a refusal to act on a real MR
// and stays one — except a branch-cleanup failure after the MR closed, which
// is returned on cleanup.Err with a nil error (gt-qyf1t).
func resolveMQPostMerge(mgr mqPostMergeManager, rigPath string, rigGit mqPostMergeGit, ref string, skipBranchDelete bool, landedCommit string) (*refinery.PostMergeResult, mqPostMergeBranchCleanup, bool, error) {
	result, cleanup, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, ref, skipBranchDelete, landedCommit)
	if err == nil {
		return result, cleanup, false, nil
	}
	// The merge landed and the MR closed; only its branch cleanup failed.
	// That rides on the cleanup for the caller to report and escalate, so the
	// post-merge command and the unit cycle still run (gt-qyf1t).
	var cleanupErr *mqPostMergeBranchCleanupError
	if errors.As(err, &cleanupErr) && result != nil && result.MR != nil {
		cleanup.Err = cleanupErr.err
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

func printMQPostMergeResult(w io.Writer, result *refinery.PostMergeResult, branchCleanup mqPostMergeBranchCleanup) {
	mr := result.MR
	fmt.Fprintf(w, "%s Post-merge: %s\n", style.Bold.Render("✓"), mr.ID)
	fmt.Fprintf(w, "  Branch: %s\n", mr.Branch)
	fmt.Fprintf(w, "  Worker: %s\n", mr.Worker)
	if branchCleanup.SubmittedHeadInferred {
		fmt.Fprintf(w, "  %s Submitted head %s inferred from the branch (the MR bead records no commit_sha); proven against %s like a submitted head\n",
			style.Dim.Render("○"), branchCleanup.SubmittedHead, mr.TargetBranch)
	}

	if result.MRClosed {
		fmt.Fprintf(w, "  %s MR closed (merged)\n", style.Success.Render("✓"))
	}
	if result.SourceIssueClosed {
		fmt.Fprintf(w, "  %s Source issue closed: %s\n", style.Success.Render("✓"), result.SourceIssueID)
	} else if result.SourceIssueNotFound {
		fmt.Fprintf(w, "  %s Source issue: %s %s\n", style.Dim.Render("○"), result.SourceIssueID, style.Dim.Render("(already closed or not found)"))
	}

	if branchCleanup.NoBranch {
		fmt.Fprintf(w, "  %s No branch name in MR (skipping branch delete)\n", style.Dim.Render("○"))
	} else if branchCleanup.Skipped {
		fmt.Fprintf(w, "  %s Branch delete skipped (--skip-branch-delete)\n", style.Dim.Render("○"))
	} else if branchCleanup.Disabled {
		fmt.Fprintf(w, "  %s Branch delete disabled by config\n", style.Dim.Render("○"))
	} else if branchCleanup.OpenPR {
		fmt.Fprintf(w, "  %s Skipping remote branch delete for %s: open PR exists (gas-fk4)\n", style.Dim.Render("○"), mr.Branch)
	} else if branchCleanup.PRUnknown {
		fmt.Fprintf(w, "  %s PR state unknown for %s, branch left in place (gas-fk4): %v\n", style.Dim.Render("○"), mr.Branch, branchCleanup.PRLookupErr)
	} else if branchCleanup.AlreadyGone {
		fmt.Fprintf(w, "  %s Remote branch already absent: %s\n", style.Dim.Render("○"), mr.Branch)
	} else if branchCleanup.LeftForOwner {
		fmt.Fprintf(w, "  %s remote branch %s left for its owner (not a polecat/integration branch)\n", style.Dim.Render("○"), mr.Branch)
	} else if branchCleanup.RemoteDeleted {
		fmt.Fprintf(w, "  %s Deleted remote branch: %s\n", style.Success.Render("✓"), mr.Branch)
	}
	if branchCleanup.Err != nil {
		fmt.Fprintf(w, "  %s remote branch cleanup failed: %v\n", style.Error.Render("✗"), branchCleanup.Err)
	}

	if branchCleanup.LocalDeleted {
		fmt.Fprintf(w, "  %s Deleted local branch: %s\n", style.Success.Render("✓"), mr.Branch)
	}
}

func printMQPostMergeOrphan(w io.Writer, cleanup mqPostMergeBranchCleanup) {
	fmt.Fprintf(w, "%s Orphaned branch cleanup: %s\n", style.Bold.Render("✓"), cleanup.Branch)
	fmt.Fprintf(w, "  %s\n", style.Dim.Render("(no MR bead; branch-only cleanup)"))

	switch {
	case cleanup.Disabled:
		fmt.Fprintf(w, "  %s Branch delete disabled by config\n", style.Dim.Render("○"))
	case cleanup.AlreadyGone:
		fmt.Fprintf(w, "  %s Remote branch already absent\n", style.Dim.Render("○"))
	case cleanup.OpenPR:
		fmt.Fprintf(w, "  %s Skipping remote branch delete for %s: open PR exists (gas-fk4)\n", style.Dim.Render("○"), cleanup.Branch)
	case cleanup.PRUnknown:
		fmt.Fprintf(w, "  %s PR state unknown for %s, branch left in place (gas-fk4): %v\n", style.Dim.Render("○"), cleanup.Branch, cleanup.PRLookupErr)
	case cleanup.RemoteDeleted:
		fmt.Fprintf(w, "  %s Deleted remote branch: %s (preserved on %s)\n", style.Success.Render("✓"), cleanup.Branch, cleanup.Target)
	}

	if cleanup.LocalDeleted {
		fmt.Fprintf(w, "  %s Deleted local branch: %s\n", style.Success.Render("✓"), cleanup.Branch)
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
	if mr == nil {
		return nil, mqPostMergeBranchCleanup{}, fmt.Errorf("merge proof failed: merge request is missing")
	}
	// The structural checks run before any inference touches the snapshot.
	// A directly-created MR can be missing its target or, worse, record its
	// target as its own source branch; an inference against such an MR would
	// read a live tip and feed it into a comparison the refusal was meant to
	// prevent.
	target := strings.TrimSpace(mr.TargetBranch)
	if target == "" {
		return nil, mqPostMergeBranchCleanup{}, fmt.Errorf("merge proof failed for MR %s: missing target branch", mr.ID)
	}
	if source := strings.TrimSpace(mr.Branch); source != "" && source == target {
		return nil, mqPostMergeBranchCleanup{}, fmt.Errorf("merge proof failed for MR %s: source branch %s matches target branch", mr.ID, source)
	}
	// head is the commit the proof will verify. When the MR bead recorded one,
	// it is that record; when it recorded none, it is the head read off the
	// branch the MR names. The snapshot's CommitSHA is never set from the
	// inference: the close-time CAS compares it against the bead's own record
	// (empty), so the snapshot stays what the bead says and the recovered head
	// travels in VerifiedHead/CommitSHAInferred instead, where the close path
	// persists it to the bead (gt-6o1u).
	head := strings.TrimSpace(mr.CommitSHA)
	var inferredHead string
	if head == "" {
		inferredHead, err = inferMQPostMergeSubmittedHead(rigGit, mr)
		if err != nil {
			return nil, mqPostMergeBranchCleanup{}, err
		}
		head = inferredHead
		if head == "" {
			return nil, mqPostMergeBranchCleanup{}, fmt.Errorf("merge proof failed for MR %s: missing submitted commit_sha", mr.ID)
		}
		// An inferred head that is already an ancestor of the target carries no
		// commits of the branch's own — the proof would pass vacuously against
		// whatever the branch did before it was replaced. Refuse it: something
		// of this branch's own work has to be what landed.
		//
		// The check cannot compare against target's whole history: after any
		// real landing, head IS an ancestor of target's current tip (that is
		// what the reachability proof below establishes), so a fast-forward or
		// merge-commit landing would look identical to a vacuous branch and be
		// refused every time (om review, attempt 3). Comparing against
		// target's own state one commit before its current tip narrows this to
		// the case that actually distinguishes them: a fast-forward's head is
		// target's own current tip (not yet an ancestor of target minus its
		// last commit), and a merge commit's head is that merge's other
		// parent (also not an ancestor of the commit before it); a genuinely
		// vacuous head was already an ancestor of target before whatever
		// landed most recently, and stays refused. resolvedTarget is fetched
		// fresh so a stale local target ref cannot change the outcome.
		defaultBranch := rigGit.RemoteDefaultBranch()
		resolvedTarget := rigGit.CleanBaseRef("origin", defaultBranch, target)
		if targetRemote := git.RemoteForRef(resolvedTarget); targetRemote != "" {
			if err := rigGit.FetchPrune(targetRemote); err != nil {
				return nil, mqPostMergeBranchCleanup{}, fmt.Errorf("merge proof failed for MR %s: refreshing %s to check inferred head %s: %w", mr.ID, targetRemote, head, err)
			}
		}
		// A resolve failure here means target's current tip has no parent —
		// possible only when the whole repository is that one commit. There is
		// no earlier state for a vacuous head to have already been an ancestor
		// of, so there is nothing this guard could catch; leave it to the
		// reachability proof below.
		if priorTarget, err := rigGit.Rev(resolvedTarget + "^"); err == nil && priorTarget != "" {
			base, err := rigGit.MergeBase(priorTarget, head)
			if err != nil {
				return nil, mqPostMergeBranchCleanup{}, fmt.Errorf("merge proof failed for MR %s: merge-base of %s and inferred head %s: %w", mr.ID, priorTarget, head, err)
			}
			if base == head {
				return nil, mqPostMergeBranchCleanup{}, fmt.Errorf("merge proof failed for MR %s: inferred head %s is already an ancestor of %s: the branch carries no commits of its own, so nothing of it landed; re-record commit_sha on the MR bead if the branch has been replaced", mr.ID, head, target)
			}
		}
	}
	mergeCommit, err := verifyMQPostMergeProof(rigGit, mr, head, landedCommit)
	if err != nil {
		if inferredHead != "" {
			// The head this refused was never submitted, and that is invisible in
			// the proof's own wording: without this the refusal reads as the
			// rebase fight it is easy to mistake it for.
			err = fmt.Errorf("%w (head inferred from branch %s: the MR bead records no commit_sha)", err, strings.TrimSpace(mr.Branch))
		}
		return nil, mqPostMergeBranchCleanup{}, err
	}
	if mergeCommit != "" {
		mr.MergeCommit = mergeCommit
	}
	if inferredHead != "" {
		mr.CommitSHAInferred = true
		mr.VerifiedHead = inferredHead
	}

	result, err := mgr.PostMergeMR(mr)
	if err != nil {
		return result, mqPostMergeBranchCleanup{}, err
	}

	branchCleanup, err := cleanupMQPostMergeBranch(rigPath, rigGit, result.MR, skipBranchDelete)
	branchCleanup.SubmittedHead = head
	branchCleanup.SubmittedHeadInferred = inferredHead != ""
	if err != nil {
		err = &mqPostMergeBranchCleanupError{err: err}
	}
	return result, branchCleanup, err
}

// mqPostMergeBranchCleanupError marks a failure of the branch cleanup that
// runs after the merge proof passed and the MR closed, so resolveMQPostMerge
// can tell it from a failure that happened before the landing was confirmed
// (gt-qyf1t). Its message is the underlying error's.
type mqPostMergeBranchCleanupError struct{ err error }

func (e *mqPostMergeBranchCleanupError) Error() string { return e.err.Error() }
func (e *mqPostMergeBranchCleanupError) Unwrap() error { return e.err }

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
//
// head is the commit the proof verifies, passed in by the caller so this
// stays pure. runVerifiedMQPostMerge resolves it: the MR bead's recorded
// commit_sha when the bead has one, the head read off the recorded branch
// when it has none (inferMQPostMergeSubmittedHead, gt-6o1u). This function
// itself never guesses a head — an empty one is a refusal, not an
// inference — and everything it checks is fail-closed against the target.
func verifyMQPostMergeProof(rigGit mqPostMergeGit, mr *refinery.MergeRequest, head string, landedCommit string) (string, error) {
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
	commit := strings.TrimSpace(head)
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

// inferMQPostMergeSubmittedHead completes an MR bead that records no
// commit_sha (a directly-created MR — neither 'gt mq submit' nor 'gt done'
// wrote it), returning the head read off the branch the MR names and "" for
// an MR that already carries one. The returned head still goes through the
// same reachability proof as a submitted one; inference only picks which
// commit is proven, never whether it has to be (gt-6o1u). A branch that is
// already gone leaves nothing to infer, and the recovery is named in the
// error.
func inferMQPostMergeSubmittedHead(rigGit mqPostMergeGit, mr *refinery.MergeRequest) (string, error) {
	if mr == nil || strings.TrimSpace(mr.CommitSHA) != "" {
		return "", nil
	}
	branch := strings.TrimSpace(mr.Branch)
	if branch == "" {
		return "", fmt.Errorf("merge proof failed for MR %s: missing submitted commit_sha and no branch on the MR to resolve it from: re-record commit_sha on the MR bead (a directly-created MR records neither)", mr.ID)
	}
	tip, err := rigGit.PushRemoteBranchTip("origin", branch)
	if err != nil {
		return "", fmt.Errorf("merge proof failed for MR %s: missing submitted commit_sha and branch %s could not be read: %w", mr.ID, branch, err)
	}
	if tip = strings.TrimSpace(tip); tip == "" {
		return "", fmt.Errorf("merge proof failed for MR %s: missing submitted commit_sha and branch %s has no remote tip (already deleted?): re-record commit_sha on the MR bead; --landed-commit cannot substitute because the attestation is bound to the submitted head", mr.ID, branch)
	}
	return tip, nil
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
// in target's history is "reachable from target". Two bindings, either of
// which suffices:
//
//   - The run of commits ending at landedCommit carries every commit of the
//     submitted range, matched by patch-id. A landing can reproduce the MR as
//     one commit (squash, merge) or as the MR's own commits replayed by the
//     rebase (a fast-forward of the branch — gt done's direct-merge convoy
//     pushes one, which leaves landedCommit carrying only the last of the MR's
//     commits against target's tip); patch-id sees the same work in both and
//     survives the sha rewrite a rebase performs.
//   - Every file the submitted branch touched (relative to where it diverged
//     from target) also appears changed in landedCommit's diff, against its
//     parent.
//
// The second is a file-level fallback, not a byte-exact one: a
// conflict-resolved rebase legitimately changes hunks within those files
// (that's the entire reason --landed-commit exists — see
// verifyMQPostMergeProof), so requiring patch-ids would reject every real
// conflict resolution along with the forgeries. It also cannot see a change
// that landed without any of its files appearing in landedCommit's own diff,
// which is what the first binding is for. Both still reject an unrelated
// on-target commit, and a partially-landed MR: the commits that did not land
// end the run or have no patch-id there, and their files are absent from the
// landed diff.
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
	if submittedRangeLandedByPatchID(rigGit, submittedBase, submittedCommit, landedCommit) {
		return landedCommit, nil
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

// submittedRangeLandedByPatchID reports whether the attested commit is itself
// the landing of the submitted range: whether the run of commits ending at it
// carries every one of the MR's commits, identified by patch-id.
//
// The walk back from the attested commit stops at the first commit whose
// change is not one of the MR's, so the run is bounded by the MR and cannot
// wander into target history. That bound is the whole point: an MR that landed
// leaves its patches on target, so a check that only asked "are these patches
// somewhere on target" would accept any later on-target commit — including one
// landed by an unrelated MR, which is what --landed-commit must never admit
// (gt-7dnx). Anchoring at the attested commit answers the question the flag
// actually asks: did *this* commit carry the MR?
//
// A missing id is not proof the MR did not land (conflict resolution rewrites
// the patch, and the run is the last landings of a rebase, so an interrupted
// run stops the walk), so a false return falls through to the file-level
// binding rather than rejecting. The reads are best-effort for that same
// reason: an unreadable commit is not evidence either way.
func submittedRangeLandedByPatchID(rigGit mqPostMergeGit, submittedBase, submittedCommit, landedCommit string) bool {
	submittedIDs, err := rigGit.PatchIDs(submittedBase, submittedCommit)
	if err != nil || len(submittedIDs) == 0 {
		// No ids to check: an empty set must never read as "all present".
		return false
	}
	owned := make(map[string]int, len(submittedIDs))
	for _, id := range submittedIDs {
		owned[id]++
	}

	var landedIDs []string
	commit := strings.TrimSpace(landedCommit)
	for {
		parent, err := rigGit.Rev(commit + "^")
		if err != nil {
			break
		}
		parent = strings.TrimSpace(parent)
		id, err := rigGit.PatchID(parent, commit)
		id = strings.TrimSpace(id)
		if err != nil || owned[id] == 0 {
			// The run of this MR's commits ends here.
			break
		}
		// Consuming the id bounds the walk by the MR's own commit count: a
		// repeat of one already matched stops it.
		owned[id]--
		landedIDs = append(landedIDs, id)
		commit = parent
	}
	if len(landedIDs) == 0 {
		return false
	}
	return len(patchIDsMissingFrom(submittedIDs, landedIDs)) == 0
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
	// "HEAD" is what git rev-parse --abbrev-ref HEAD prints for a detached
	// worktree instead of failing outright; a stray second gt done can record
	// it as the MR's branch field while leaving commit_sha untouched, so the
	// merge itself looks unaffected. Trusting it here verbatim is exactly the
	// degraded-record bug this guards against (gt-1p1i): "HEAD" is not a ref
	// under refs/heads, so ls-remote --heads finds nothing for it and the
	// caller below would report the real, still-orphaned work branch as
	// already gone — a false success that skips its actual cleanup. Refuse
	// instead of guessing; the real branch name was lost and needs a human
	// (or the orphan-branch reaper) to find and delete it.
	if cleanup.Branch == "HEAD" {
		return cleanup, fmt.Errorf("remote branch delete: MR %s records the literal ref %q as its branch, not a real branch name (a detached-HEAD gt done capture, see gt-1p1i); the actual work branch was never recorded and needs manual cleanup", mr.ID, cleanup.Branch)
	}
	if skipBranchDelete {
		cleanup.Skipped = true
		return cleanup, nil
	}
	if !mqDeleteMergedBranchesEnabled(rigPath) {
		cleanup.Disabled = true
		return cleanup, nil
	}
	// A crew (or any non-work) branch belongs to its owner, and the refinery
	// clone's pre-push hook refuses to delete it anyway: attempting it only
	// turned every crew MR's post-merge into a failure (gt-qyf1t).
	if !mqPostMergeRemoteBranchDeletable(cleanup.Branch) {
		cleanup.LeftForOwner = true
		return cleanup, nil
	}

	// The lease pins to the head the proof verified. For a recovered head the
	// snapshot's CommitSHA is still the bead's own record (empty) and the
	// verified head rides in VerifiedHead (gt-6o1u).
	expectedHead := strings.TrimSpace(mr.CommitSHA)
	if expectedHead == "" && mr.CommitSHAInferred {
		expectedHead = strings.TrimSpace(mr.VerifiedHead)
	}
	if expectedHead == "" {
		return cleanup, fmt.Errorf("remote branch delete %s: missing submitted commit_sha", cleanup.Branch)
	}
	deleteAt := expectedHead

	// Deleting a branch with an open PR causes GitHub to auto-close the PR as
	// "closed" (not "merged"), destroying the PR audit trail. (gas-fk4)
	// A failed or ambiguous lookup fails closed the same way an open PR does
	// (Unknown), but it is reported as an unresolved lookup, not a claimed
	// open PR the operator would go hunt for and never find (gt-ghpk).
	prState, prErr := rigGit.PullRequestProtection(git.PullRequestRef{URL: mr.PRURL, Number: mr.PRNumber, Branch: cleanup.Branch, HeadSHA: expectedHead})
	switch prState {
	case git.PRProtectionOpen:
		cleanup.OpenPR = true
	case git.PRProtectionUnknown:
		cleanup.PRUnknown = true
		cleanup.PRLookupErr = prErr
	default:
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
	// This runs identically whether expectedHead was submitted or inferred
	// (gt-6o1u): an inferred head still only proved the tip the proof read,
	// not whatever the branch points at now, so a moved tip needs the same
	// content-preservation check before the delete is allowed to follow it —
	// skipping it here was a fail-open branch-delete on exactly the MRs with
	// the least provenance.
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
