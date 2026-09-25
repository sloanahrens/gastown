package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Polecat command flags
var (
	polecatListJSON  bool
	polecatListAll   bool
	polecatForce     bool
	polecatRemoveAll bool
)

var polecatCmd = &cobra.Command{
	Use:     "polecat",
	Aliases: []string{"polecats"},
	GroupID: GroupAgents,
	Short:   "Manage polecats (persistent identity, ephemeral sessions)",
	RunE:    requireSubcommand,
	Long: `Manage polecat lifecycle in rigs.

Polecats have PERSISTENT IDENTITY but EPHEMERAL SESSIONS. Each polecat has
a permanent agent bead and CV chain that accumulates work history across
assignments. Sessions and sandboxes are ephemeral — spawned for specific
tasks, cleaned up on completion — but the identity persists.

A polecat is either:
  - Working: Actively doing assigned work
  - Stalled: Session crashed mid-work (needs Witness intervention)
  - Zombie: Finished but gt done failed (needs cleanup)
  - Nuked: Session ended, identity persists (ready for next assignment)

Self-cleaning model: When work completes, the polecat runs 'gt done',
which pushes the branch, submits to the merge queue, and exits. The
Witness then nukes the sandbox. The polecat's identity (agent bead)
persists with agent_state=nuked, preserving work history.

Session vs sandbox: The Claude session cycles frequently (handoffs,
compaction). The git worktree (sandbox) persists until nuke. Work
survives session restarts.

Cats build features. Dogs clean up messes.`,
}

var polecatListCmd = &cobra.Command{
	Use:   "list [rig]",
	Short: "List polecats in a rig",
	Long: `List polecats in a rig or all rigs.

In the transient model, polecats exist only while working. The list shows
all polecats with their states:
  - working: Actively working on an issue
  - done: Completed work, waiting for cleanup
  - stuck: Needs assistance

Examples:
  gt polecat list greenplace
  gt polecat list --all
  gt polecat list greenplace --json`,
	RunE: runPolecatList,
}

var polecatAddCmd = &cobra.Command{
	Use:        "add <rig> <name>",
	Short:      "Add a new polecat to a rig (DEPRECATED)",
	Deprecated: "use 'gt polecat identity add' instead. This command will be removed in v1.0.",
	Long: `Add a new polecat to a rig.

DEPRECATED: Use 'gt polecat identity add' instead. This command will be removed in v1.0.

Creates a polecat directory, clones the rig repo, creates a work branch,
and initializes state.

Example:
  gt polecat identity add greenplace Toast  # Preferred
  gt polecat add greenplace Toast           # Deprecated`,
	Args: cobra.ExactArgs(2),
	RunE: runPolecatAdd,
}

var polecatRemoveCmd = &cobra.Command{
	Use:   "remove <rig>/<polecat>... | <rig> --all",
	Short: "Remove polecats from a rig",
	Long: `Remove one or more polecats from a rig.

Fails if session is running (stop first).
Warns if uncommitted changes exist.
Use --force to bypass checks.

Examples:
  gt polecat remove greenplace/Toast
  gt polecat remove greenplace/Toast greenplace/Furiosa
  gt polecat remove greenplace --all
  gt polecat remove greenplace --all --force`,
	Args: cobra.MinimumNArgs(1),
	RunE: runPolecatRemove,
}

var polecatStatusCmd = &cobra.Command{
	Use:   "status <rig>/<polecat>",
	Short: "Show detailed status for a polecat",
	Long: `Show detailed status for a polecat.

Displays comprehensive information including:
  - Current lifecycle state (working, done, stuck, idle)
  - Assigned issue (if any)
  - Session status (running/stopped, attached/detached)
  - Session creation time
  - Last activity time

NOTE: The argument is <rig>/<polecat> — a single argument with a slash
separator, NOT two separate arguments. For example: greenplace/Toast

Examples:
  gt polecat status greenplace/Toast
  gt polecat status greenplace/Toast --json`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatStatus,
}

var (
	polecatStatusJSON                    bool
	polecatGitStateJSON                  bool
	polecatGCDryRun                      bool
	polecatNukeAll                       bool
	polecatNukeDryRun                    bool
	polecatNukeForce                     bool
	polecatCheckRecoveryJSON             bool
	polecatCheckRecoveryReconcileCleanup bool
	polecatCheckRecoveryBatchJSON        bool
	polecatCheckRecoveryBatchReconcile   bool
	polecatPoolInitDryRun                bool
	polecatPoolInitSize                  int
)

var polecatGCCmd = &cobra.Command{
	Use:   "gc <rig>",
	Short: "Garbage collect stale polecat branches",
	Long: `Garbage collect stale polecat branches in a rig.

Polecats use unique timestamped branches (polecat/<name>-<timestamp>) to
prevent drift issues. Over time, these branches accumulate when stale
polecats are repaired.

This command removes orphaned branches:
  - Branches for polecats that no longer exist
  - Old timestamped branches (keeps only the current one per polecat)

Examples:
  gt polecat gc greenplace
  gt polecat gc greenplace --dry-run`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatGC,
}

var polecatNukeCmd = &cobra.Command{
	Use:   "nuke <rig>/<polecat>... | <rig> --all",
	Short: "Completely destroy a polecat (session, worktree, branch, agent bead)",
	Long: `Completely destroy a polecat and all its artifacts.

This is the nuclear option for post-merge cleanup. It:
  1. Kills the Claude session (if running)
  2. Deletes the git worktree (bypassing all safety checks)
  3. Deletes the polecat branch
  4. Closes the agent bead (if exists)

SAFETY CHECKS: The command refuses to nuke a polecat if:
  - cleanup_status is dirty, unknown, or missing
  - Worktree fallback detects unpushed/uncommitted/stashed changes
  - Polecat has an open merge request (MR bead or active_mr)
  - Polecat has work on its hook

Use --force to bypass safety checks (LOSES WORK).
Use --dry-run to see what would happen and safety check status.

PRESERVATION: before deleting anything, nuke pushes the polecat's branch to
origin (falling back to <branch>-<sha7> when the plain push is rejected as
non-fast-forward) and refuses to proceed unless a remote ref is confirmed to
contain the branch tip. --force alone does not override that refusal: it takes
--force AND GT_NUKE_ACKNOWLEDGE_UNPRESERVED=1 to delete a branch whose only
copy is local.

Examples:
  gt polecat nuke greenplace/Toast
  gt polecat nuke greenplace/Toast greenplace/Furiosa
  gt polecat nuke greenplace --all
  gt polecat nuke greenplace --all --dry-run
  gt polecat nuke greenplace/Toast --force  # bypass safety checks`,
	Args: cobra.MinimumNArgs(1),
	RunE: runPolecatNuke,
}

var polecatGitStateCmd = &cobra.Command{
	Use:   "git-state <rig>/<polecat>",
	Short: "Show git state for pre-kill verification",
	Long: `Show git state for a polecat's worktree.

Used by the Witness for pre-kill verification to ensure no work is lost.
Returns whether the worktree is clean (safe to kill) or dirty (needs cleanup).

Checks:
  - Working tree: uncommitted changes
  - Unpushed commits: commits ahead of origin/main
  - Stashes: stashed changes

Examples:
  gt polecat git-state greenplace/Toast
  gt polecat git-state greenplace/Toast --json`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatGitState,
}

var polecatCheckRecoveryCmd = &cobra.Command{
	Use:   "check-recovery <rig>/<polecat>",
	Short: "Check if polecat needs recovery vs safe to nuke",
	Long: `Check recovery status of a polecat based on cleanup_status, active_mr, and merge queue state.

Used by the Witness to determine appropriate cleanup action:
  - SAFE_TO_NUKE: cleanup_status is 'clean', active_mr is terminal, AND work submitted to merge queue
  - NEEDS_MQ_SUBMIT: git is clean but work was never submitted to the merge queue
  - NEEDS_RECOVERY: cleanup_status, active_mr, or fallback git predicates require recovery

This prevents accidental data loss when cleaning up dormant polecats.
The Witness should escalate NEEDS_RECOVERY and NEEDS_MQ_SUBMIT cases to the Mayor.

Examples:
  gt polecat check-recovery greenplace/Toast
  gt polecat check-recovery greenplace/Toast --json`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatCheckRecovery,
}

var polecatCheckRecoveryBatchCmd = &cobra.Command{
	Use:   "check-recovery-batch <rig>",
	Short: "Check recovery status for every polecat in a rig in one pass",
	Long: `Check recovery status for every polecat in a rig, fleet-wide, in one process.

Answers the same SAFE_TO_NUKE / NEEDS_MQ_SUBMIT / PENDING_MR / NEEDS_RECOVERY
question as 'gt polecat check-recovery' for each polecat in the rig, but
without spawning one 'gt polecat check-recovery' subprocess per polecat.

check-recovery's per-polecat cost is dominated by bd subprocess round trips:
one bulk agent-bead fetch and one bulk merge-request fetch, shared by every
polecat in the sweep, replace what would otherwise be that many repeated
full-table scans (see gt-b839, follow-up to gt-ct3). Each polecat's git-state
check still runs per-worktree — that part is unavoidable and not a bd cost.

Examples:
  gt polecat check-recovery-batch greenplace
  gt polecat check-recovery-batch greenplace --json`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatCheckRecoveryBatch,
}

var (
	polecatStaleJSON      bool
	polecatStaleThreshold int
	polecatStaleCleanup   bool
	polecatStaleDryRun    bool
	polecatPruneDryRun    bool
	polecatPruneRemote    bool
)

var polecatStaleCmd = &cobra.Command{
	Use:   "stale <rig>",
	Short: "Detect stale polecats that may need cleanup",
	Long: `Detect stale polecats in a rig that are candidates for cleanup.

A polecat is considered stale if:
  - No active tmux session
  - Way behind main (>threshold commits) OR no agent bead
  - Has no uncommitted work that could be lost

The default threshold is 20 commits behind main.

Use --cleanup to automatically nuke stale polecats that are safe to remove.
Use --dry-run with --cleanup to see what would be cleaned.

Examples:
  gt polecat stale greenplace
  gt polecat stale greenplace --threshold 50
  gt polecat stale greenplace --json
  gt polecat stale greenplace --cleanup
  gt polecat stale greenplace --cleanup --dry-run`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatStale,
}

var polecatPruneCmd = &cobra.Command{
	Use:   "prune <rig>",
	Short: "Prune stale polecat branches (local and remote)",
	Long: `Prune stale polecat branches in a rig.

Finds and deletes polecat branches that are no longer needed:
  - Branches fully merged to main
  - Branches whose remote tracking branch was deleted (post-merge cleanup)
  - Branches for polecats that no longer exist (orphaned)

Uses safe deletion (git branch -d) — only removes fully merged branches.
Also cleans up remote polecat branches that are fully merged.

Use --dry-run to preview what would be pruned.
Use --remote to also prune remote polecat branches on origin.

Examples:
  gt polecat prune greenplace
  gt polecat prune greenplace --dry-run
  gt polecat prune greenplace --remote`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatPrune,
}

var polecatPoolInitCmd = &cobra.Command{
	Use:   "pool-init <rig>",
	Short: "Initialize a persistent polecat pool for a rig",
	Long: `Initialize a persistent polecat pool for a rig.

Creates N polecats with identities and worktrees in IDLE state,
ready for immediate work assignment via gt sling.

Pool size is determined by (in priority order):
  1. --size flag
  2. polecat_pool_size in rig config.json
  3. Default: 4

Polecat names come from:
  1. polecat_names in rig config.json (if specified)
  2. The rig's name pool theme (default: mad-max)

Existing polecats are preserved — only new ones are created
to reach the target pool size.

Examples:
  gt polecat pool-init gastown
  gt polecat pool-init gastown --size 6
  gt polecat pool-init gastown --dry-run`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatPoolInit,
}

func init() {
	// List flags
	polecatListCmd.Flags().BoolVar(&polecatListJSON, "json", false, "Output as JSON")
	polecatListCmd.Flags().BoolVar(&polecatListAll, "all", false, "List polecats in all rigs")

	// Remove flags
	polecatRemoveCmd.Flags().BoolVarP(&polecatForce, "force", "f", false, "Force removal, bypassing checks")
	polecatRemoveCmd.Flags().BoolVar(&polecatRemoveAll, "all", false, "Remove all polecats in the rig")

	// Status flags
	polecatStatusCmd.Flags().BoolVar(&polecatStatusJSON, "json", false, "Output as JSON")

	// Git-state flags
	polecatGitStateCmd.Flags().BoolVar(&polecatGitStateJSON, "json", false, "Output as JSON")

	// GC flags
	polecatGCCmd.Flags().BoolVar(&polecatGCDryRun, "dry-run", false, "Show what would be deleted without deleting")

	// Nuke flags
	polecatNukeCmd.Flags().BoolVar(&polecatNukeAll, "all", false, "Nuke all polecats in the rig")
	polecatNukeCmd.Flags().BoolVar(&polecatNukeDryRun, "dry-run", false, "Show what would be nuked without doing it")
	polecatNukeCmd.Flags().BoolVarP(&polecatNukeForce, "force", "f", false, "Force nuke, bypassing all safety checks (LOSES WORK)")

	// Check-recovery flags
	polecatCheckRecoveryCmd.Flags().BoolVar(&polecatCheckRecoveryJSON, "json", false, "Output as JSON")
	polecatCheckRecoveryCmd.Flags().BoolVar(&polecatCheckRecoveryReconcileCleanup, "reconcile-cleanup", false, "Safely rewrite stale dirty cleanup_status to clean when live recovery predicates prove no work is at risk")

	// Check-recovery-batch flags
	polecatCheckRecoveryBatchCmd.Flags().BoolVar(&polecatCheckRecoveryBatchJSON, "json", false, "Output as JSON")
	polecatCheckRecoveryBatchCmd.Flags().BoolVar(&polecatCheckRecoveryBatchReconcile, "reconcile-cleanup", false, "Safely rewrite stale dirty cleanup_status to clean when live recovery predicates prove no work is at risk")

	// Stale flags
	polecatStaleCmd.Flags().BoolVar(&polecatStaleJSON, "json", false, "Output as JSON")
	polecatStaleCmd.Flags().IntVar(&polecatStaleThreshold, "threshold", 20, "Commits behind main to consider stale")
	polecatStaleCmd.Flags().BoolVar(&polecatStaleCleanup, "cleanup", false, "Automatically nuke stale polecats")
	polecatStaleCmd.Flags().BoolVar(&polecatStaleDryRun, "dry-run", false, "Show what would be cleaned without doing it")

	// Prune flags
	polecatPruneCmd.Flags().BoolVar(&polecatPruneDryRun, "dry-run", false, "Show what would be pruned without doing it")
	polecatPruneCmd.Flags().BoolVar(&polecatPruneRemote, "remote", false, "Also prune remote polecat branches on origin")

	// Pool-init flags
	polecatPoolInitCmd.Flags().BoolVar(&polecatPoolInitDryRun, "dry-run", false, "Show what would be created without doing it")
	polecatPoolInitCmd.Flags().IntVar(&polecatPoolInitSize, "size", 0, "Pool size (overrides rig config)")

	// Add subcommands
	polecatCmd.AddCommand(polecatListCmd)
	polecatCmd.AddCommand(polecatAddCmd)
	polecatCmd.AddCommand(polecatRemoveCmd)
	polecatCmd.AddCommand(polecatStatusCmd)
	polecatCmd.AddCommand(polecatGitStateCmd)
	polecatCmd.AddCommand(polecatCheckRecoveryCmd)
	polecatCmd.AddCommand(polecatCheckRecoveryBatchCmd)
	polecatCmd.AddCommand(polecatGCCmd)
	polecatCmd.AddCommand(polecatNukeCmd)
	polecatCmd.AddCommand(polecatStaleCmd)
	polecatCmd.AddCommand(polecatPruneCmd)
	polecatCmd.AddCommand(polecatPoolInitCmd)

	rootCmd.AddCommand(polecatCmd)
}

// PolecatListItem represents a polecat in list output.
type PolecatListItem struct {
	Rig   string        `json:"rig"`
	Name  string        `json:"name"`
	State polecat.State `json:"state"`
	Issue string        `json:"issue,omitempty"`
	// Agent is the coding agent the live session runs (the session's GT_AGENT,
	// written at spawn). Empty for a polecat with no live session.
	Agent string `json:"agent,omitempty"`
	// MRID and MRStatus are the polecat's merge request from one bulk
	// rig-wide query: MRID is the MR bead — the agent bead's active_mr when
	// set, else the open MR this worker submitted — and MRStatus is its queue
	// state (open|ready|blocked|merged|rejected|missing). A polecat with no MR
	// in this rig reports neither field.
	MRID          string `json:"mr_id,omitempty"`
	MRStatus      string `json:"mr_status,omitempty"`
	CleanupStatus string `json:"cleanup_status,omitempty"`
	ActiveMR      string `json:"active_mr,omitempty"`
	// CleanupStatusSource and GitStateSource label where cleanup_status and the
	// git facts came from, so a consumer can tell a recorded self-report from a
	// live measurement: cleanup_status is always "recorded" (it is written once
	// by gt done and never re-derived), while git_state_source is "live" when
	// the worktree was probed, "unknown" (with git_state_reason) when that probe
	// failed, and "recorded" when no probe was attempted at all and the reuse
	// verdict fell back to the recorded hint (claude-41j.1 D9).
	CleanupStatusSource  string   `json:"cleanup_status_source,omitempty"`
	GitStateSource       string   `json:"git_state_source,omitempty"`
	GitStateReason       string   `json:"git_state_reason,omitempty"`
	Branch               string   `json:"branch,omitempty"`
	Verdict              string   `json:"verdict,omitempty"`
	Reason               string   `json:"reason,omitempty"`
	Reusable             bool     `json:"reusable"`
	SafeToNuke           bool     `json:"safe_to_nuke"`
	NeedsRecovery        bool     `json:"needs_recovery"`
	NeedsMQSubmit        bool     `json:"needs_mq_submit"`
	MQStatus             string   `json:"mq_status,omitempty"`
	CountsTowardCapacity bool     `json:"counts_toward_capacity"`
	ReuseStatus          string   `json:"reuse_status,omitempty"`
	Blockers             []string `json:"blockers,omitempty"`
	SessionRunning       bool     `json:"session_running"`
	Zombie               bool     `json:"zombie,omitempty"`
	Foreign              bool     `json:"foreign,omitempty"`
	BeadLookupFailed     bool     `json:"bead_lookup_failed,omitempty"`
	SessionName          string   `json:"session_name,omitempty"`
}

// effectivePolecatState returns the observable state used by polecat list output.
// Active work is ground truth for working; tmux liveness alone is not enough
// because persistent polecats may keep a reusable live session after completion.
// Zombie entries are never auto-rewritten.
//
// spawning says the agent bead is still inside its spawn grace (see
// polecatInventoryItem.Spawning): the dispatch is in flight, so the "session
// dead + work assigned → stalled" rewrite below must not fire. A grace applied
// in the inventory but undone here is the same polecat read two ways (gt-yteq).
func effectivePolecatState(item PolecatListItem, spawning bool) polecat.State {
	state := item.State
	// A running session only implies working when there is active work attached.
	// Without an issue, rewriting idle/done to working recreates "Issue: (none)".
	if item.SessionRunning && item.Issue != "" && item.CountsTowardCapacity && (state == polecat.StateDone || state == polecat.StateIdle) {
		return polecat.StateWorking
	}
	// When session is dead but beads still says "working", mark as stalled
	// (not done — work was interrupted, not completed). The manager's loadFromBeads
	// now returns StateStalled for this case, but list reconciliation may override.
	if !item.SessionRunning && !item.Zombie && state == polecat.StateWorking {
		if spawning {
			return polecat.StateSpawning
		}
		return polecat.StateStalled
	}
	return state
}

// polecatSpawnGraceWindow resolves the window a dispatched-but-not-live polecat
// is read as spawning before it becomes stalled, from the same town thresholds
// the Witness uses (config.WitnessThresholds.HeartbeatStartupGrace, default
// 5m), so list and witness agree on when spawning ends. An unreadable town root
// yields the compiled-in default: the grace only ever delays a stalled verdict,
// so failing to read the config must not fail it off (gt-yteq).
func polecatSpawnGraceWindow(townRoot string) time.Duration {
	if strings.TrimSpace(townRoot) == "" {
		return config.DefaultWitnessHeartbeatStartupGrace
	}
	return config.LoadOperationalConfig(townRoot).GetWitnessConfig().HeartbeatStartupGraceD()
}

// polecatAgentMRDetails renders the agent and merge-request fields for one
// polecat line — "agent=wisp mr=gt-wisp-1c6 mr_status=ready" — or "" when the
// polecat has neither. Fields are omitted, never shown empty, so a line only
// ever claims what the rig-wide join actually found.
func polecatAgentMRDetails(p PolecatListItem) string {
	fields := make([]string, 0, 3)
	if p.Agent != "" {
		fields = append(fields, "agent="+p.Agent)
	}
	if p.MRID != "" {
		details := "mr=" + p.MRID
		if p.MRStatus != "" {
			details += " " + p.MRStatus
		}
		fields = append(fields, details)
	}
	return strings.Join(fields, " ")
}

// polecatReuseDetailLine renders one polecat's "reuse: ..." detail line, or ""
// when the polecat has no reuse status to report.
//
// The provenance of each cleanup input is marked inline (claude-41j.1 D9):
// cleanup_status is always a recorded self-report, so it is tagged as such
// rather than left to look like state measured now, and the git facts behind
// the verdict say whether they were "live", "unknown" (with the reason that
// failed the probe), or a "recorded" hint standing in for a probe that never
// ran. A reader must be able to tell which side of that line the verdict came
// from without asking the JSON.
func polecatReuseDetailLine(p PolecatListItem) string {
	if p.ReuseStatus == "" {
		return ""
	}
	details := "reuse: " + p.ReuseStatus
	if p.CleanupStatus != "" {
		details += " cleanup=" + p.CleanupStatus
		if p.CleanupStatusSource != "" {
			details += " (" + p.CleanupStatusSource + ")"
		}
	}
	if p.GitStateSource != "" {
		details += " git=" + p.GitStateSource
		if p.GitStateReason != "" {
			details += " (" + p.GitStateReason + ")"
		}
	}
	if p.ActiveMR != "" {
		details += " active_mr=" + p.ActiveMR
	}
	return details
}

type reuseMRShower interface {
	Show(issueID string) (*beads.Issue, error)
}

func activeMRBlocksReuse(bd reuseMRShower, mrID, sourceHint string, requireGitSafe, gitSafe bool) bool {
	assessment := polecat.AssessActiveMR(bd, polecat.ActiveMRInput{ActiveMR: mrID, SourceIssueHint: sourceHint, RequireGitSafe: requireGitSafe, GitSafe: gitSafe})
	return assessment.Pending
}

func polecatReuseStatus(state polecat.State, cleanupStatus, activeMR, branch string, activeMRBlocks, staleCleanupSafe bool) string {
	facts := polecat.WorkstateFacts{State: state, CleanupStatus: polecat.CleanupStatus(cleanupStatus), ActiveMR: activeMR, Branch: branch, HookBeadSafe: true}
	if activeMRBlocks {
		facts.ActiveMRBlocker = "active_mr=" + activeMR + " status=open"
	}
	if staleCleanupSafe {
		// Caller has already established the CanIgnoreStaleCleanupStatus
		// predicates hold (workTerminal/hookSafe/activeMRSafe/gitSafe); a
		// terminal work ref is the only fact NewWorkstateInput needs from us
		// to reach the same conclusion through ResolveIgnoreCleanupStatus.
		facts.AssignedBeadTerminal = true
	}
	return polecat.DecideWorkstate(polecat.NewWorkstateInput(facts)).ReuseStatus
}

// getPolecatManager creates a polecat manager for the given rig.
func getPolecatManager(rigName string) (*polecat.Manager, *rig.Rig, error) {
	_, r, err := getRig(rigName)
	if err != nil {
		return nil, nil, err
	}

	polecatGit := git.NewGit(r.Path)
	t := tmux.NewTmux()
	mgr := polecat.NewManager(r, polecatGit, t)

	return mgr, r, nil
}

func runPolecatList(cmd *cobra.Command, args []string) error {
	var rigs []*rig.Rig

	if polecatListAll {
		// List all rigs
		allRigs, err := getAllRigs()
		if err != nil {
			return err
		}
		rigs = allRigs
	} else {
		// Need a rig name
		if len(args) < 1 {
			return fmt.Errorf("rig name required (or use --all)")
		}
		_, r, err := getPolecatManager(args[0])
		if err != nil {
			return err
		}
		rigs = []*rig.Rig{r}
	}

	// Collect polecats from all rigs
	sessions, err := loadPolecatSessionSet(newPoolSessionLister())
	if err != nil {
		return fmt.Errorf("listing tmux sessions: %w", err)
	}
	// Spawn grace: the window the Witness gives a dispatched session before it
	// would call it stalled (config.WitnessThresholds.HeartbeatStartupGrace,
	// default 5m). An unreadable town root falls back to the compiled-in
	// default rather than switching grace off — grace only ever delays the
	// stalled verdict (gt-yteq). One clock for the whole listing, so every
	// polecat is judged against the same instant.
	townRoot, _ := workspace.FindFromCwd()
	spawnWindow := polecatSpawnGraceWindow(townRoot)
	now := time.Now()
	// Every row of the output, in output order, with the inputs its build
	// needs. Assembled serially (the per-rig queries below are one per rig, not
	// one per seat) and resolved concurrently at the end.
	var seats []polecatSeat

	for _, r := range rigs {
		// The filesystem read and the session filter are the two cheap facts
		// that decide whether this rig has anything to report. Both are local
		// (a directory listing, an in-memory lookup), so they come first: the
		// queries below are the expensive part — five Dolt CLI round trips per
		// rig — and a rig with no polecat directory and no tmux session has no
		// row to build out of any of them (gt-8q0s).
		polecatNames, err := listPolecatDirectoryNames(r.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to list polecats in %s: %v\n", r.Name, err)
			continue
		}
		rigSessions := sessions.namesForRig(r.Name)
		if len(polecatNames) == 0 && len(rigSessions) == 0 {
			continue
		}

		bd := beads.New(r.Path)

		// ONE merge-request query per rig, joined per polecat below — never a
		// `bd show` per polecat (MRs are ephemeral wisps, and the per-id path
		// is the blindness this replaces).
		mrIndex, mrErr := loadPolecatMRIndex(bd, r.Name)
		if mrErr != nil {
			// The index is empty and every active_mr reads status=unknown
			// (fail-closed, as before), but say so — a silently unreadable
			// queue is how the original hardcoded "unknown" hid real state.
			fmt.Fprintf(os.Stderr, "warning: failed to list merge requests in %s: %v — MR status reported as unknown\n", r.Name, mrErr)
		}

		agents, agentErr := bd.ListAgentBeads()
		agentLookupFailed := agentErr != nil
		if agentLookupFailed {
			fmt.Fprintf(os.Stderr, "warning: failed to list agent beads in %s: %v — orphan sessions in this rig cannot be confirmed foreign, treating as zombie\n", r.Name, agentErr)
			agents = nil
		}
		activeWork, activeWorkErr := listActivePolecatWorkByName(bd, r.Name)
		if activeWorkErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to list active polecat work in %s: %v\n", r.Name, activeWorkErr)
			activeWork = nil
		}

		// Track known polecat names from filesystem for zombie detection. This
		// is exactly the set of worktree directories, so it is built here rather
		// than accumulated by the seat loop below — which now runs later, on the
		// probe pool.
		knownNames := make(map[string]bool, len(polecatNames))
		for _, name := range polecatNames {
			knownNames[name] = true
		}

		for _, name := range polecatNames {
			agentBeadID := polecatBeadIDForRig(r, r.Name, name)
			agentBead := agents[agentBeadID]
			fields := parsePolecatAgentFields(agentBead)
			// Grace is dated by the agent bead's own last write: a bead that
			// still says spawning and was touched inside the window is a
			// dispatch in flight, not a stall.
			env := polecatListInventoryEnv(r.Path, r.Name, name, mrIndex,
				polecatActiveMRReader{index: mrIndex, bd: bd},
				polecatSpawnFacts{
					UpdatedAt: polecat.AgentBeadUpdatedAt(agentBead),
					Grace:     spawnWindow,
					Now:       now,
				})
			seats = append(seats, polecatSeat{
				rigName:       r.Name,
				name:          name,
				fields:        fields,
				activeWork:    activeWork[name],
				activeWorkErr: activeWorkErr,
				sessions:      sessions,
				env:           env,
			})
		}

		// Discover zombie (and foreign) tmux sessions: sessions without matching
		// worktree directories. Genuine zombies occur when a worktree is deleted
		// but the tmux session persists (incomplete nuke or session naming
		// mismatch) — those still have an agent bead. A session with no worktree
		// AND no agent bead was never dispatched as a polecat at all (e.g. a
		// hermetic test's session escaping onto the wrong socket, gt-yav3);
		// report it as foreign, not zombie, so it never counts toward capacity
		// or gets restart/nuke treatment aimed at real polecats.
		for _, sessionName := range rigSessions {
			_, polecatName, ok := parsePolecatSessionName(sessionName)
			if !ok {
				continue
			}
			if knownNames[polecatName] {
				continue
			}
			agentBeadID := polecatBeadIDForRig(r, r.Name, polecatName)
			presence := beadAbsent
			switch {
			case agentLookupFailed:
				presence = beadLookupFailed
			default:
				if _, ok := agents[agentBeadID]; ok {
					presence = beadPresent
				}
			}
			// Classified here, not on the pool: an orphan session is decided
			// from session and bead presence alone, so it needs no probe. It
			// still takes its place in `seats` to keep the output order.
			orphan := classifyOrphanSession(r.Name, polecatName, sessionName, presence)
			seats = append(seats, polecatSeat{decided: &orphan})
		}
	}

	// One probe per seat, fanned out across a bounded pool. Order is preserved,
	// so this is the serial listing with its measuring overlapped.
	allPolecats := resolvePolecatSeats(seats)

	// Output
	if polecatListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(allPolecats)
	}

	if len(allPolecats) == 0 {
		fmt.Println("No polecats found.")
		return nil
	}

	fmt.Printf("%s\n\n", style.Bold.Render("Polecats"))
	for _, p := range allPolecats {
		// Session indicator
		sessionStatus := style.Dim.Render("○")
		if p.SessionRunning {
			sessionStatus = style.Success.Render("●")
		}

		// State color
		stateStr := string(p.State)
		switch p.State {
		case polecat.StateWorking:
			stateStr = style.Info.Render(stateStr)
		case polecat.StateSpawning:
			// Starting up, not broken: informational, like working.
			stateStr = style.Info.Render(stateStr)
		case polecat.StateStuck:
			stateStr = style.Warning.Render(stateStr)
		case polecat.StateStalled:
			stateStr = style.Error.Render(stateStr)
		case polecat.StateReviewNeeded:
			stateStr = style.Warning.Render(stateStr)
		case polecat.StateDone:
			stateStr = style.Success.Render(stateStr)
		case polecat.StateZombie:
			stateStr = style.Error.Render(stateStr)
		case polecat.StateForeign:
			stateStr = style.Dim.Render(stateStr)
		default:
			stateStr = style.Dim.Render(stateStr)
		}

		fmt.Printf("  %s %s/%s  %s\n", sessionStatus, p.Rig, p.Name, stateStr)
		if p.Issue != "" {
			fmt.Printf("    %s\n", style.Dim.Render(p.Issue))
		}
		if details := polecatAgentMRDetails(p); details != "" {
			fmt.Printf("    %s\n", style.Dim.Render(details))
		}
		if line := polecatReuseDetailLine(p); line != "" {
			fmt.Printf("    %s\n", style.Dim.Render(line))
		}
		if p.BeadLookupFailed && p.SessionName != "" {
			fmt.Printf("    %s\n", style.Warning.Render("session: "+p.SessionName+" (no worktree, bead lookup FAILED — unconfirmed zombie, do not treat as foreign)"))
		} else if p.Zombie && p.SessionName != "" {
			fmt.Printf("    %s\n", style.Dim.Render("session: "+p.SessionName+" (no worktree)"))
		} else if p.Foreign && p.SessionName != "" {
			fmt.Printf("    %s\n", style.Dim.Render("session: "+p.SessionName+" (no worktree, no bead — foreign session)"))
		}
	}

	return nil
}

// beadPresence is the tri-state result of an agent-bead lookup for an orphan
// tmux session. Collapsing "no bead" and "lookup failed" into a single bool
// (as an earlier version of this function did) meant one Dolt hiccup relabeled
// every genuine zombie as foreign and hid it from nuke/restart consideration —
// see the gt-yav3 MR1 bounce.
type beadPresence int

const (
	// beadAbsent means the lookup succeeded and found no bead: the session
	// never had one (e.g. a hermetic test's session escaping onto the wrong
	// socket) and is genuinely foreign.
	beadAbsent beadPresence = iota
	// beadPresent means the lookup succeeded and found a bead: the session
	// belongs to a real polecat whose worktree is gone — a genuine zombie.
	beadPresent
	// beadLookupFailed means the lookup itself errored (e.g. Dolt
	// unreachable): presence is unknown, so the caller must not assume
	// absence.
	beadLookupFailed
)

// classifyOrphanSession builds the list entry for a tmux session that parses
// as a polecat name in rigName but has no matching worktree directory.
// presence reports whether an agent bead exists for that name. A confirmed
// absence (beadAbsent) is foreign, never a zombie. A confirmed presence
// (beadPresent) is a genuine zombie. A failed lookup (beadLookupFailed) fails
// safe as an unconfirmed zombie — never silently downgraded to foreign, which
// would hide a real zombie — and is flagged loud via BeadLookupFailed so
// callers/output don't mistake it for a confirmed reading.
func classifyOrphanSession(rigName, polecatName, sessionName string, presence beadPresence) PolecatListItem {
	switch presence {
	case beadAbsent:
		return PolecatListItem{
			Rig:            rigName,
			Name:           polecatName,
			State:          polecat.StateForeign,
			SessionRunning: true,
			Foreign:        true,
			SessionName:    sessionName,
		}
	case beadLookupFailed:
		return PolecatListItem{
			Rig:              rigName,
			Name:             polecatName,
			State:            polecat.StateZombie,
			SessionRunning:   true,
			Zombie:           true,
			BeadLookupFailed: true,
			SessionName:      sessionName,
		}
	default: // beadPresent
		return PolecatListItem{
			Rig:            rigName,
			Name:           polecatName,
			State:          polecat.StateZombie,
			SessionRunning: true,
			Zombie:         true,
			SessionName:    sessionName,
		}
	}
}

func runPolecatAdd(cmd *cobra.Command, args []string) error {
	// Emit deprecation warning
	fmt.Fprintf(os.Stderr, "%s 'gt polecat add' is deprecated. Use 'gt polecat identity add' instead.\n",
		style.Warning.Render("Warning:"))
	fmt.Fprintf(os.Stderr, "         This command will be removed in v1.0.\n\n")

	rigName := args[0]
	polecatName := args[1]

	mgr, _, err := getPolecatManager(rigName)
	if err != nil {
		return err
	}

	fmt.Printf("Adding polecat %s to rig %s...\n", polecatName, rigName)

	p, err := mgr.Add(polecatName)
	if err != nil {
		return fmt.Errorf("adding polecat: %w", err)
	}

	fmt.Printf("%s Polecat %s added.\n", style.SuccessPrefix, p.Name)
	fmt.Printf("  %s\n", style.Dim.Render(p.ClonePath))
	fmt.Printf("  Branch: %s\n", style.Dim.Render(p.Branch))

	return nil
}

func runPolecatRemove(cmd *cobra.Command, args []string) error {
	targets, err := resolvePolecatTargets(args, polecatRemoveAll)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		fmt.Println("No polecats to remove.")
		return nil
	}

	// Remove each polecat
	t := tmux.NewTmux()
	var removeErrors []string
	removed := 0

	for _, p := range targets {
		// Check if session is running
		if !polecatForce {
			polecatMgr := polecat.NewSessionManager(t, p.r)
			running, _ := polecatMgr.IsRunning(p.polecatName)
			if running {
				removeErrors = append(removeErrors, fmt.Sprintf("%s/%s: session is running (stop first or use --force)", p.rigName, p.polecatName))
				continue
			}
		}

		fmt.Printf("Removing polecat %s/%s...\n", p.rigName, p.polecatName)

		if err := p.mgr.Remove(p.polecatName, polecatForce); err != nil {
			if errors.Is(err, polecat.ErrHasChanges) {
				removeErrors = append(removeErrors, fmt.Sprintf("%s/%s: has uncommitted changes (use --force)", p.rigName, p.polecatName))
			} else {
				removeErrors = append(removeErrors, fmt.Sprintf("%s/%s: %v", p.rigName, p.polecatName, err))
			}
			continue
		}

		fmt.Printf("  %s removed\n", style.Success.Render("✓"))
		removed++
	}

	// Report results
	if len(removeErrors) > 0 {
		fmt.Printf("\n%s Some removals failed:\n", style.Warning.Render("Warning:"))
		for _, e := range removeErrors {
			fmt.Printf("  - %s\n", e)
		}
	}

	if removed > 0 {
		fmt.Printf("\n%s Removed %d polecat(s).\n", style.SuccessPrefix, removed)
	}

	if len(removeErrors) > 0 {
		return fmt.Errorf("%d removal(s) failed", len(removeErrors))
	}

	return nil
}

// PolecatStatus represents detailed polecat status for JSON output.
type PolecatStatus struct {
	Rig            string        `json:"rig"`
	Name           string        `json:"name"`
	State          polecat.State `json:"state"`
	Issue          string        `json:"issue,omitempty"`
	ClonePath      string        `json:"clone_path"`
	Branch         string        `json:"branch"`
	SessionRunning bool          `json:"session_running"`
	SessionID      string        `json:"session_id,omitempty"`
	Attached       bool          `json:"attached,omitempty"`
	Windows        int           `json:"windows,omitempty"`
	CreatedAt      string        `json:"created_at,omitempty"`
	LastActivity   string        `json:"last_activity,omitempty"`
}

func runPolecatStatus(cmd *cobra.Command, args []string) error {
	rigName, polecatName, err := parseAddress(args[0])
	if err != nil {
		return err
	}

	mgr, r, err := getPolecatManager(rigName)
	if err != nil {
		return err
	}

	// Get polecat info
	p, err := mgr.Get(polecatName)
	if err != nil {
		return fmt.Errorf("polecat '%s' not found in rig '%s'", polecatName, rigName)
	}

	// Get session info
	t := tmux.NewTmux()
	polecatMgr := polecat.NewSessionManager(t, r)
	sessInfo, err := polecatMgr.Status(polecatName)
	if err != nil {
		// Non-fatal - continue without session info
		sessInfo = &polecat.SessionInfo{
			Polecat: polecatName,
			Running: false,
		}
	}

	// JSON output
	if polecatStatusJSON {
		status := PolecatStatus{
			Rig:            rigName,
			Name:           polecatName,
			State:          p.State,
			Issue:          p.Issue,
			ClonePath:      p.ClonePath,
			Branch:         p.Branch,
			SessionRunning: sessInfo.Running,
			SessionID:      sessInfo.SessionID,
			Attached:       sessInfo.Attached,
			Windows:        sessInfo.Windows,
		}
		if !sessInfo.Created.IsZero() {
			status.CreatedAt = sessInfo.Created.Format("2006-01-02 15:04:05")
		}
		if !sessInfo.LastActivity.IsZero() {
			status.LastActivity = sessInfo.LastActivity.Format("2006-01-02 15:04:05")
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	// Human-readable output
	fmt.Printf("%s\n\n", style.Bold.Render(fmt.Sprintf("Polecat: %s/%s", rigName, polecatName)))

	// State with color
	stateStr := string(p.State)
	switch p.State {
	case polecat.StateWorking:
		stateStr = style.Info.Render(stateStr)
	case polecat.StateStuck:
		stateStr = style.Warning.Render(stateStr)
	case polecat.StateStalled:
		stateStr = style.Error.Render(stateStr)
	case polecat.StateReviewNeeded:
		stateStr = style.Warning.Render(stateStr)
	case polecat.StateDone:
		stateStr = style.Success.Render(stateStr)
	default:
		stateStr = style.Dim.Render(stateStr)
	}
	fmt.Printf("  State:         %s\n", stateStr)

	// Issue
	if p.Issue != "" {
		fmt.Printf("  Issue:         %s\n", p.Issue)
	} else {
		fmt.Printf("  Issue:         %s\n", style.Dim.Render("(none)"))
	}

	// Clone path and branch
	fmt.Printf("  Clone:         %s\n", style.Dim.Render(p.ClonePath))
	fmt.Printf("  Branch:        %s\n", style.Dim.Render(p.Branch))

	// Session info
	fmt.Println()
	fmt.Printf("%s\n", style.Bold.Render("Session"))

	if sessInfo.Running {
		fmt.Printf("  Status:        %s\n", style.Success.Render("running"))
		fmt.Printf("  Session ID:    %s\n", style.Dim.Render(sessInfo.SessionID))

		if sessInfo.Attached {
			fmt.Printf("  Attached:      %s\n", style.Info.Render("yes"))
		} else {
			fmt.Printf("  Attached:      %s\n", style.Dim.Render("no"))
		}

		if sessInfo.Windows > 0 {
			fmt.Printf("  Windows:       %d\n", sessInfo.Windows)
		}

		if !sessInfo.Created.IsZero() {
			fmt.Printf("  Created:       %s\n", sessInfo.Created.Format("2006-01-02 15:04:05"))
		}

		if !sessInfo.LastActivity.IsZero() {
			// Show relative time for activity
			ago := formatActivityTime(sessInfo.LastActivity)
			fmt.Printf("  Last Activity: %s (%s)\n",
				sessInfo.LastActivity.Format("15:04:05"),
				style.Dim.Render(ago))
		}
	} else {
		fmt.Printf("  Status:        %s\n", style.Dim.Render("not running"))
	}

	return nil
}

// formatActivityTime returns a human-readable relative time string.
func formatActivityTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// GitState represents the git state of a polecat's worktree.
type GitState struct {
	Clean                    bool     `json:"clean"`
	UncommittedFiles         []string `json:"uncommitted_files"`
	UnpushedCommits          int      `json:"unpushed_commits"`
	ComparisonBase           string   `json:"comparison_base,omitempty"`
	UnpreservedPatchCount    int      `json:"unpreserved_patch_count"`
	StashCount               int      `json:"stash_count"`                  // Current-branch stashes: per-polecat risk.
	SharedStashCount         int      `json:"shared_stash_count,omitempty"` // Other branch stashes visible through the shared repo.
	PreservationCheckFailed  bool     `json:"preservation_check_failed,omitempty"`
	PreservationCheckFailure string   `json:"preservation_check_failure,omitempty"`
}

func runPolecatGitState(cmd *cobra.Command, args []string) error {
	rigName, polecatName, err := parseAddress(args[0])
	if err != nil {
		return err
	}

	mgr, r, err := getPolecatManager(rigName)
	if err != nil {
		return err
	}

	// Verify polecat exists
	p, err := mgr.Get(polecatName)
	if err != nil {
		return fmt.Errorf("polecat '%s' not found in rig '%s'", polecatName, rigName)
	}

	// Get git state from the polecat's worktree
	state, err := getGitState(p.ClonePath)
	if err != nil {
		return fmt.Errorf("getting git state: %w", err)
	}

	// JSON output
	if polecatGitStateJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(state)
	}

	// Human-readable output
	fmt.Printf("%s\n\n", style.Bold.Render(fmt.Sprintf("Git State: %s/%s", r.Name, polecatName)))

	// Working tree status
	if len(state.UncommittedFiles) == 0 {
		fmt.Printf("  Working Tree:  %s\n", style.Success.Render("clean"))
	} else {
		fmt.Printf("  Working Tree:  %s\n", style.Warning.Render("dirty"))
		fmt.Printf("  Uncommitted:   %s\n", style.Warning.Render(fmt.Sprintf("%d files", len(state.UncommittedFiles))))
		for _, f := range state.UncommittedFiles {
			fmt.Printf("                 %s\n", style.Dim.Render(f))
		}
	}

	// Unpushed commits
	if state.ComparisonBase != "" {
		fmt.Printf("  Comparison:   %s (%d unpreserved patch(es))\n", style.Dim.Render(state.ComparisonBase), state.UnpreservedPatchCount)
	}
	if state.UnpushedCommits == 0 {
		fmt.Printf("  Unpushed:      %s\n", style.Success.Render("0 commits"))
	} else {
		fmt.Printf("  Unpushed:      %s\n", style.Warning.Render(fmt.Sprintf("%d commits ahead", state.UnpushedCommits)))
	}

	// Stashes
	if state.StashCount == 0 {
		fmt.Printf("  Branch Stashes: %s\n", style.Dim.Render("0"))
	} else {
		fmt.Printf("  Branch Stashes: %s\n", style.Warning.Render(fmt.Sprintf("%d", state.StashCount)))
	}
	if state.SharedStashCount > 0 {
		fmt.Printf("  Shared Stashes: %s\n", style.Dim.Render(fmt.Sprintf("%d (repo-wide, not this branch)", state.SharedStashCount)))
	}

	// Verdict
	fmt.Println()
	if state.Clean {
		fmt.Printf("  Verdict:       %s\n", style.Success.Render("CLEAN (safe to kill)"))
	} else {
		fmt.Printf("  Verdict:       %s\n", style.Error.Render("DIRTY (needs cleanup)"))
	}

	return nil
}

// getGitState checks the git state of a worktree.
func getGitState(worktreePath string) (*GitState, error) {
	return getGitStateWithTargets(worktreePath, nil)
}

func getGitStateWithTargets(worktreePath string, targets []string) (*GitState, error) {
	state := &GitState{
		Clean:            true,
		UncommittedFiles: []string{},
	}

	worktreeGit := git.NewGit(worktreePath)
	workStatus, err := worktreeGit.CheckUncommittedWork()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	if workStatus.HasUncommittedChanges {
		// getGitStateWithTargets feeds check-recovery, checkPolecatSafety's
		// pre-nuke gate, and `gt polecat git-state`'s "safe to kill" verdict —
		// destructive/near-destructive consumers, not the reuse gate. The
		// index-skew relaxation (NonRuntimeNonSkewPaths,
		// CleanExcludingRuntimeAndIndexSkew) is scoped to reuse eligibility
		// only (see IndexSkewFiles' doc comment); this path stays on
		// NonRuntimePaths so a genuinely dirty seat still blocks here even if
		// gt-ui2x's reuse gate has cleared it (gt-ui2x om review).
		state.UncommittedFiles = workStatus.NonRuntimePaths()
		if len(state.UncommittedFiles) > 0 {
			state.Clean = false
		}
	}
	if workStatus.StashCount > 0 {
		state.StashCount = workStatus.StashCount
		state.Clean = false
	}

	branch, _ := worktreeGit.CurrentBranch()
	if preservation, preserveErr := worktreeGit.BranchPreservationStatus(branch, "origin", targets); preserveErr == nil {
		state.ComparisonBase = preservation.ComparisonBase
		state.UnpreservedPatchCount = preservation.UnpreservedPatchCount
		if preservation.UnpreservedPatchCount > 0 {
			state.UnpushedCommits = preservation.UnpreservedPatchCount
			state.Clean = false
		}
	} else {
		// gt-14a: an error here means we could not prove there are zero
		// unpushed commits — not that there are zero. Silently leaving
		// UnpushedCommits at 0 and state.Clean untouched previously let a
		// transient/unresolvable comparison-ref failure masquerade as a
		// clean worktree, reproducing the fail-open gt-7kr already fixed
		// in manager.go's workstateInputForPolecat. Fail closed instead.
		state.Clean = false
		state.PreservationCheckFailed = true
		state.PreservationCheckFailure = preserveErr.Error()
	}

	// Check for stashes using Git.StashCount() which filters by current branch.
	// Without branch filtering, worktrees see repo-wide stashes and produce
	// false "NEEDS_RECOVERY" verdicts for worktrees with zero stashes of their own.
	if totalStashes, stashErr := worktreeGit.StashCountAll(); stashErr == nil && totalStashes > state.StashCount {
		state.SharedStashCount = totalStashes - state.StashCount
	}

	return state, nil
}

// RecoveryStatus represents whether a polecat needs recovery or is safe to nuke.
type RecoveryStatus struct {
	Rig                  string                `json:"rig"`
	Polecat              string                `json:"polecat"`
	CleanupStatus        polecat.CleanupStatus `json:"cleanup_status"`
	NeedsRecovery        bool                  `json:"needs_recovery"`
	Verdict              string                `json:"verdict"` // SAFE_TO_NUKE, PENDING_MR, NEEDS_RECOVERY, or NEEDS_MQ_SUBMIT
	Reason               string                `json:"reason,omitempty"`
	Reusable             bool                  `json:"reusable"`
	SafeToNuke           bool                  `json:"safe_to_nuke"`
	NeedsMQSubmit        bool                  `json:"needs_mq_submit"`
	CountsTowardCapacity bool                  `json:"counts_toward_capacity"`
	ReuseStatus          string                `json:"reuse_status,omitempty"`
	Branch               string                `json:"branch,omitempty"`
	Issue                string                `json:"issue,omitempty"`
	MQStatus             string                `json:"mq_status,omitempty"` // "submitted", "not_submitted", "not_required", "unknown"
	ActiveMR             string                `json:"active_mr,omitempty"`
	Blockers             []string              `json:"blockers,omitempty"`
	Diagnostics          []string              `json:"diagnostics,omitempty"`
	RecoveryActions      []string              `json:"recovery_actions,omitempty"`
	Reconciled           bool                  `json:"reconciled,omitempty"`
	// CleanupStatusSource is always "recorded": cleanup_status is the polecat's
	// own self-report from gt done, never a live measurement. GitStateSource
	// says whether the git facts behind the verdict were measured ("live"),
	// failed to be measured ("unknown", with GitStateReason), or never measured
	// at all ("recorded" — the recorded hint stood in). See
	// polecat.RecordedCleanupBlocks.
	CleanupStatusSource string `json:"cleanup_status_source,omitempty"`
	GitStateSource      string `json:"git_state_source,omitempty"`
	GitStateReason      string `json:"git_state_reason,omitempty"`
}

func runPolecatCheckRecovery(cmd *cobra.Command, args []string) error {
	rigName, polecatName, err := parseAddress(args[0])
	if err != nil {
		return err
	}

	mgr, r, err := getPolecatManager(rigName)
	if err != nil {
		return err
	}

	// Verify polecat exists and get info
	p, err := mgr.Get(polecatName)
	if err != nil {
		return fmt.Errorf("polecat '%s' not found in rig '%s'", polecatName, rigName)
	}

	bd := beads.New(r.Path)
	status := checkRecoveryForPolecat(bd, r, rigName, polecatName, p, polecatCheckRecoveryReconcileCleanup)

	// JSON output
	if polecatCheckRecoveryJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	// Human-readable output
	renderCheckRecoveryText(os.Stdout, status)

	return nil
}

// runPolecatCheckRecoveryBatch answers check-recovery for every polecat in a
// rig within one process, sharing a single bulk agent-bead fetch and a single
// bulk merge-request fetch across all of them (gt-b839, follow-up to gt-ct3)
// instead of paying for both, per polecat, the way N independent
// 'gt polecat check-recovery' subprocess invocations would.
func runPolecatCheckRecoveryBatch(cmd *cobra.Command, args []string) error {
	rigName := args[0]

	mgr, r, err := getPolecatManager(rigName)
	if err != nil {
		return err
	}

	bd := beads.New(r.Path)
	// Each preload degrades independently into a warning: a failed bulk fetch
	// leaves that cache unwarmed, and the per-polecat helpers it feeds
	// (GetAgentBead, FindMRForBranchAny) fall back to their normal per-call bd
	// path for every polecat instead of silently reporting wrong data for all
	// of them — the same degrade-per-query pattern loadBeadsBatch uses for
	// runPolecatList (gt-ls4u).
	if err := bd.PreloadAgentBeads(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to preload agent beads in %s: %v — falling back to per-polecat bd show\n", rigName, err)
	}
	if err := bd.PreloadMergeRequests(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to preload merge requests in %s: %v — falling back to per-polecat bd list\n", rigName, err)
	}

	polecatNames, err := listPolecatDirectoryNames(r.Path)
	if err != nil {
		return fmt.Errorf("listing polecats in %s: %w", rigName, err)
	}

	statuses := make([]RecoveryStatus, 0, len(polecatNames))
	for _, polecatName := range polecatNames {
		p, err := mgr.Get(polecatName)
		if err != nil {
			// Vanished between the directory listing and mgr.Get (e.g. nuked
			// mid-sweep) — not this sweep's job to report on a polecat that no
			// longer exists.
			continue
		}
		statuses = append(statuses, checkRecoveryForPolecat(bd, r, rigName, polecatName, p, polecatCheckRecoveryBatchReconcile))
	}

	if polecatCheckRecoveryBatchJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(statuses)
	}

	for _, status := range statuses {
		renderCheckRecoveryText(os.Stdout, status)
		fmt.Fprintln(os.Stdout)
	}
	return nil
}

// checkRecoveryForPolecat computes one polecat's RecoveryStatus. It is the
// per-polecat body both runPolecatCheckRecovery (a single polecat) and
// runPolecatCheckRecoveryBatch (every polecat in a rig, sharing one bd
// instance and its preloaded caches — see PreloadAgentBeads/
// PreloadMergeRequests) drive; the two callers differ only in how many times
// they call it and whether bd's caches are warmed first.
func checkRecoveryForPolecat(bd *beads.Beads, r *rig.Rig, rigName, polecatName string, p *polecat.Polecat, reconcileCleanup bool) RecoveryStatus {
	// Get cleanup_status from agent bead
	// We need to read it directly from beads since manager doesn't expose it
	agentBeadID := polecatBeadIDForRig(r, rigName, polecatName)
	assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
	agentIssue, fields, err := bd.GetAgentBead(agentBeadID)

	status := RecoveryStatus{
		Rig:     rigName,
		Polecat: polecatName,
		Branch:  p.Branch,
		Issue:   p.Issue,
	}
	beadTerminal := isAssignedBeadTerminal(bd, status.Issue)
	// targetRefs is resolved once below, inside whichever branch actually
	// applies (no-agent-bead vs. agent-bead-found) — the agent-bead-found
	// branch has an extra sourceHint input, so resolving it here unconditionally
	// used to mean the agent-bead-found branch immediately discarded and
	// recomputed it, paying for recoveryTargetRefs' bd lookups (including a
	// FindMRForBranchAny call) twice per check-recovery invocation (gt-ct3).
	var targetRefs []string
	var targetRefLookupFailed bool
	var mrForBranch *beads.Issue
	var mrForBranchErr error
	// gt-2h6: a worktree directory that is structurally gone (not merely a
	// git command that failed) can never self-report a fresh CleanupStatus,
	// so a missing/unknown status here must not permanently veto recovery.
	// See ResolveIgnoreCleanupStatus for the full gating rationale.
	worktreeStructurallyMissing := polecat.IsStructuralWorktreeError(polecat.VerifyWorktreeExists(p.ClonePath))
	facts := polecat.WorkstateFacts{State: p.State, CleanupStatus: polecat.CleanupUnknown, Branch: p.Branch, HookBeadSafe: true, WorktreeStructurallyMissing: worktreeStructurallyMissing}
	var gitState *GitState
	var gitErr error
	gitStateLoaded := false
	loadGitState := func() {
		if gitStateLoaded {
			return
		}
		gitState, gitErr = getGitStateWithTargets(p.ClonePath, targetRefs)
		gitStateLoaded = true
	}

	if err != nil || fields == nil {
		targetRefs, targetRefLookupFailed, mrForBranch, mrForBranchErr = recoveryTargetRefs(bd, status.Issue, status.ActiveMR, status.Branch)
		// No agent bead reachable - fall back to a live git check.
		//
		// gt-14a: this branch used to derive a confident CleanupClean straight
		// from a narrow local git check ("no dirty files, no stash, no
		// unpushed commits" => clean) whenever the agent bead couldn't be
		// read. That is exactly the gitSafe-implies-clean promotion gt-7kr
		// (d32b9f5) removed from workstateInputForPolecat in manager.go — but
		// that fix never touched this CLI-only duplicate. An unreadable agent
		// bead means hook_bead, active_mr and push/mr failure flags are all
		// unverifiable here, not that they're absent; treating that as
		// "clean" reintroduced the fail-open for this code path specifically.
		// facts.CleanupStatus already defaults to CleanupUnknown, which
		// NewWorkstateInput/DecideWorkstate correctly fail closed on, so
		// leave it there and only use live git facts as supplementary
		// evidence, same as the agent-bead-found branch below does via
		// applyGitStateToWorkstateFacts.
		//
		// Still honor mgr.Get's canonical hooked-issue detection even without
		// an agent bead to read hook_bead from — a missing agent bead must
		// not silently drop the hook-bead safety signal either. Treat any
		// resolved hook bead as unconditionally unsafe here (rather than
		// looking it up via hookBeadSafeForCleanup): the agent bead is
		// already unreadable, so a bd lookup used to decide "safe" would be
		// trusting the same unreliable data source this branch exists to
		// distrust.
		if hookBead := recoveryHookBead(bd, assignee, nil, nil, p); hookBead != "" {
			facts.HookBead = hookBead
			facts.HookBeadSafe = false
		}
		// Read straight off p.Issue, so it stands even when the agent bead
		// could not be read (gt-pldt).
		facts.AssignedBeadTerminal = beadTerminal
		loadGitState()
		applyGitStateToWorkstateFacts(&facts, p.ClonePath, gitState, gitErr)
	} else {
		// Use cleanup_status from agent bead, then overlay direct git and MQ facts.
		// gt-ui2x: the bead was actually read here (fields != nil), unlike the
		// no-agent-bead branch above — hook_bead/push_failed/mr_failed/active_mr
		// below all came from this same read, so they're verified facts, not
		// unread defaults. That is the precondition ResolveIgnoreCleanupStatus's
		// agentBeadRead branch requires before a missing/unknown cleanup_status
		// can clear on a live-clean probe.
		facts.AgentBeadRead = true
		facts.CleanupStatus = polecat.CleanupStatus(fields.CleanupStatus)
		status.ActiveMR = fields.ActiveMR
		facts.ActiveMR = fields.ActiveMR
		hookBead := recoveryHookBead(bd, assignee, agentIssue, fields, p)
		hookSafe, hookTerminal, _ := hookBeadSafeForCleanup(bd, hookBead)
		sourceHint := agentSourceIssueHint(status.Issue, fields)
		targetRefs, targetRefLookupFailed, mrForBranch, mrForBranchErr = recoveryTargetRefs(bd, status.Issue, status.ActiveMR, status.Branch, sourceHint)
		if status.Issue == "" && sourceHint != "" {
			status.Issue = sourceHint
		}
		if !beadTerminal && sourceHint != "" {
			beadTerminal = isAssignedBeadTerminal(bd, sourceHint)
		}
		facts.HookBead = hookBead
		facts.HookBeadSafe = hookSafe
		facts.HookBeadTerminal = hookTerminal
		facts.PushFailed = fields.PushFailed
		facts.MRFailed = fields.MRFailed
		partialSpawn, diagnostic := partialSpawnWithoutDurableHook(bd, fields, assignee, status.Issue)
		if diagnostic != "" {
			status.Diagnostics = append(status.Diagnostics, diagnostic)
		}
		activeMRAssessment := polecat.ActiveMRAssessment{}
		if fields.ActiveMR != "" {
			gitSafe := activeMRGitSafeForWorktree(p.ClonePath)
			// hq-kr3hm: a dangling active_mr used to report PENDING_MR forever,
			// telling an agent to PRESERVE a polecat whose work already landed.
			// The landed probe is what separates "gone and finished" from
			// "gone and still at risk" — see AssessActiveMRWithLandedEvidence.
			activeMRAssessment = polecat.AssessActiveMRWithLandedEvidence(bd, polecat.ActiveMRInput{
				ActiveMR:        fields.ActiveMR,
				SourceIssueHint: sourceHint,
				RequireGitSafe:  true,
				GitSafe:         gitSafe,
			}, func() polecat.LandedEvidence {
				return polecat.ProbeWorkLandedOnRef(p.ClonePath, status.Branch, "origin")
			})
			if status.Issue == "" && activeMRAssessment.SourceIssue != "" {
				status.Issue = activeMRAssessment.SourceIssue
			}
			if activeMRAssessment.SourceTerminal {
				facts.ActiveMRSourceTerminal = true
			}
			if activeMRAssessment.Pending {
				facts.ActiveMRBlocker = activeMRAssessment.Reason
			}
		}
		facts.PartialSpawnWithoutDurableHook = partialSpawn
		// Settled here, before the ignore-gate diagnostic below reads
		// facts.WorkTerminal(), so that diagnostic and the classifier agree;
		// the MQ verdict reads this one fact as submission evidence, so a
		// terminal hook bead or active-MR source is not folded in (gt-pldt).
		facts.AssignedBeadTerminal = beadTerminal
		// gt-hsg: cleanup-status ignoring for BOTH the "partial spawn never
		// durably hooked" case and the "stale dirty status, live facts prove
		// safe" case used to be decided here, ad hoc, with the partial-spawn
		// branch setting IgnoreCleanupStatus unconditionally — never checking
		// hookSafe/activeMRSafe/gitSafe at all. That is precisely the
		// ungated fail-open promotion gt-7kr removed from manager.go,
		// reintroduced here under a different precondition. Load git state
		// unconditionally and let NewWorkstateInput's single, shared
		// ResolveIgnoreCleanupStatus gate decide from the full fact set
		// instead of duplicating (and drifting from) that policy here.
		loadGitState()
		applyGitStateToWorkstateFacts(&facts, p.ClonePath, gitState, gitErr)
		directGitSafe := !facts.GitCheckFailed && !facts.GitDirty && facts.StashCount == 0 && facts.UnpushedCommits == 0
		liveGitProbeRan := facts.GitStateSource == polecat.GitStateSourceLive
		if !facts.CleanupStatus.IsSafe() {
			// claude-41j.1 D9 splits this diagnostic in two, because the record
			// can now be ignored for either of two reasons: a live git probe
			// answered and superseded a git-derived status, or the narrow
			// partial-spawn/gone-worktree/agent-bead-read hatches resolved a
			// missing one.
			switch {
			case !polecat.RecordedCleanupBlocks(facts.CleanupStatus, facts.GitStateSource):
				status.Diagnostics = append(status.Diagnostics, fmt.Sprintf("ignored_cleanup_status=%s cleanup_status_source=%s git_state_source=%s live_git_supersedes=recorded", facts.CleanupStatus, polecat.CleanupStatusSourceRecorded, facts.GitStateSource))
			case polecat.ResolveIgnoreCleanupStatus(facts.CleanupStatus, partialSpawn, worktreeStructurallyMissing, facts.AgentBeadRead, liveGitProbeRan, facts.WorkTerminal(), hookSafe, !activeMRAssessment.Pending, directGitSafe):
				status.Diagnostics = append(status.Diagnostics, fmt.Sprintf("ignored_cleanup_status=%s partial_spawn=%v worktree_missing=%v agent_bead_read=%v direct_git_state=safe work_ref=terminal", facts.CleanupStatus, partialSpawn, worktreeStructurallyMissing, facts.AgentBeadRead))
			}
		}
	}
	input := polecat.NewWorkstateInput(facts)

	status.CleanupStatus = input.CleanupStatus
	applyMQFactsToWorkstateInput(&input, &status, bd, p.ClonePath, targetRefs, targetRefLookupFailed, gitState, gitErr, mrForBranch, mrForBranchErr)
	disposition := polecat.DecideWorkstate(input)
	applyWorkstateDispositionToRecoveryStatus(&status, disposition)

	if reconcileCleanup {
		reconcileCleanupStatusIfSafe(&status, bd, agentBeadID, p, fields)
	}

	return status
}

// renderCheckRecoveryText writes the human-readable check-recovery report for
// status. It must switch on exactly the same status.Verdict value the JSON
// encoder marshals (see runPolecatCheckRecovery) — gt-u4x was a live divergence
// where this switch had no case for WorkstateVerdictWorking and silently fell
// into a "default means SAFE_TO_NUKE" branch, printing a false clearance for
// an actively working polecat while --json on the same struct, same instant,
// correctly reported verdict=WORKING/safe_to_nuke=false. Any verdict without
// an explicit case below must fail closed (NOT render as SAFE_TO_NUKE) so a
// future new verdict can't silently reintroduce that split.
func renderCheckRecoveryText(w io.Writer, status RecoveryStatus) {
	fmt.Fprintf(w, "%s\n\n", style.Bold.Render(fmt.Sprintf("Recovery Status: %s/%s", status.Rig, status.Polecat)))
	cleanup := string(status.CleanupStatus)
	if status.CleanupStatusSource != "" {
		// The recorded hint is labeled where a human reads it, not only in
		// the JSON: an unlabeled cleanup_status reads like live state.
		cleanup += " (" + status.CleanupStatusSource + ")"
	}
	if status.GitStateSource != "" {
		cleanup += " git=" + status.GitStateSource
		if status.GitStateReason != "" {
			cleanup += " (" + status.GitStateReason + ")"
		}
	}
	fmt.Fprintf(w, "  Cleanup Status:  %s\n", cleanup)
	if status.Branch != "" {
		fmt.Fprintf(w, "  Branch:          %s\n", status.Branch)
	}
	if status.Issue != "" {
		fmt.Fprintf(w, "  Issue:           %s\n", status.Issue)
	}
	if status.ActiveMR != "" {
		fmt.Fprintf(w, "  Active MR:       %s\n", status.ActiveMR)
	}
	if len(status.Diagnostics) > 0 {
		fmt.Fprintf(w, "  Diagnostics:     %s\n", strings.Join(status.Diagnostics, "; "))
	}
	fmt.Fprintln(w)

	switch status.Verdict {
	case polecat.WorkstateVerdictNeedsMQSubmit:
		fmt.Fprintf(w, "  Verdict:         %s\n", style.Warning.Render("NEEDS_MQ_SUBMIT"))
		fmt.Fprintf(w, "  MQ Status:       %s\n", status.MQStatus)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s Work is pushed but was never submitted to the merge queue.\n", style.Warning.Render("⚠"))
		fmt.Fprintln(w, "  Submit to MQ before cleanup, or the branch will be orphaned.")
	case polecat.WorkstateVerdictPendingMR:
		fmt.Fprintf(w, "  Verdict:         %s\n", style.Warning.Render("PENDING_MR"))
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Work is waiting on an active merge request; preserve this polecat until it lands.")
	case polecat.WorkstateVerdictNeedsRecovery:
		fmt.Fprintf(w, "  Verdict:         %s\n", style.Error.Render("NEEDS_RECOVERY"))
		if status.Reason != "" {
			// The predicate family the classifier used, so the text names the
			// same thing --json's reason field does (gt-3r1h).
			fmt.Fprintf(w, "  Reason:          %s\n", status.Reason)
		}
		fmt.Fprintln(w)
		if len(status.Blockers) > 0 {
			fmt.Fprintf(w, "  %s Cleanup refused by these predicate(s):\n", style.Warning.Render("⚠"))
			for _, blocker := range status.Blockers {
				fmt.Fprintf(w, "    - %s\n", blocker)
			}
			if len(status.RecoveryActions) > 0 {
				fmt.Fprintln(w)
				fmt.Fprintln(w, "  Recovery action(s):")
				for _, action := range status.RecoveryActions {
					fmt.Fprintf(w, "    - %s\n", action)
				}
			}
		} else {
			// DecideWorkstate names a blocker for every refusal
			// (WorkstateDisposition.Blockers), so a status that reaches here
			// was assembled without it. Report that gap concretely instead of
			// the former "refused by an unknown recovery predicate", which
			// told the reader nothing and forced an escalation every time
			// (gt-3r1h).
			fmt.Fprintf(w, "  %s Cleanup refused, but this status names no blocker (reason=%q).\n", style.Warning.Render("⚠"), status.Reason)
			fmt.Fprintln(w, "  DecideWorkstate names a blocker for every refusal, so this status did not come from it. Report the gap before acting.")
		}
		fmt.Fprintln(w, "  Escalate to Mayor for recovery before cleanup.")
	case polecat.WorkstateVerdictWorking:
		fmt.Fprintf(w, "  Verdict:         %s\n", style.Error.Render("WORKING"))
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s Polecat is actively working; NOT safe to nuke.\n", style.Warning.Render("⚠"))
		if len(status.Blockers) > 0 {
			fmt.Fprintln(w, "  Blocker(s):")
			for _, blocker := range status.Blockers {
				fmt.Fprintf(w, "    - %s\n", blocker)
			}
		}
	case polecat.WorkstateVerdictSafeToNuke:
		fmt.Fprintf(w, "  Verdict:         %s\n", style.Success.Render("SAFE_TO_NUKE"))
		if status.MQStatus != "" {
			fmt.Fprintf(w, "  MQ Status:       %s\n", status.MQStatus)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s Safe to nuke - no work at risk.\n", style.Success.Render("✓"))
	default:
		fmt.Fprintf(w, "  Verdict:         %s\n", style.Error.Render(status.Verdict))
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s Unrecognized verdict %q — treating as NOT safe to nuke. Escalate to Mayor.\n", style.Warning.Render("⚠"), status.Verdict)
	}
}

func applyGitStateToWorkstateFacts(facts *polecat.WorkstateFacts, worktreePath string, gitState *GitState, gitErr error) {
	// claude-41j.1 D9: the git facts applied here are a live measurement, and
	// the verdict now re-derives from them — the recorded cleanup_status is
	// demoted to a hint (polecat.RecordedCleanupBlocks). Label the provenance
	// before any early return, including the clean case: "the probe ran and
	// found nothing" is exactly the answer that supersedes a stale record.
	facts.GitStateSource = polecat.GitStateSourceLive
	if gitErr != nil {
		facts.GitCheckFailed = true
		facts.GitCheckFailedReason = recoveryGitStateBlocker(worktreePath, gitState, gitErr)
		facts.GitStateSource = polecat.GitStateSourceUnknown
		return
	}
	if gitState != nil && gitState.PreservationCheckFailed {
		facts.GitCheckFailed = true
		facts.GitCheckFailedReason = fmt.Sprintf("git_state=unknown path=%s: unpushed-commit check failed: %s", worktreePath, gitState.PreservationCheckFailure)
		facts.GitStateSource = polecat.GitStateSourceUnknown
		return
	}
	if gitState == nil || gitState.Clean {
		return
	}
	if gitState.UnpushedCommits > 0 {
		facts.UnpushedCommits = gitState.UnpushedCommits
	}
	if gitState.StashCount > 0 {
		facts.StashCount = gitState.StashCount
	}
	if len(gitState.UncommittedFiles) > 0 {
		facts.GitDirty = true
		facts.GitDirtyReason = fmt.Sprintf("git_state=has_uncommitted uncommitted_files=%d", len(gitState.UncommittedFiles))
	}
}

// mrForBranch/mrForBranchErr is the FindMRForBranchAny(status.Branch) result
// the caller already resolved via recoveryTargetRefs. FindMRForBranchAny scans
// every gt:merge-request bead in the rig's Dolt db, so reusing it here instead
// of querying again saves a second full scan on every check-recovery call for
// a branch with submittable work (gt-ct3).
//
// bd is taken as issueShower rather than *beads.Beads because that is all this
// needs (one Show call): a nil *beads.Beads converts to a NON-nil interface and
// then reaches isMQNotRequiredSource's nil guard as a live pointer, so the
// guard passes and Show is called on a nil receiver. Narrowing the parameter to
// the interface keeps that guard meaningful and lets tests inject a fake.
//
// The assigned-bead terminality is read off the input rather than passed in as
// its own argument: it is a fact about the bead, and a caller-free parameter
// slot is somewhere a wider notion of "terminal work ref" can be handed in
// unnoticed — that is exactly how the MQ verdict widened to "submitted" for a
// terminal hook bead (gt-pldt).
func applyMQFactsToWorkstateInput(input *polecat.WorkstateInput, status *RecoveryStatus, bd issueShower, worktreePath string, targetRefs []string, targetRefLookupFailed bool, gitState *GitState, gitErr error, mrForBranch *beads.Issue, mrForBranchErr error) {
	if status.Branch == "" {
		return
	}
	input.MQCheckRequired = true
	input.HasSubmittableWork = hasSubmittableWorkForRecovery(worktreePath, targetRefs, gitState, gitErr)
	input.MQNotRequired = isMQNotRequiredSource(bd, status.Issue)
	if targetRefLookupFailed {
		input.MQLookupFailed = true
	}
	if !input.HasSubmittableWork || input.MQNotRequired || input.AssignedBeadTerminal {
		return
	}
	if mrForBranchErr != nil {
		input.MQLookupFailed = true
		return
	}
	input.MRSubmitted = mrForBranch != nil
}

func applyWorkstateDispositionToRecoveryStatus(status *RecoveryStatus, disposition polecat.WorkstateDisposition) {
	status.Verdict = disposition.Verdict
	status.Reason = disposition.Reason
	status.Reusable = disposition.Reusable
	status.SafeToNuke = disposition.SafeToNuke
	status.NeedsRecovery = disposition.NeedsRecovery
	status.NeedsMQSubmit = disposition.NeedsMQSubmit
	status.CountsTowardCapacity = disposition.CountsTowardCapacity
	status.ReuseStatus = disposition.ReuseStatus
	status.MQStatus = disposition.MQStatus
	status.Blockers = disposition.Blockers
	// Provenance travels with the verdict everywhere it is reported, so a
	// consumer of check-recovery output can tell the recorded cleanup hint from
	// the live git measurement behind a verdict (claude-41j.1 D9).
	status.CleanupStatusSource = disposition.CleanupStatusSource
	status.GitStateSource = disposition.GitStateSource
	status.GitStateReason = disposition.GitStateReason
	status.RecoveryActions = recoveryActionsForBlockers(disposition.Blockers)
}

type issueShower interface {
	Show(issueID string) (*beads.Issue, error)
}

func cleanupStatusBlocker(status polecat.CleanupStatus) string {
	switch status {
	case polecat.CleanupClean:
		return ""
	case "":
		return "cleanup_status=<missing>"
	case polecat.CleanupUnknown:
		return "cleanup_status=unknown"
	default:
		return fmt.Sprintf("cleanup_status=%s", status)
	}
}

func cleanupStatusBlockerForRecovery(status polecat.CleanupStatus, partialSpawnWithoutHook bool) string {
	if partialSpawnWithoutHook && (status == "" || status == polecat.CleanupUnknown) {
		return ""
	}
	return cleanupStatusBlocker(status)
}

func agentHookBead(agentIssue *beads.Issue, fields *beads.AgentFields) string {
	if agentIssue != nil && agentIssue.HookBead != "" {
		return agentIssue.HookBead
	}
	if fields != nil {
		return fields.HookBead
	}
	return ""
}

// assignedIssueLookup is the direct-tracking-model query (status=hooked,
// assignee=<polecat>, plus the open/in_progress statuses it also covers)
// that manager.go's loadFromBeads treats as the primary source of truth for
// whether a polecat is actively holding work.
type assignedIssueLookup interface {
	GetAssignedIssue(assignee string) (*beads.Issue, error)
}

// recoveryHookBead resolves the hook-bead safety signal for check-recovery.
//
// gt-14a: agentHookBead only reads the legacy agent-bead hook_bead field,
// which the direct-tracking model (hq-l6mm5, see loadFromBeads in
// manager.go) no longer keeps populated for every polecat. Relying on it
// alone let a genuinely hooked, actively working polecat (status=hooked
// work bead, live session) read as unhooked here while mgr.Get's primary
// tier still saw it correctly — the hook-bead safety signal must not depend
// on a field that can be legitimately empty under the current dispatch
// model. Fall back to the polecat's own canonically-detected issue whenever
// mgr.Get classified it as actively working.
//
// gt-ido: those two signals both bottom out empty exactly when mgr.Get's own
// State/Issue read (p) is itself the thing that's stale or racing the
// direct-tracking bead update — the legacy field and the "trust p" fallback
// can agree on "" while a bead assigned to this polecat is still open/hooked.
// Re-derive the same direct-tracking fact manager.go's primary tier uses,
// independently, as a last-resort check before concluding there is truly no
// hook: it is the one signal here that isn't sourced from p.
func recoveryHookBead(bd assignedIssueLookup, assignee string, agentIssue *beads.Issue, fields *beads.AgentFields, p *polecat.Polecat) string {
	if hookBead := agentHookBead(agentIssue, fields); hookBead != "" {
		return hookBead
	}
	if p != nil && p.State == polecat.StateWorking && p.Issue != "" {
		return p.Issue
	}
	if bd != nil && assignee != "" {
		if issue, err := bd.GetAssignedIssue(assignee); err == nil && issue != nil {
			return issue.ID
		}
	}
	return ""
}

func activeMRGitSafeForWorktree(worktreePath string) bool {
	g := git.NewGit(worktreePath)
	branch, err := g.CurrentBranch()
	if err != nil || branch == "" {
		return false
	}
	status, err := g.CheckUncommittedWork()
	if err != nil || !status.CleanExcludingRuntime() || status.StashCount > 0 || status.UnpushedCommits > 0 {
		return false
	}
	pushed, unpushed, err := g.BranchPushedToRemote(branch, "origin")
	if err != nil {
		return false
	}
	return pushed && unpushed == 0
}

func hookBeadSafeForCleanup(bd issueShower, hookBead string) (safe bool, terminal bool, blocker string) { //nolint:unparam // blocker is diagnostic output for tests/logging; callers discard it today
	if hookBead == "" {
		return true, false, ""
	}
	if bd == nil {
		return false, false, fmt.Sprintf("hook_bead=%s status=unverified", hookBead)
	}
	issue, err := bd.Show(hookBead)
	if err != nil {
		return false, false, fmt.Sprintf("hook_bead=%s status=lookup_error: %v", hookBead, err)
	}
	if issue == nil {
		return false, false, fmt.Sprintf("hook_bead=%s status=missing", hookBead)
	}
	if !beads.IssueStatus(issue.Status).IsTerminal() {
		return false, false, fmt.Sprintf("hook_bead=%s status=%s", hookBead, issue.Status)
	}
	return true, true, ""
}

type cleanupStatusUpdater interface {
	UpdateAgentCleanupStatus(id string, cleanupStatus string) error
}

func reconcileCleanupStatusIfSafe(status *RecoveryStatus, updater cleanupStatusUpdater, agentBeadID string, p *polecat.Polecat, fields *beads.AgentFields) {
	previous, ok := cleanupStatusReconcileCandidate(status, p, fields)
	if !ok {
		return
	}
	if updater == nil {
		status.NeedsRecovery = true
		status.Verdict = "NEEDS_RECOVERY"
		status.Blockers = append(status.Blockers, "cleanup_reconcile_failed: updater unavailable")
		return
	}
	if err := updater.UpdateAgentCleanupStatus(agentBeadID, string(polecat.CleanupClean)); err != nil {
		status.NeedsRecovery = true
		status.Verdict = "NEEDS_RECOVERY"
		status.Blockers = append(status.Blockers, fmt.Sprintf("cleanup_reconcile_failed: %v", err))
		return
	}
	status.CleanupStatus = polecat.CleanupClean
	status.Reconciled = true
	status.Diagnostics = append(status.Diagnostics, fmt.Sprintf("reconciled_cleanup_status=clean previous=%s", previous))
}

func cleanupStatusReconcileCandidate(status *RecoveryStatus, p *polecat.Polecat, fields *beads.AgentFields) (polecat.CleanupStatus, bool) {
	if status == nil || p == nil || fields == nil {
		return "", false
	}
	previous := polecat.CleanupStatus(fields.CleanupStatus)
	if previous == "" || previous == polecat.CleanupClean {
		return previous, false
	}
	if p.State != polecat.StateIdle || beads.AgentState(fields.AgentState) != beads.AgentStateIdle {
		return previous, false
	}
	if status.NeedsRecovery || status.Verdict != "SAFE_TO_NUKE" {
		return previous, false
	}
	if status.Branch != "" && status.MQStatus != "submitted" && status.MQStatus != "not_required" {
		return previous, false
	}
	return previous, true
}

func agentSourceIssueHint(currentIssue string, fields *beads.AgentFields) string {
	if currentIssue != "" {
		return currentIssue
	}
	if fields == nil {
		return ""
	}
	if fields.LastSourceIssue != "" {
		return fields.LastSourceIssue
	}
	return fields.HookBead
}

func partialSpawnWithoutDurableHook(bd issueShower, fields *beads.AgentFields, assignee, currentIssue string) (bool, string) {
	if bd == nil || fields == nil || fields.AgentState != "spawning" || fields.HookBead == "" || currentIssue != "" {
		return false, ""
	}
	issue, err := bd.Show(fields.HookBead)
	if err != nil || issue == nil {
		return false, ""
	}
	if (issue.Status == beads.StatusHooked && issue.Assignee == assignee) || issue.Assignee == assignee {
		return false, ""
	}
	return true, fmt.Sprintf("partial_spawn_without_durable_hook agent_state=%s hook_bead=%s hook_status=%s hook_assignee=%q", fields.AgentState, fields.HookBead, issue.Status, issue.Assignee)
}

func recoveryGitStateBlocker(worktreePath string, gitState *GitState, gitErr error) string {
	if gitErr != nil {
		return fmt.Sprintf("git_state=unknown path=%s: %v", worktreePath, gitErr)
	}
	if gitState == nil || gitState.Clean {
		return ""
	}
	if gitState.UnpushedCommits > 0 {
		return fmt.Sprintf("git_state=has_unpushed unpushed_commits=%d", gitState.UnpushedCommits)
	}
	if gitState.StashCount > 0 {
		return fmt.Sprintf("git_state=has_stash stash_count=%d", gitState.StashCount)
	}
	return fmt.Sprintf("git_state=has_uncommitted uncommitted_files=%d", len(gitState.UncommittedFiles))
}

func recoveryActionsForBlockers(blockers []string) []string {
	for _, blocker := range blockers {
		if strings.HasPrefix(blocker, "git_state=has_stash") {
			return []string{"preserve branch-owned stash entries to auditable recovery refs before cleanup, then rerun check-recovery"}
		}
	}
	return nil
}

func activeMRBlocker(bd issueShower, mrID, sourceHint string, requireGitSafe, gitSafe bool) string {
	assessment := polecat.AssessActiveMR(bd, polecat.ActiveMRInput{
		ActiveMR:        mrID,
		SourceIssueHint: sourceHint,
		RequireGitSafe:  requireGitSafe,
		GitSafe:         gitSafe,
	})
	if assessment.Pending {
		return assessment.Reason
	}
	return ""
}

func hasSubmittableWorkForRecovery(worktreePath string, targetRefs []string, gitState *GitState, gitErr error) bool {
	g := git.NewGit(worktreePath)
	branch, _ := g.CurrentBranch()
	if status, err := g.BranchTargetStatus(branch, "origin", targetRefs); err == nil {
		return status.UnpreservedPatchCount > 0
	}
	if branch, err := g.CurrentBranch(); err == nil && branch != "" && !isRecoveryBaseBranch(branch) {
		if pushed, _, err := g.BranchPushedToRemote(branch, "origin"); err == nil && pushed {
			return true
		}
	}
	return gitErr != nil || (gitState != nil && (gitState.UnpushedCommits > 0 || gitState.PreservationCheckFailed))
}

func isRecoveryBaseBranch(branch string) bool {
	return branch == "main" || branch == "master" || strings.HasPrefix(branch, "integration/")
}

// recoveryTargetRefs also returns the MR bead it found (if any) for branch,
// via FindMRForBranchAny, so callers that separately need "is there an MR
// for this branch" (e.g. applyMQFactsToWorkstateInput's submitted check) can
// reuse it instead of re-running the same lookup — FindMRForBranchAny scans
// every gt:merge-request bead in the rig's Dolt db, so repeating it per
// check-recovery invocation was a real, measured cost (gt-ct3).
func recoveryTargetRefs(bd *beads.Beads, issueID, activeMR, branch string, extraIssueIDs ...string) (refs []string, lookupFailed bool, mrForBranch *beads.Issue, mrForBranchErr error) {
	appendMRTarget := func(issue *beads.Issue) {
		if fields := beads.ParseMRFields(issue); fields != nil && fields.Target != "" {
			refs = append(refs, fields.Target)
		}
	}
	if bd != nil {
		if activeMR != "" {
			if issue, err := bd.Show(activeMR); err == nil {
				appendMRTarget(issue)
			} else if !errors.Is(err, beads.ErrNotFound) {
				lookupFailed = true
			}
		}
		if branch != "" {
			mrForBranch, mrForBranchErr = bd.FindMRForBranchAny(branch)
			if mrForBranchErr == nil {
				appendMRTarget(mrForBranch)
			} else if !errors.Is(mrForBranchErr, beads.ErrNotFound) {
				lookupFailed = true
			}
		}
		// sourceHint (the caller's extraIssueIDs) is frequently identical to
		// issueID (agentSourceIssueHint returns status.Issue verbatim whenever
		// it's non-empty) — dedupe before showing, or the same bead gets a
		// second bd subprocess call for no new information (gt-ct3).
		for _, candidateIssueID := range uniqueStrings(append([]string{issueID}, extraIssueIDs...)) {
			if candidateIssueID == "" {
				continue
			}
			if issue, err := bd.Show(candidateIssueID); err == nil {
				appendAttachmentTargets(&refs, bd, issue)
			} else {
				lookupFailed = true
			}
		}
	}
	return uniqueStrings(refs), lookupFailed, mrForBranch, mrForBranchErr
}

func appendAttachmentTargets(refs *[]string, bd *beads.Beads, issue *beads.Issue) {
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil {
		return
	}
	appendBaseBranchVars(refs, attachment.FormulaVars)
	for _, value := range attachment.AttachedVars {
		appendBaseBranchVars(refs, value)
	}
	if attachment.ConvoyID != "" && bd != nil {
		if convoy, err := bd.Show(attachment.ConvoyID); err == nil {
			if fields := beads.ParseConvoyFields(convoy); fields != nil && fields.BaseBranch != "" {
				*refs = append(*refs, fields.BaseBranch)
			}
		}
	}
}

func appendBaseBranchVars(refs *[]string, vars string) {
	for _, line := range strings.Split(vars, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "base_branch" {
			continue
		}
		if value = strings.TrimSpace(value); value != "" {
			*refs = append(*refs, value)
		}
	}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// mrFinder is the subset of *beads.Beads that applyMQCheck needs. It lets us
// unit-test the verdict logic without a real bd binary.
type mrFinder interface {
	FindMRForBranchAny(branch string) (*beads.Issue, error)
}

// isAssignedBeadTerminal reports whether the polecat's assigned bead (if any)
// is in a terminal status (closed/tombstone). Returns false on any lookup
// failure — callers must only use this to *skip* further escalation, never to
// escalate, so a false negative is safe.
func isAssignedBeadTerminal(bd *beads.Beads, issueID string) bool {
	if issueID == "" || bd == nil {
		return false
	}
	issue, err := bd.Show(issueID)
	if err != nil || issue == nil {
		return false
	}
	return beads.IssueStatus(issue.Status).IsTerminal()
}

// isMQNotRequiredSource reports whether the source bead intentionally bypasses
// the internal merge queue. The caller still gates this on SAFE_TO_NUKE so dirty
// or unpushed local work is never hidden by source metadata.
func isMQNotRequiredSource(bd issueShower, issueID string) bool {
	if issueID == "" || bd == nil {
		return false
	}
	issue, err := bd.Show(issueID)
	if err != nil || issue == nil {
		return false
	}
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil {
		return false
	}
	if attachment.NoMerge || attachment.ReviewOnly {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(attachment.MergeStrategy), "local")
}

// applyMQCheck mutates status based on merge-queue state for the polecat's
// branch. If beadTerminal is true, the assigned bead is already closed, so
// there is nothing to submit and we leave the verdict as SAFE_TO_NUKE.
//
// This guard fixes the zombie-restart loop documented in bead aa-55d8:
// a closed "no-op audit" bead (e.g. aa-xtee) used to report NEEDS_MQ_SUBMIT
// forever, causing witness patrols to restart the polecat on every cycle.
func applyMQCheck(status *RecoveryStatus, bd mrFinder, beadTerminal, hasSubmittableWork, mqNotRequired bool) {
	if !hasSubmittableWork || mqNotRequired {
		// No commits/content ahead of the integration branch means gt done had
		// nothing to enqueue; treating that as missing MQ submission causes
		// recovery loops on no-op/report-only assignments.
		status.MQStatus = "not_required"
		return
	}
	if beadTerminal {
		// Work exists, but the bead is already terminal.
		status.MQStatus = "submitted"
		return
	}
	mr, mrErr := bd.FindMRForBranchAny(status.Branch)
	if mrErr != nil {
		// Can't verify MQ — fail closed until the queue state can be checked.
		status.MQStatus = "unknown"
		status.NeedsRecovery = true
		status.Verdict = "NEEDS_RECOVERY"
		status.Blockers = append(status.Blockers, fmt.Sprintf("mq_lookup_error: %v", mrErr))
		return
	}
	if mr != nil {
		status.MQStatus = "submitted"
		return
	}
	// Work was pushed but never entered the merge queue
	status.MQStatus = "not_submitted"
	status.NeedsRecovery = true
	status.Verdict = "NEEDS_MQ_SUBMIT"
}

func runPolecatGC(cmd *cobra.Command, args []string) error {
	rigName := args[0]

	mgr, r, err := getPolecatManager(rigName)
	if err != nil {
		return err
	}

	fmt.Printf("Garbage collecting stale polecat branches in %s...\n\n", r.Name)

	if polecatGCDryRun {
		// Dry run - list branches that would be deleted
		repoGit := git.NewGit(r.Path)

		// List all polecat branches
		branches, err := repoGit.ListBranches("polecat/*")
		if err != nil {
			return fmt.Errorf("listing branches: %w", err)
		}

		if len(branches) == 0 {
			fmt.Println("No polecat branches found.")
			return nil
		}

		// Get current branches
		polecats, err := mgr.List()
		if err != nil {
			return fmt.Errorf("listing polecats: %w", err)
		}

		currentBranches := make(map[string]bool)
		for _, p := range polecats {
			currentBranches[p.Branch] = true
		}

		// Show what would be deleted
		toDelete := 0
		for _, branch := range branches {
			if !currentBranches[branch] {
				fmt.Printf("  Would delete: %s\n", style.Dim.Render(branch))
				toDelete++
			} else {
				fmt.Printf("  Keep (in use): %s\n", style.Success.Render(branch))
			}
		}

		fmt.Printf("\nWould delete %d branch(es), keep %d\n", toDelete, len(branches)-toDelete)
		return nil
	}

	// Actually clean up
	deleted, err := mgr.CleanupStaleBranches()
	if err != nil {
		return fmt.Errorf("cleanup failed: %w", err)
	}

	if deleted == 0 {
		fmt.Println("No stale branches to clean up.")
	} else {
		fmt.Printf("%s Deleted %d stale branch(es).\n", style.SuccessPrefix, deleted)
	}

	return nil
}

// splitLines splits a string into non-empty lines.
func splitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func runPolecatNuke(cmd *cobra.Command, args []string) error {
	targets, err := resolvePolecatTargets(args, polecatNukeAll)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		fmt.Println("No polecats to nuke.")
		return nil
	}

	// Safety checks: refuse to nuke polecats with active work unless --force is set
	if !polecatNukeForce && !polecatNukeDryRun {
		var blocked []*SafetyCheckResult
		for _, p := range targets {
			result := checkPolecatSafety(p)
			if result.Blocked {
				blocked = append(blocked, result)
			}
		}

		if len(blocked) > 0 {
			displaySafetyCheckBlocked(blocked)
			return fmt.Errorf("blocked: %d polecat(s) failed nuke safety checks: %s", len(blocked), formatSafetyCheckBlockers(blocked))
		}
	}

	// Nuke each polecat
	var nukeErrors []string
	nuked := 0
	batchPurge := !polecatNukeDryRun && len(targets) > 1
	purgeRigs := make(map[string]*rig.Rig)
	dryRunBlocked := 0

	for _, p := range targets {
		if polecatNukeDryRun {
			blocked := !polecatNukeForce && checkPolecatSafety(p).Blocked
			if blocked {
				fmt.Printf("Would refuse to nuke %s/%s without --force:\n", p.rigName, p.polecatName)
				dryRunBlocked++
			} else {
				fmt.Printf("Would nuke %s/%s:\n", p.rigName, p.polecatName)
			}
			fmt.Printf("  - Kill session: gt-%s-%s\n", p.rigName, p.polecatName)
			fmt.Printf("  - Delete worktree: %s/polecats/%s\n", p.r.Path, p.polecatName)
			fmt.Printf("  - Delete branch (if exists)\n")
			fmt.Printf("  - Reset agent bead: %s\n", polecatBeadIDForRig(p.r, p.rigName, p.polecatName))

			if displayDryRunSafetyCheck(p) && !blocked {
				dryRunBlocked++
			}
			fmt.Println()
			continue
		}

		if polecatNukeForce {
			fmt.Printf("%s Nuking %s/%s (--force)...\n", style.Warning.Render("⚠"), p.rigName, p.polecatName)
		} else {
			fmt.Printf("Nuking %s/%s...\n", p.rigName, p.polecatName)
		}

		if err := nukePolecatFullWithOptions(p.polecatName, p.rigName, p.mgr, p.r, nukePolecatOptions{
			Force:                  polecatNukeForce,
			AcknowledgeUnpreserved: nukeAcknowledgesUnpreserved(),
			PurgeClosedEphemerals:  !batchPurge,
		}); err != nil {
			nukeErrors = append(nukeErrors, fmt.Sprintf("%s/%s: %v", p.rigName, p.polecatName, err))
			continue
		}

		nuked++
		if batchPurge {
			purgeRigs[p.r.Path] = p.r
		}
	}
	if batchPurge && len(purgeRigs) > 0 {
		for _, r := range purgeRigs {
			purgeClosedEphemeralBeads(beads.New(r.Path), beads.FindTownRoot(r.Path))
		}
	}

	// Report results
	if polecatNukeDryRun {
		if dryRunBlocked > 0 {
			fmt.Printf("\n%s %s\n", style.Warning.Render("⚠"), dryRunNukeSummary(len(targets), dryRunBlocked))
		} else {
			fmt.Printf("\n%s %s\n", style.Info.Render("ℹ"), dryRunNukeSummary(len(targets), dryRunBlocked))
		}
		return nil
	}

	if len(nukeErrors) > 0 {
		fmt.Printf("\n%s Some nukes failed:\n", style.Warning.Render("Warning:"))
		for _, e := range nukeErrors {
			fmt.Printf("  - %s\n", e)
		}
	}

	if nuked > 0 {
		fmt.Printf("\n%s Nuked %d polecat(s).\n", style.SuccessPrefix, nuked)
	}

	// Final cleanup: Kill any orphaned Claude processes that escaped the session termination.
	// This catches processes that called setsid() or were reparented during session shutdown.
	if !polecatNukeDryRun {
		cleanupOrphanedProcesses()
	}

	if len(nukeErrors) > 0 {
		return fmt.Errorf("%d nuke(s) failed", len(nukeErrors))
	}

	return nil
}

func dryRunNukeSummary(total, blocked int) string {
	if blocked > 0 {
		return fmt.Sprintf("Would refuse to nuke %d of %d polecat(s) without --force.", blocked, total)
	}
	return fmt.Sprintf("Would nuke %d polecat(s).", total)
}

// nukePolecatFull performs the complete cleanup sequence for a single polecat:
// 1. Kill tmux session
// 2. Delete worktree (via RemoveWithOptions with nuclear=true)
// 3. Delete git branch
// 4. Close agent bead
// This is the canonical cleanup path used by both `polecat nuke` and `polecat stale --cleanup`.
func nukePolecatFull(polecatName, rigName string, mgr *polecat.Manager, r *rig.Rig) error {
	return nukePolecatFullWithOptions(polecatName, rigName, mgr, r, nukePolecatOptions{PurgeClosedEphemerals: true})
}

type nukePolecatOptions struct {
	Force                 bool
	PurgeClosedEphemerals bool
	// AcknowledgeUnpreserved is the explicit acknowledgement required, together
	// with Force, to delete a polecat whose branch tip could not be shown to
	// exist on the remote. Force alone is NOT enough: an unpreserved branch
	// looks exactly like a routine nuke from the outside, so the operator has
	// to name the loss. See EnvNukeAcknowledgeUnpreserved.
	AcknowledgeUnpreserved bool
}

// EnvNukeAcknowledgeUnpreserved is the env var that makes
// nukePolecatOptions.AcknowledgeUnpreserved true on the command line. It is
// deliberately an env var rather than a flag so that it cannot be reached by
// repeating a previous --force invocation.
const EnvNukeAcknowledgeUnpreserved = "GT_NUKE_ACKNOWLEDGE_UNPRESERVED"

// nukeAcknowledgesUnpreserved reports whether the operator has explicitly
// accepted losing an unpreserved branch.
func nukeAcknowledgesUnpreserved() bool {
	return os.Getenv(EnvNukeAcknowledgeUnpreserved) == "1"
}

// preserveOutcome is the result of the pre-nuke self-preserve push.
type preserveOutcome struct {
	// RemoteRef is the remote ref verified to contain the branch tip.
	RemoteRef string
	// AlreadyThere is true when the tip was reachable from the remote before
	// this call pushed anything.
	AlreadyThere bool
}

// errBranchNotPreserved means no remote ref could be shown to contain the
// branch tip, so deleting the local branch would destroy its only copy.
var errBranchNotPreserved = errors.New("branch not preserved on remote")

// preserveBranchBeforeNuke pushes branch's tip to remote so that the worktree
// and local branch can be deleted without losing work. It FAILS CLOSED: it
// returns nil only when a remote ref is confirmed to contain the tip, and the
// caller must not delete anything otherwise.
//
// The old version of this step treated every push failure as non-fatal and
// still reported the branch as preserved, so a nuke whose self-preserve push
// was rejected (non-fast-forward: the remote branch had moved on) deleted the
// worktree and local branch while claiming success. Nothing was lost only
// because the commits happened to exist on a sibling ref (gt-yxys).
//
// Order of checks, each cheaper than the one after it:
//
//  1. Fetch, then test REACHABILITY from remote-tracking branches. Tip
//     membership is not enough: work already merged into main is an ancestor of
//     origin/main, so grepping `ls-remote` for the branch name finds nothing
//     even though the commit is preserved.
//  2. Push branch:branch and verify the remote tip is exactly the branch tip.
//  3. On a rejected push, push to <branch>-<sha7> instead. That ref cannot
//     conflict, so a rejection there means the remote is unusable and the work
//     has nowhere safe to go.
func preserveBranchBeforeNuke(g *git.Git, branch, remote string) (preserveOutcome, error) {
	ref := "refs/heads/" + branch
	tip, err := g.Rev(ref)
	if err != nil {
		return preserveOutcome{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	tip = strings.TrimSpace(tip)
	if tip == "" {
		return preserveOutcome{}, fmt.Errorf("resolve %s: empty sha", ref)
	}

	// Refresh remote-tracking refs before deciding anything: every check below
	// reads the remote's state, and a stale origin/main is how work that exists
	// only locally gets classified as preserved.
	if err := g.Fetch(remote); err != nil {
		return preserveOutcome{}, fmt.Errorf("fetch %s before nuke: %w", remote, err)
	}
	if refs, err := g.RemoteRefsContaining(tip); err == nil && len(refs) > 0 {
		return preserveOutcome{RemoteRef: refs[0], AlreadyThere: true}, nil
	}

	// Preserve under the branch's own name when possible: the merge queue and
	// the refinery look for it there.
	pushTo := branch
	if err := g.Push(remote, branch+":"+pushTo, false); err != nil {
		// Non-fast-forward: the remote branch is at a commit this tip is not a
		// descendant of. A side ref cannot conflict, so it is the fallback —
		// not a reason to delete the branch anyway.
		pushTo = branch + "-" + refSuffixSHA(tip)
		if fallbackErr := g.Push(remote, branch+":"+pushTo, false); fallbackErr != nil {
			return preserveOutcome{}, fmt.Errorf("%w: push %s:%s failed (%v) and fallback push %s:%s failed (%v)",
				errBranchNotPreserved, branch, branch, err, branch, pushTo, fallbackErr)
		}
	}

	// Never trust the push exit code alone — the entire point of this step is
	// to know the work is on the remote before deleting the only other copy.
	if err := g.VerifyPushedCommit(remote, pushTo, tip); err != nil {
		return preserveOutcome{}, fmt.Errorf("%w: %v", errBranchNotPreserved, err)
	}
	return preserveOutcome{RemoteRef: remote + "/" + pushTo}, nil
}

// refSuffixSHA is the short-sha suffix for a fallback preserve ref
// (<branch>-<sha7>): enough to identify the commit, short enough to read.
func refSuffixSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// preserveFailureBlocker decides whether a nuke may proceed after its
// self-preserve push failed. It returns nil only for an explicit override:
// --force AND the acknowledgement env var. Anything else is a hard stop, so a
// failed preserve can never be mistaken for a successful one.
func preserveFailureBlocker(rigName, polecatName string, force, acknowledged bool, cause error) error {
	if cause == nil {
		return nil
	}
	if force && acknowledged {
		return nil
	}
	remediation := fmt.Sprintf("  Push %s/%s by hand, or re-run with:\n    %s=1 %s",
		rigName, polecatName, EnvNukeAcknowledgeUnpreserved, "gt polecat nuke "+rigName+"/"+polecatName+" --force")
	if force {
		remediation = fmt.Sprintf("  The %s override is required as well as --force.\n%s",
			EnvNukeAcknowledgeUnpreserved, remediation)
	}
	return fmt.Errorf("refusing to nuke %s/%s: %v\n"+
		"  The worktree and local branch were NOT deleted.\n%s",
		rigName, polecatName, cause, remediation)
}

func nukePolecatFullWithOptions(polecatName, rigName string, mgr *polecat.Manager, r *rig.Rig, opts nukePolecatOptions) error {
	if err := checkNukeActiveMRSafety(mgr, polecatName, rigName, opts.Force); err != nil {
		return err
	}

	t := tmux.NewTmux()

	// Step 1: Kill tmux session unconditionally to prevent ghost sessions
	// when IsRunning fails to detect the session.
	//
	// The kill runs before the preserve check on purpose: a live polecat keeps
	// committing, so a branch verified while its session is still running can
	// grow a new unpushed commit before the worktree is deleted.
	sessMgr := polecat.NewSessionManager(t, r)
	if err := sessMgr.Stop(polecatName, true); err != nil {
		if !errors.Is(err, polecat.ErrSessionNotFound) {
			fmt.Printf("  %s session kill failed: %v\n", style.Warning.Render("⚠"), err)
		}
	} else {
		fmt.Printf("  %s killed session\n", style.Success.Render("✓"))
	}

	// Step 2: Get polecat info before deletion (for branch name + hooked work bead)
	polecatInfo, getErr := mgr.Get(polecatName)
	var branchToDelete string
	if getErr == nil && polecatInfo != nil {
		branchToDelete = polecatInfo.Branch
	}

	// Step 2.5: Burn any molecule attached to the polecat's hooked work bead.
	// Without this, nuked polecats leave orphan molecule refs that block re-sling.
	// The stale attached_molecule in the work bead's description causes sling to
	// fail with "bead already has N attached molecule(s)" on re-dispatch (gt-npzy).
	if getErr == nil && polecatInfo != nil && polecatInfo.Issue != "" {
		nukeCleanupMolecules(polecatInfo.Issue, r)
	}

	// Step 2.75: Self-preserve push before nuke (gt-4vr guardrail, gt-yxys
	// fail-closed). The worktree and local branch are about to be destroyed, so
	// this is the last chance to get unpushed commits onto the remote. A failed
	// push stops the nuke instead of being reported as "preserved".
	var preservedRef string
	// alreadyOnRemote distinguishes "this run pushed the branch" from "the work
	// was already reachable", which Step 4 reports differently: a merged branch
	// is not waiting on the refinery.
	var alreadyOnRemote bool
	if branchToDelete != "" {
		// Pick a repo that actually holds the branch. The worktree (when it
		// still exists) is preferred because it has the session's latest
		// commits; the bare repo is the fallback once the worktree is gone.
		// Use ClonePath from the polecat record — the worktree lives at
		// <rig>/polecats/<name>/<rigName>/, not <rig>/polecats/<name>/.
		var candidates []*git.Git
		if polecatInfo != nil && polecatInfo.ClonePath != "" {
			if _, statErr := os.Stat(polecatInfo.ClonePath); statErr == nil {
				candidates = append(candidates, git.NewGit(polecatInfo.ClonePath))
			}
		}
		bareRepoPath := filepath.Join(r.Path, ".repo.git")
		if info, statErr := os.Stat(bareRepoPath); statErr == nil && info.IsDir() {
			candidates = append(candidates, git.NewGitWithDir(bareRepoPath, ""))
		}
		var pushGit *git.Git
		for _, candidate := range candidates {
			if exists, err := candidate.BranchExists(branchToDelete); err == nil && exists {
				pushGit = candidate
				break
			}
		}

		switch {
		case pushGit == nil:
			// Not in any local ref: there is no committed copy of this branch
			// to lose, so the nuke can proceed. Uncommitted work in the
			// worktree is what the safety gates above are for.
			fmt.Printf("  %s branch %s has no local ref — nothing to preserve\n",
				style.Dim.Render("○"), branchToDelete)
		default:
			outcome, preserveErr := preserveBranchBeforeNuke(pushGit, branchToDelete, "origin")
			if preserveErr == nil {
				preservedRef = outcome.RemoteRef
				alreadyOnRemote = outcome.AlreadyThere
				if outcome.AlreadyThere {
					fmt.Printf("  %s %s already reachable from %s — nothing to preserve\n",
						style.Success.Render("✓"), branchToDelete, outcome.RemoteRef)
				} else {
					fmt.Printf("  %s preserved branch %s as %s\n", style.Success.Render("✓"), branchToDelete, outcome.RemoteRef)
					if outcome.RemoteRef != "origin/"+branchToDelete {
						fmt.Printf("  %s origin/%s was not fast-forwardable; work parked on %s instead\n",
							style.Warning.Render("⚠"), branchToDelete, outcome.RemoteRef)
					}
				}
			}

			if blocker := preserveFailureBlocker(rigName, polecatName, opts.Force, opts.AcknowledgeUnpreserved, preserveErr); blocker != nil {
				return blocker
			}
			if preserveErr != nil {
				fmt.Printf("  %s %v\n  %s proceeding under --force with %s set — %s will be deleted with no remote copy\n",
					style.Error.Render("✗"), preserveErr, style.Warning.Render("⚠"),
					EnvNukeAcknowledgeUnpreserved, branchToDelete)
			}
		}
	}

	// Step 2.9: Give the hooked work bead back (gt-vm5g4). Without this the
	// bead stays hooked to a polecat that no longer exists: `gt session restart`
	// fails with "polecat not found" and the work waits for a witness patrol to
	// notice. Compare-and-release, so a bead already re-slung elsewhere is left
	// alone. It runs after the preserve gate (a refused nuke keeps its work) and
	// before removal, which resets the agent bead itself.
	//
	// Work that survives on a polecat branch (polecat.WorkSurvival: unmerged
	// patches, local or on origin) keeps the hook instead, as does an unknown
	// answer: releasing it would let a re-sling start a fresh polecat from main
	// over that work (gt-ibt8, gt-da2x). Removal below applies the same rule.
	// The resume comment is written only once the final state is known.
	var infoForHook *polecat.Polecat
	if getErr == nil {
		infoForHook = polecatInfo
	}
	hookedWork := startNukeHookedWork(
		newPolecatWorkReleaserFn(beads.FindTownRoot(r.Path), ""),
		func(beadID string) (string, error) { return nukeSurvivingWorkFn(r.Path, beadID) },
		rigName, polecatName, infoForHook,
		func() string { return readAgentHookBeadFn(r, rigName, polecatName) },
	)

	// Step 3: Delete worktree (nuclear=true to bypass safety checks for stale polecats)
	if err := mgr.RemoveWithOptions(polecatName, opts.Force, true, false); err != nil {
		if errors.Is(err, polecat.ErrPolecatNotFound) {
			fmt.Printf("  %s worktree already gone\n", style.Dim.Render("○"))
			resetPolecatAgentBeadForReuse(r, rigName, polecatName)
		} else {
			return fmt.Errorf("worktree removal failed: %w", err)
		}
	} else {
		fmt.Printf("  %s deleted worktree\n", style.Success.Render("✓"))
	}

	// Step 4: Delete local branch (if we know it)
	// Local branch can always be deleted (worktree is already gone).
	// Remote branch is never deleted during nuke — the refinery owns
	// remote branch cleanup after successful merge (gt mq post-merge).
	// This prevents the race where nuke deletes the branch before the
	// refinery has a chance to merge it. (gt-v5ku)
	if branchToDelete != "" {
		repoGit := getRepoGitForRig(r.Path)
		if err := repoGit.DeleteBranch(branchToDelete, true); err != nil {
			fmt.Printf("  %s branch delete: %v\n", style.Dim.Render("○"), err)
		} else {
			fmt.Printf("  %s deleted local branch %s\n", style.Success.Render("✓"), branchToDelete)
		}
		// Only claim preservation for a ref this run actually verified. When the
		// override let an unpreserved branch through, say so — silence here is
		// how a nuke reports success after destroying the only copy (gt-yxys).
		switch {
		case preservedRef != "" && alreadyOnRemote:
			fmt.Printf("  %s %s's work already on %s\n", style.Dim.Render("○"), branchToDelete, preservedRef)
		case preservedRef != "":
			fmt.Printf("  %s remote ref %s preserved for refinery merge\n", style.Dim.Render("○"), preservedRef)
		case opts.AcknowledgeUnpreserved:
			fmt.Printf("  %s %s deleted with no remote copy (%s set)\n",
				style.Warning.Render("⚠"), branchToDelete, EnvNukeAcknowledgeUnpreserved)
		default:
			fmt.Printf("  %s %s had no remote copy to report\n",
				style.Warning.Render("⚠"), branchToDelete)
		}
	}

	// Step 4.5: Report the hooked work bead's final state; a kept hook gets the
	// resume comment now that removal can no longer change it.
	hookedWork.finish()

	// Step 5: Purge closed ephemeral beads (wisps) accumulated during sessions.
	// Without this, closed wisps from mol-polecat-work steps, mol-witness-patrol
	// cycles, etc. accumulate across sessions and pollute bd ready/list (hq-6161m).
	if opts.PurgeClosedEphemerals {
		purgeClosedEphemeralBeads(beads.New(r.Path), beads.FindTownRoot(r.Path))
	}

	// gt-7kr: destruction left no audit trail — nuke never emitted a feed
	// event. Best-effort: never fail the nuke over telemetry.
	reason := "nuked"
	if opts.Force {
		reason = "nuked --force"
	}
	_ = events.LogFeed(events.TypeKill, nukeActorIdentity(), events.KillPayload(rigName, polecatName, reason))

	return nil
}

// nukeSurvivingWorkFn is the shared surviving-work predicate for a rig; a
// seam for tests.
var nukeSurvivingWorkFn = polecat.SurvivingWorkForIssue

// nukeHookedWork carries a nuke's hooked work bead from the release decision
// (before the sandbox is removed) to the final report (after).
type nukeHookedWork struct {
	rel      polecatWorkReleaser
	survives func(beadID string) (string, error)
	agentID  string
	beadID   string
}

// workSurvivalVerdict classifies a survival answer: the branch the work
// survives on, or unknown (an error other than "the rig has no git repo").
func workSurvivalVerdict(branch string, err error) (survivesOn string, unknown bool) {
	if err != nil && !errors.Is(err, polecat.ErrNoRigRepo) {
		return "", true
	}
	return branch, false
}

// startNukeHookedWork decides what happens to the nuked polecat's hooked work
// bead before the sandbox is removed. The bead is the polecat record's issue
// or, for a polecat reaped before its nuke, the agent bead's hook_bead
// (readHook). Only a bead still held by this polecat is touched. Surviving work
// — or an unknown answer — keeps the hook for now; otherwise the bead is
// released through the shared compare-and-release helper. Returns nil when
// there is nothing left to settle after removal.
func startNukeHookedWork(rel polecatWorkReleaser, survives func(beadID string) (string, error),
	rigName, polecatName string, p *polecat.Polecat, readHook func() string) *nukeHookedWork {
	beadID := ""
	if p != nil {
		beadID = p.Issue
	}
	if beadID == "" && readHook != nil {
		beadID = readHook()
	}
	if beadID == "" {
		return nil
	}
	h := &nukeHookedWork{rel: rel, survives: survives, agentID: fmt.Sprintf("%s/polecats/%s", rigName, polecatName), beadID: beadID}
	if held, _ := heldBy(rel, h.agentID, beadID); !held {
		return nil
	}
	if branch, unknown := workSurvivalVerdict(survives(beadID)); branch != "" || unknown {
		return h
	}
	h.release()
	return nil
}

func (h *nukeHookedWork) release() {
	if out := releasePolecatWork(h.rel, h.agentID, h.beadID, false); !out.Released && out.SkipNote != "" {
		fmt.Printf("  %s hooked work %s not released: %s\n", style.Dim.Render("○"), h.beadID, out.SkipNote)
	}
}

// finish settles a kept hook once removal (and the local branch delete) is
// over. Survival is asked again, because removal can change the answer: work
// that no longer survives is released (guarded), and a hook that stays gets a
// comment built from the final answer — the resume command for a branch, or
// the command to re-check an unknown answer.
func (h *nukeHookedWork) finish() {
	if h == nil {
		return
	}
	if held, _ := heldBy(h.rel, h.agentID, h.beadID); !held {
		return
	}
	branch, unknown := workSurvivalVerdict(h.survives(h.beadID))
	rigName := strings.SplitN(h.agentID, "/", 2)[0]
	var note, text string
	switch {
	case branch != "":
		note = "work survives on " + branch
		text = fmt.Sprintf("gt polecat nuke: %s was nuked with its work preserved on branch %s. "+
			"The bead stays hooked so a re-sling does not start a fresh polecat from main over that work.\n"+
			"Resume it:          gt sling %s %s --branch %s\n"+
			"Discard and redo:   gt sling %s %s --force",
			h.agentID, branch, h.beadID, rigName, branch, h.beadID, rigName)
	case unknown:
		note = "could not verify surviving work"
		text = fmt.Sprintf("gt polecat nuke: %s was nuked; hook kept: could not verify surviving work; "+
			"run gt polecat surviving-work %s", h.agentID, h.beadID)
	default:
		h.release()
		return
	}
	fmt.Printf("  %s Kept %s hooked: %s\n", style.Dim.Render("○"), h.beadID, note)
	if err := h.rel.Annotate(h.beadID, text); err != nil {
		fmt.Printf("  %s Could not annotate %s: %v\n", style.Dim.Render("Warning:"), h.beadID, err)
	}
}

// readAgentHookBeadFn is a seam for tests.
var readAgentHookBeadFn = readAgentHookBead

// readAgentHookBead returns the hook_bead recorded on a polecat's agent bead,
// or "" when it cannot be read.
func readAgentHookBead(r *rig.Rig, rigName, polecatName string) string {
	agentBeadID := polecatBeadIDForRig(r, rigName, polecatName)
	issue, err := beads.New(r.Path).ForAgentBead().Show(agentBeadID)
	if err != nil || issue == nil {
		return ""
	}
	fields := beads.ParseAgentFields(issue.Description)
	if fields == nil {
		return ""
	}
	return fields.HookBead
}

// nukeActorIdentity returns a best-effort identity string for the agent or
// operator performing a nuke, for feed-event attribution. Falls back to
// "unknown" rather than failing the nuke over an identity lookup.
func nukeActorIdentity() string {
	roleInfo, err := GetRole()
	if err != nil {
		return "unknown"
	}
	return formatActorIdentity(roleInfo)
}

// formatActorIdentity renders a RoleInfo as a feed-attribution actor string.
// Pulled out of nukeActorIdentity so the formatting itself is testable
// without a real Gas Town workspace on disk.
func formatActorIdentity(roleInfo RoleInfo) string {
	switch roleInfo.Role {
	case RoleMayor:
		return constants.RoleMayor
	case RoleCrew:
		return fmt.Sprintf("%s/crew/%s", roleInfo.Rig, roleInfo.Polecat)
	case RolePolecat:
		return fmt.Sprintf("%s/%s", roleInfo.Rig, roleInfo.Polecat)
	case RoleWitness:
		return fmt.Sprintf("%s/witness", roleInfo.Rig)
	case RoleRefinery:
		return fmt.Sprintf("%s/refinery", roleInfo.Rig)
	case RoleDeacon:
		return constants.RoleDeacon
	default:
		return string(roleInfo.Role)
	}
}

type activeMRRemovalChecker interface {
	ActiveMRRemovalBlocker(name string) (activeMR, blocker string)
}

func checkNukeActiveMRSafety(checker activeMRRemovalChecker, polecatName, rigName string, force bool) error {
	if force || checker == nil {
		return nil
	}
	if activeMR, blocker := checker.ActiveMRRemovalBlocker(polecatName); blocker != "" {
		return fmt.Errorf("cannot nuke %s/%s: MR %s is still pending in merge queue (%s)\nRefinery will process the MR and clean up after merge\nUse --force to override (risks data loss)", rigName, polecatName, activeMR, blocker)
	}
	return nil
}

func resetPolecatAgentBeadForReuse(r *rig.Rig, rigName, polecatName string) {
	agentBeadID := polecatBeadIDForRig(r, rigName, polecatName)
	bd := beads.New(r.Path)
	if err := bd.ForAgentBead().ResetAgentBeadForReuse(agentBeadID, "nuked"); err != nil {
		fmt.Printf("  %s agent bead not found or already cleaned\n", style.Dim.Render("○"))
	} else {
		fmt.Printf("  %s reset agent bead %s\n", style.Success.Render("✓"), agentBeadID)
	}
}

// nukeCleanupMolecules burns any molecule attached to a work bead during polecat nuke.
// This prevents stale attached_molecule references from blocking re-dispatch (gt-npzy).
// Best-effort: failures are logged but don't abort the nuke.
func nukeCleanupMolecules(workBeadID string, r *rig.Rig) {
	// Use mayor/rig as workDir so ResolveBeadsDir finds the Dolt-backed
	// .beads/ directory, not the gitignored rig-root .beads/. Without this,
	// detach/close operations route to the wrong database and the stale
	// molecule attachment persists on the work bead. (gt--1up)
	bd := beads.New(filepath.Join(r.Path, "mayor", "rig"))

	// Fetch the work bead to check for attached molecules
	issue, err := bd.Show(workBeadID)
	if err != nil {
		fmt.Printf("  %s molecule cleanup: could not fetch work bead %s: %v\n",
			style.Dim.Render("○"), workBeadID, err)
		return
	}

	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil || attachment.AttachedMolecule == "" {
		return // No molecule attached — nothing to clean up
	}

	moleculeID := attachment.AttachedMolecule

	// Force-close descendant steps before detaching (prevents orphaned step beads).
	// Uses force variant since nuke is destructive — must succeed even for beads in
	// invalid states. Best-effort — log but proceed in nuke path.
	if _, err := forceCloseDescendants(bd, moleculeID); err != nil {
		style.PrintWarning("nuke: could not close descendants of %s: %v", moleculeID, err)
	}

	// Detach the molecule with audit trail
	if _, detachErr := bd.DetachMoleculeWithAudit(workBeadID, beads.DetachOptions{
		Operation: "burn",
		Reason:    "polecat nuked: cleaning stale molecule",
	}); detachErr != nil {
		fmt.Printf("  %s molecule detach failed for %s: %v\n",
			style.Warning.Render("⚠"), moleculeID, detachErr)
		return
	}

	// Remove dependency bonds so stale molecule discovery does not block re-dispatch.
	removeMoleculeBonds(bd, workBeadID, moleculeID)

	// Force-close the orphaned wisp root so it doesn't linger
	if closeErr := bd.ForceCloseWithReason("burned: polecat nuked", moleculeID); closeErr != nil {
		fmt.Printf("  %s molecule root close failed for %s: %v\n",
			style.Warning.Render("⚠"), moleculeID, closeErr)
	} else {
		fmt.Printf("  %s burned stale molecule %s from work bead %s\n",
			style.Success.Render("✓"), moleculeID, workBeadID)
	}
}

// cleanupOrphanedProcesses kills Claude processes that survived session termination.
// Uses aggressive zombie detection via tmux session verification.
func cleanupOrphanedProcesses() {
	results, err := util.CleanupZombieClaudeProcesses()
	if err != nil {
		// Non-fatal: log and continue
		fmt.Printf("  %s orphan cleanup check failed: %v\n", style.Dim.Render("○"), err)
		return
	}

	if len(results) == 0 {
		return
	}

	// Report what was cleaned up
	var killed, escalated int
	for _, r := range results {
		switch r.Signal {
		case "SIGTERM", "SIGKILL":
			killed++
		case "UNKILLABLE":
			escalated++
		}
	}

	if killed > 0 {
		fmt.Printf("  %s cleaned up %d orphaned process(es)\n", style.Success.Render("✓"), killed)
	}
	if escalated > 0 {
		fmt.Printf("  %s %d process(es) survived SIGKILL (unkillable)\n", style.Warning.Render("⚠"), escalated)
	}
}

func runPolecatStale(cmd *cobra.Command, args []string) error {
	rigName := args[0]
	mgr, r, err := getPolecatManager(rigName)
	if err != nil {
		return err
	}

	fmt.Printf("Detecting stale polecats in %s (threshold: %d commits behind main)...\n\n", r.Name, polecatStaleThreshold)

	staleInfos, err := mgr.DetectStalePolecats(polecatStaleThreshold)
	if err != nil {
		return fmt.Errorf("detecting stale polecats: %w", err)
	}

	if len(staleInfos) == 0 {
		fmt.Println("No polecats found.")
		return nil
	}

	// JSON output
	if polecatStaleJSON {
		return json.NewEncoder(os.Stdout).Encode(staleInfos)
	}

	// Summary counts
	var staleCount, safeCount int
	for _, info := range staleInfos {
		if info.IsStale {
			staleCount++
		} else {
			safeCount++
		}
	}

	// Display results
	for _, info := range staleInfos {
		statusIcon := style.Success.Render("●")
		statusText := "active"
		if info.IsStale {
			statusIcon = style.Warning.Render("○")
			statusText = "stale"
		}

		fmt.Printf("%s %s (%s)\n", statusIcon, style.Bold.Render(info.Name), statusText)

		// Session status
		if info.HasActiveSession {
			fmt.Printf("    Session: %s\n", style.Success.Render("running"))
		} else {
			fmt.Printf("    Session: %s\n", style.Dim.Render("stopped"))
		}

		// Commits behind
		if info.CommitsBehind > 0 {
			behindStyle := style.Dim
			if info.CommitsBehind >= polecatStaleThreshold {
				behindStyle = style.Warning
			}
			fmt.Printf("    Behind main: %s\n", behindStyle.Render(fmt.Sprintf("%d commits", info.CommitsBehind)))
		}

		// Agent state
		if info.AgentState != "" {
			fmt.Printf("    Agent state: %s\n", info.AgentState)
		} else {
			fmt.Printf("    Agent state: %s\n", style.Dim.Render("no bead"))
		}

		// Uncommitted work
		if info.HasUncommittedWork {
			fmt.Printf("    Uncommitted: %s\n", style.Error.Render("yes"))
		}

		// Reason
		fmt.Printf("    Reason: %s\n", info.Reason)
		fmt.Println()
	}

	// Summary
	fmt.Printf("Summary: %d stale, %d active\n", staleCount, safeCount)

	// Cleanup if requested
	if polecatStaleCleanup && staleCount > 0 {
		fmt.Println()
		if polecatStaleDryRun {
			fmt.Printf("Would clean up %d stale polecat(s):\n", staleCount)
			for _, info := range staleInfos {
				if info.IsStale {
					fmt.Printf("  - %s: %s\n", info.Name, info.Reason)
				}
			}
		} else {
			fmt.Printf("Cleaning up %d stale polecat(s)...\n", staleCount)
			nuked := 0
			batchPurge := staleCount > 1
			for _, info := range staleInfos {
				if !info.IsStale {
					continue
				}
				fmt.Printf("Nuking %s...\n", info.Name)
				if err := nukePolecatFullWithOptions(info.Name, rigName, mgr, r, nukePolecatOptions{
					AcknowledgeUnpreserved: nukeAcknowledgesUnpreserved(),
					PurgeClosedEphemerals:  !batchPurge,
				}); err != nil {
					fmt.Printf("  %s (%v)\n", style.Error.Render("failed"), err)
				} else {
					nuked++
				}
			}
			if batchPurge && nuked > 0 {
				purgeClosedEphemeralBeads(beads.New(r.Path), beads.FindTownRoot(r.Path))
			}
			fmt.Printf("\n%s Nuked %d stale polecat(s).\n", style.SuccessPrefix, nuked)

			// Clean up any orphaned processes that survived session termination
			cleanupOrphanedProcesses()
		}
	}

	return nil
}

func runPolecatPrune(cmd *cobra.Command, args []string) error {
	rigName := args[0]

	mgr, r, err := getPolecatManager(rigName)
	if err != nil {
		return err
	}

	// Use the mayor/rig clone (or bare repo) for branch operations
	var repoGit *git.Git
	bareRepoPath := filepath.Join(r.Path, ".repo.git")
	if info, statErr := os.Stat(bareRepoPath); statErr == nil && info.IsDir() {
		repoGit = git.NewGitWithDir(bareRepoPath, "")
	} else {
		repoGit = git.NewGit(filepath.Join(r.Path, "mayor", "rig"))
	}

	fmt.Printf("Pruning stale polecat branches in %s...\n", r.Name)

	// First, prune stale remote-tracking refs so we detect deleted remote branches
	if err := repoGit.FetchPrune("origin"); err != nil {
		if polecatPruneRemote {
			return fmt.Errorf("refreshing origin before remote prune: %w", err)
		}
		fmt.Printf("  %s fetch --prune: %v (continuing anyway)\n", style.Warning.Render("⚠"), err)
	}

	// Prune local branches that are merged or have no remote
	pruned, err := repoGit.PruneStaleBranches("polecat/*", polecatPruneDryRun)
	if err != nil {
		return fmt.Errorf("pruning local branches: %w", err)
	}

	if len(pruned) == 0 {
		fmt.Println("No stale local polecat branches found.")
	} else {
		verb := "Pruned"
		if polecatPruneDryRun {
			verb = "Would prune"
		}
		for _, b := range pruned {
			fmt.Printf("  %s %s (%s)\n", style.Success.Render("✓"), b.Name, b.Reason)
		}
		fmt.Printf("\n%s %d local branch(es).\n", verb, len(pruned))
	}

	// Optionally prune remote polecat branches
	if polecatPruneRemote {
		fmt.Println()
		fmt.Println("Pruning remote polecat branches...")

		remotePruned, remoteErr := pruneRemotePolecatBranches(managerStateLookup(mgr), repoGit, polecatPruneDryRun)
		if remoteErr != nil {
			return remoteErr
		}

		if remotePruned == 0 {
			fmt.Println("No stale remote polecat branches found.")
		} else {
			verb := "Pruned"
			if polecatPruneDryRun {
				verb = "Would prune"
			}
			fmt.Printf("\n%s %d remote branch(es).\n", verb, remotePruned)
		}
	}

	return nil
}

// minRemoteBranchPruneAge is the minimum time a generated polecat branch must
// have existed before --remote pruning will even consider deleting it,
// regardless of what beads/tmux report about its owning polecat. Ancestry
// alone cannot tell a truly stale, abandoned branch apart from one that
// belongs to a polecat still mid-spawn in the very dispatch burst that
// triggered this prune run: a brand-new branch with zero commits ahead of
// the base is trivially "merged" into it, exactly like a merged-and-abandoned
// one (gt-527j). This grace window is a backstop for state that has not
// propagated yet, independent of the liveness lookup below.
const minRemoteBranchPruneAge = 15 * time.Minute

// polecatStateLookup resolves a polecat's current lifecycle state by name.
// The production implementation wraps a *polecat.Manager's Get; tests
// substitute a deterministic fake so a remote-prune decision never depends
// on a live beads/tmux backend to be exercised.
type polecatStateLookup func(name string) (polecat.State, error)

// managerStateLookup adapts a polecat.Manager (nil-safe) to a
// polecatStateLookup. A nil manager yields a nil lookup, which
// remotePolecatBranchEligibleForPrune treats as "cannot verify" and fails
// closed.
func managerStateLookup(mgr *polecat.Manager) polecatStateLookup {
	if mgr == nil {
		return nil
	}
	return func(name string) (polecat.State, error) {
		p, err := mgr.Get(name)
		if err != nil {
			return "", err
		}
		return p.State, nil
	}
}

// remotePolecatBranchEligibleForPrune reports whether branch may even be
// considered for remote pruning, before the ancestry/merge-preservation
// check runs. It fails closed — returns false — on every case it cannot
// positively clear: an unparseable branch name, an undecodable or too-young
// generated timestamp, a missing state lookup, or a polecat whose live state
// could not be read. Only a branch that is old enough AND whose owning
// polecat is demonstrably not live (idle/done, or no such identity exists
// any more) is eligible; the caller still has to prove it is actually
// preserved on the target branch on top of that (git branch -d semantics —
// unmerged work is never deleted).
func remotePolecatBranchEligibleForPrune(lookup polecatStateLookup, branch string, now time.Time) bool {
	meta, ok := polecat.ParseGeneratedBranchName(branch)
	if !ok || meta.Polecat == "" {
		return false
	}

	generatedAt, ok := meta.GeneratedAt()
	if !ok || now.Sub(generatedAt) < minRemoteBranchPruneAge {
		return false
	}

	if lookup == nil {
		return false
	}
	state, err := lookup(meta.Polecat)
	if err != nil {
		// A definitive "no such polecat identity" means nothing can be live
		// under this name any more. Any other error (beads/tmux unreadable,
		// timeout, ...) is an unknown state, which fails closed.
		return errors.Is(err, polecat.ErrPolecatNotFound)
	}
	return state.IsReuseEligible()
}

func pruneRemotePolecatBranches(lookup polecatStateLookup, repoGit *git.Git, dryRun bool) (int, error) {
	defaultBranch := repoGit.RemoteDefaultBranch()
	target := repoGit.CleanDefaultBranchBaseRef("origin", defaultBranch)
	if targetRemote := git.RemoteForRef(target); targetRemote != "" && targetRemote != "origin" {
		if err := repoGit.FetchPrune(targetRemote); err != nil {
			return 0, fmt.Errorf("refreshing %s before remote prune: %w", targetRemote, err)
		}
	}
	remoteRefs, lsErr := repoGit.ListPushRemoteRefsWithHashes("origin", "refs/heads/polecat/")
	if lsErr != nil {
		return 0, fmt.Errorf("listing remote refs: %w", lsErr)
	}

	now := time.Now()
	remotePruned := 0
	for _, ref := range remoteRefs {
		if !strings.HasPrefix(ref.Name, "refs/heads/") {
			continue
		}
		branch := strings.TrimPrefix(ref.Name, "refs/heads/")
		if !remotePolecatBranchEligibleForPrune(lookup, branch, now) {
			continue
		}

		status, statusErr := repoGit.PushRemoteRefTargetStatus("origin", ref, target)
		if statusErr != nil || !status.Preserved {
			continue
		}

		if dryRun {
			fmt.Printf("  Would delete remote: %s\n", style.Dim.Render(branch))
			remotePruned++
			continue
		}
		if delErr := repoGit.DeleteRemoteBranchIfAt("origin", branch, ref.Hash); delErr != nil {
			fmt.Printf("  %s remote %s: %v\n", style.Warning.Render("⚠"), branch, delErr)
			continue
		}
		fmt.Printf("  %s deleted remote %s\n", style.Success.Render("✓"), branch)
		remotePruned++
	}

	return remotePruned, nil
}

// runPolecatPoolInit creates a persistent polecat pool for a rig.
// Creates N polecats with identities and worktrees in IDLE state.
// Existing polecats are preserved — only new ones are created.
func runPolecatPoolInit(cmd *cobra.Command, args []string) error {
	rigName := args[0]

	mgr, r, err := getPolecatManager(rigName)
	if err != nil {
		return err
	}

	// Determine pool size: flag > rig config > default
	poolSize := 4 // default
	rigCfg, cfgErr := rig.LoadRigConfig(r.Path)
	if cfgErr == nil && rigCfg.PolecatPoolSize > 0 {
		poolSize = rigCfg.PolecatPoolSize
	}
	if polecatPoolInitSize > 0 {
		poolSize = polecatPoolInitSize
	}

	// Determine names: rig config > name pool theme
	var fixedNames []string
	if cfgErr == nil && len(rigCfg.PolecatNames) > 0 {
		fixedNames = rigCfg.PolecatNames
	}

	// List existing polecats to avoid recreating them
	existing, err := mgr.List()
	if err != nil {
		return fmt.Errorf("listing existing polecats: %w", err)
	}
	existingNames := make(map[string]bool)
	for _, p := range existing {
		existingNames[p.Name] = true
	}

	fmt.Printf("Initializing persistent polecat pool for %s (target size: %d)\n", rigName, poolSize)
	if len(existing) > 0 {
		fmt.Printf("  Existing polecats: %d\n", len(existing))
	}

	// Build the list of names to create
	var namesToCreate []string
	if len(fixedNames) > 0 {
		// Use configured names, skip ones that already exist
		for _, name := range fixedNames {
			if len(namesToCreate)+len(existingNames) >= poolSize {
				break
			}
			if !existingNames[name] {
				namesToCreate = append(namesToCreate, name)
			}
		}
	} else {
		// Use name pool allocation for new names
		namePool := mgr.GetNamePool()
		namePool.Reconcile(existingNamesList(existing))
		for len(namesToCreate)+len(existingNames) < poolSize {
			name, allocErr := namePool.Allocate()
			if allocErr != nil {
				return fmt.Errorf("allocating polecat name: %w", allocErr)
			}
			if !existingNames[name] {
				namesToCreate = append(namesToCreate, name)
			}
		}
	}

	if len(namesToCreate) == 0 {
		fmt.Printf("\n%s Pool already at target size (%d polecats).\n", style.Bold.Render("✓"), len(existing))
		return nil
	}

	if polecatPoolInitDryRun {
		fmt.Printf("\nWould create %d polecat(s):\n", len(namesToCreate))
		for _, name := range namesToCreate {
			fmt.Printf("  %s %s\n", style.Dim.Render("→"), name)
		}
		return nil
	}

	// Create each polecat
	fmt.Printf("\nCreating %d polecat(s)...\n", len(namesToCreate))
	created := 0
	for _, name := range namesToCreate {
		fmt.Printf("  %s Creating %s...", style.Dim.Render("→"), name)
		p, addErr := mgr.Add(name)
		if addErr != nil {
			fmt.Printf(" %s %v\n", style.Warning.Render("FAILED"), addErr)
			continue
		}
		// Set agent state to idle (polecat was created without work).
		// Use the retry variant: createAgentBeadWithRetry above leaves a brief
		// Dolt MVCC visibility window where the just-committed bead isn't yet
		// readable by the next UpdateAgentState query, surfacing as "issue not
		// found". Retries with backoff close that window — same pattern as
		// SetAgentStateWithRetry's other call site in polecat_spawn.go.
		if stateErr := mgr.SetAgentStateWithRetry(name, "idle"); stateErr != nil {
			fmt.Printf(" %s (created but couldn't set idle state: %v)\n", style.Warning.Render("⚠"), stateErr)
		} else {
			fmt.Printf(" %s (%s)\n", style.Success.Render("✓"), style.Dim.Render(p.ClonePath))
		}
		created++
	}

	fmt.Printf("\n%s Pool initialized: %d created, %d total (target: %d)\n",
		style.Bold.Render("✓"), created, created+len(existing), poolSize)

	// Sync hooks so all polecat settings.json files reflect current defaults.
	// Pool-init may run long after rig-add, when gt defaults have changed.
	townRoot, twErr := workspace.FindFromCwdOrError()
	if twErr == nil {
		ensureHooksBase()
		if err := syncRigHooks(townRoot, rigName); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to sync hooks after pool-init: %v\n", err)
		}
	}

	return nil
}

// existingNamesList extracts polecat names from a slice of Polecat pointers.
func existingNamesList(polecats []*polecat.Polecat) []string {
	names := make([]string, len(polecats))
	for i, p := range polecats {
		names[i] = p.Name
	}
	return names
}
