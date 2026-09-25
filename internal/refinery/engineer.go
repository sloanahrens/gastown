// Package refinery provides the merge queue processing agent.
package refinery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/lintlock"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/util"
)

// shortSHA returns at most 8 characters of a SHA for display.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// DefaultStaleClaimTimeout is the default duration after which a claimed MR
// is considered abandoned and eligible for re-claim. This is conservative
// to avoid re-claiming MRs that are legitimately processing long test suites.
// Can be overridden per-rig via MergeQueueConfig.StaleClaimTimeout.
const DefaultStaleClaimTimeout = 30 * time.Minute

// isClaimStale checks if a claimed MR should be considered abandoned based on
// its UpdatedAt timestamp and configured timeout. Returns true if the claim
// is stale (eligible for re-claim), false if the claim is recent or the
// timestamp is invalid/missing.
func isClaimStale(updatedAt string, timeout time.Duration) (stale bool, parseErr error) {
	if updatedAt == "" {
		return false, nil // No timestamp - assume claim is valid
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return false, err // Caller should log the parse error
	}
	return time.Since(t) >= timeout, nil
}

// GateConfig defines a single quality gate command.
// GatePhase controls when a gate runs in the merge pipeline.
type GatePhase string

const (
	// GatePhasePreMerge runs the gate before the squash merge (default).
	// The gate validates the source branch on the target baseline.
	GatePhasePreMerge GatePhase = "pre-merge"

	// GatePhasePostSquash runs the gate after the squash merge but before push.
	// The gate validates the actual combined code, catching issues that only
	// manifest in the merged result (broken imports, boot failures, missing
	// templates). On failure, the merge is reset.
	GatePhasePostSquash GatePhase = "post-squash"
)

type GateConfig struct {
	// Cmd is the shell command to execute.
	Cmd string `json:"cmd"`

	// Timeout is the maximum time the gate command may run.
	// Zero means no timeout (inherits context deadline).
	Timeout time.Duration `json:"timeout"`

	// Phase controls when this gate runs: "pre-merge" (default) or "post-squash".
	// Pre-merge gates run before the squash merge on the source branch.
	// Post-squash gates run after the squash merge on the combined result,
	// before pushing. On post-squash failure, the merge is reset.
	Phase GatePhase `json:"phase"`
}

// GateStep is one named command in an ordered gate sequence.
type GateStep struct {
	Name string
	Cmd  string
}

// GateResult holds the outcome of a single gate execution.
type GateResult struct {
	Name    string
	Success bool
	Error   string
	Elapsed time.Duration
}

// MergeQueueConfig holds configuration for the merge queue processor.
//
// Note: Integration branch gating (polecat/refinery enabled flags) is handled at
// MR creation time via config.MergeQueueConfig and formula injection, not here.
// The Engineer's job is to merge whatever target the MR specifies — it doesn't
// need to know whether integration branches are enabled.
type MergeQueueConfig struct {
	// Enabled controls whether the merge queue is active.
	Enabled bool `json:"enabled"`

	// OnConflict is the strategy for handling conflicts: "assign_back" or "auto_rebase".
	OnConflict string `json:"on_conflict"`

	// RunTests controls whether to run tests before merging.
	RunTests bool `json:"run_tests"`

	// TestCommand is the command to run for testing.
	TestCommand string `json:"test_command"`

	// DeleteMergedBranches controls whether to delete branches after merge.
	DeleteMergedBranches bool `json:"delete_merged_branches"`

	// RetryFlakyTests is the number of times to retry flaky tests.
	RetryFlakyTests int `json:"retry_flaky_tests"`

	// PollInterval is how often to check for new MRs.
	PollInterval time.Duration `json:"poll_interval"`

	// MaxConcurrent is the maximum number of MRs to process concurrently.
	MaxConcurrent int `json:"max_concurrent"`

	// StaleClaimTimeout is how long a claimed MR can go without updates before
	// being considered abandoned and eligible for re-claim. This handles the
	// case where a refinery crashes mid-merge, leaving an MR permanently claimed.
	// Set conservatively to avoid re-claiming MRs with long-running test suites.
	// NOTE: Only one refinery instance runs per rig (enforced by ErrAlreadyRunning
	// in manager.go), so concurrent re-claim is not a concern in practice.
	StaleClaimTimeout time.Duration `json:"stale_claim_timeout"`

	// Gates defines named quality gate commands to run before merging.
	// When non-empty, gates replace the legacy RunTests/TestCommand path.
	// Each gate runs as a shell command with an optional per-gate timeout.
	Gates map[string]*GateConfig `json:"gates"`

	// GatesParallel controls whether gates run concurrently.
	// When true, all gates start simultaneously; any failure = overall failure.
	GatesParallel bool `json:"gates_parallel"`

	// BatchGateSteps is the ordered command sequence the batch gate runs, in
	// place of the legacy chained TestCommand. Set at runtime by `gt mq batch
	// run` from the rig's setup/typecheck/lint/build/test commands, so each
	// step is named in its own failure and the lint step can wait out
	// golangci-lint's lock (gt-ijqw). Not read from config.json.
	BatchGateSteps []GateStep `json:"-"`

	// StaleClaimWarningAfter is how long a claimed MR can sit without updates
	// before it triggers a "warning" severity anomaly.
	StaleClaimWarningAfter time.Duration `json:"stale_claim_warning_after"`

	// StaleClaimCriticalAfter is how long a claimed MR can sit without updates
	// before it triggers a "critical" severity anomaly.
	StaleClaimCriticalAfter time.Duration `json:"stale_claim_critical_after"`

	// MaxRetryCount is the maximum number of conflict resolution retries
	// before escalation to Mayor.
	MaxRetryCount int `json:"max_retry_count"`

	// AutoPush controls whether the refinery pushes to origin after merging.
	// When false, the refinery merges locally but does not push — the user
	// or a separate process handles pushing. Useful to avoid triggering
	// CI/CD builds (e.g. Vercel) on every merge.
	AutoPush bool `json:"auto_push"`

	// MergeStrategy controls how the refinery lands work: "direct" (default)
	// does local merge + git push; "pr" uses the VCS provider's merge API
	// which respects branch protection/restriction rules.
	MergeStrategy string `json:"merge_strategy,omitempty"`

	// VCSProvider selects the VCS platform for PR operations when
	// MergeStrategy="pr". Valid values: "github" (default), "bitbucket".
	VCSProvider string `json:"vcs_provider,omitempty"`

	// RequireReview controls whether the refinery requires at least one approving
	// review before merging a PR. Only meaningful when MergeStrategy="pr".
	// Nil defaults to false (no review required).
	RequireReview *bool `json:"require_review,omitempty"`

	// Batch holds configuration for the batch-then-bisect merge queue.
	// When nil or MaxBatchSize <= 1, batching is disabled and MRs process sequentially.
	Batch *BatchConfig `json:"batch,omitempty"`

	// Editorial configures the om editorial gate (push precondition, review
	// parameters). Nil means no rig tier has set it; Required defaults to
	// false so upstream behavior is unchanged.
	Editorial *config.EditorialConfig `json:"editorial,omitempty"`
}

// DefaultMergeQueueConfig returns sensible defaults for merge queue configuration.
func DefaultMergeQueueConfig() *MergeQueueConfig {
	return &MergeQueueConfig{
		Enabled:                 true,
		OnConflict:              "assign_back",
		RunTests:                true,
		TestCommand:             "",
		DeleteMergedBranches:    true,
		GatesParallel:           true, // gt-8b2i: run gates concurrently (~2x speedup)
		RetryFlakyTests:         1,
		PollInterval:            30 * time.Second,
		MaxConcurrent:           1,
		StaleClaimTimeout:       DefaultStaleClaimTimeout,
		StaleClaimWarningAfter:  2 * time.Hour,
		StaleClaimCriticalAfter: 6 * time.Hour,
		MaxRetryCount:           5,
		AutoPush:                true,
	}
}

// MRInfo holds merge request information for display and processing.
// This replaces mrqueue.MR after the mrqueue package removal.
type MRInfo struct {
	ID              string     // Bead ID (e.g., "gt-abc123")
	Branch          string     // Source branch (e.g., "polecat/nux")
	Target          string     // Target branch (e.g., "main")
	SourceIssue     string     // The work item being merged
	Worker          string     // Who did the work
	Rig             string     // Which rig
	Title           string     // MR title
	Priority        int        // Priority (lower = higher priority)
	AgentBead       string     // Agent bead ID that created this MR
	CommitSHA       string     // Source branch tip submitted to the queue
	PRURL           string     // Recorded pull request URL, if available
	PRNumber        int        // Recorded pull request number, if available
	RetryCount      int        // Conflict retry count
	ConflictTaskID  string     // Open conflict-resolution task for this MR (if any)
	ConvoyID        string     // Parent convoy ID if part of a convoy
	ConvoyCreatedAt *time.Time // Convoy creation time
	CreatedAt       time.Time  // MR creation time
	BlockedBy       string     // Task ID blocking this MR

	// Labels are the MR bead's labels. The batch reviewer needs them because
	// editorial.RetirementLabel — the mark that lets a deliberate rubric
	// retirement past the criterion-deletion guard — lives here and nowhere
	// else in the MR's data (gt-2oi0).
	Labels []string

	// Pre-verification fields (Phase 3: polecat-owned rebasing)
	// When set, the refinery can skip gates if VerifiedBase matches target HEAD.
	PreVerified     bool      // Polecat ran full gates after rebasing onto target
	PreVerifiedAt   time.Time // When verification completed
	PreVerifiedBase string    // Target branch SHA at verification time

	// EditorialReviewedHead is the commit gt mq review last produced an
	// approve/request_changes verdict for (refs/notes/om lives there). The
	// push precondition reads it to find the note without scanning.
	// Empty means "never reviewed" and the precondition refuses to push
	// when the rig requires editorial review.
	EditorialReviewedHead string

	// PreVerifiedGates/Exit/Log make the fast-path honest (om-gate T8): the
	// fast-path additionally requires PreVerifiedGates to match the rig's
	// current gate-set hash (config.GateSetSHA) before trusting the stamp —
	// a rig that adds, removes, or edits a gate command invalidates any
	// pre-verification recorded against the old set.
	PreVerifiedGates string
	PreVerifiedExit  int
	PreVerifiedLog   string

	// Raw data for agent-side queue health analysis (ZFC: agent decides, Go transports)
	UpdatedAt          time.Time // When the MR was last updated
	Assignee           string    // Who claimed this MR (empty = unclaimed)
	BranchExistsLocal  bool      // Whether the MR branch exists locally (this repo's refs/heads)
	BranchExistsRemote bool      // Whether the MR branch exists on origin (live ls-remote, not cached tracking refs)
	// BranchExistsLocalUnknown/Remote are true when the corresponding
	// existence check's query itself failed (network hiccup, timeout, a repo
	// in a bad state) rather than confirming absence — safeBranchExistenceCheck
	// then reports the paired BranchExists* as true (the fail-open direction
	// ListQueueAnomalies' both-false orphan rule needs), so this is the only
	// way a JSON reader of `gt refinery ready --all --json` can distinguish
	// "confirmed present" from "could not tell" (gt-bagu).
	BranchExistsLocalUnknown  bool
	BranchExistsRemoteUnknown bool
}

// MRAnomaly represents an MR queue health problem that can stall processing.
type MRAnomaly struct {
	ID       string        `json:"id"`
	Branch   string        `json:"branch"`
	Type     string        `json:"type"` // stale-claim | orphaned-branch
	Assignee string        `json:"assignee,omitempty"`
	Age      time.Duration `json:"age,omitempty"`
	Detail   string        `json:"detail"`
}

// errMergeSlotTimeout is returned by acquireMainPushSlot when retries are
// exhausted due to slot contention. Infrastructure errors (beads down,
// permission errors) return a different error so callers can distinguish
// transient contention from real failures that need operator attention.
var errMergeSlotTimeout = errors.New("merge slot contention timeout")

// mergeSlotSeq is a package-level counter for unique merge slot holder IDs.
// Using time.Now().UnixNano() alone is insufficient on Windows where timer
// resolution can cause identical timestamps across concurrent goroutines.
var mergeSlotSeq uint64

// Engineer is the merge queue processor that polls for ready merge-requests
// and processes them according to the merge queue design.
type Engineer struct {
	rig                   *rig.Rig
	beads                 *beads.Beads
	git                   *git.Git
	config                *MergeQueueConfig
	prProvider            PRProvider // VCS-specific PR operations (nil when MergeStrategy != "pr")
	workDir               string
	output                io.Writer    // Output destination for user-facing messages
	router                *mail.Router // Mail router for sending protocol messages
	mergeSlotEnsureExists func() (string, error)
	mergeSlotAcquire      func(holder string, addWaiter bool) (*beads.MergeSlotStatus, error)
	mergeSlotRelease      func(holder string) error
	mergeSlotMaxRetries   int           // Max retries for slot acquisition (0 = no retry)
	mergeSlotRetryBackoff time.Duration // Initial backoff between retries
	mergeSlotStaleAfter   time.Duration // Per-push lease TTL (0 = defaultMergeSlotStaleAfter)
	recoverDeadWorker     func(deadWorkerRecoveryRequest) bool
	testAllowSyntheticMRs bool               // Test-only: legacy merge-mechanics tests use synthetic MRs without beads.
	editorialExec         editorial.ExecFunc // Gate-script invoker for batch editorial reviews; production: editorial.RunGateScript, tests override with a stub.

	// dirtyWorktreeReportedAt is the Unix-nano time of the last
	// external-dirty-worktree escalation to the witness (gt-nnwy). The
	// worktree stays dirty until a human clears it, so without this the
	// identical refusal would re-escalate on every poll cycle.
	dirtyWorktreeReportedAt atomic.Int64

	// currentGateSetSHAFn resolves the rig's current gate-set hash
	// (config.GateSetSHA over the same binding gt sling/gt done use) for the
	// pre-verification fast-path staleness check (om-gate T8). Overridable
	// in tests so the check doesn't require a real rig config layout.
	currentGateSetSHAFn func() string

	// findOrphanDoltServersFn scans for orphaned test 'dolt sql-server'
	// processes immediately before a gate run, so a hang or timeout caused by
	// contention with a leaked server isn't silently attributed to the diff
	// under test (gt-twil). Overridable in tests.
	findOrphanDoltServersFn func() ([]util.DoltOrphanServer, error)

	// escalateFn replaces the witness nudge (escalateToWitness) in tests. Nil
	// in production, where the nudge is the real one; a test that leaves it
	// nil would reach a live witness.
	escalateFn func(string)

	// notifyMergedFn replaces the witness MERGED mail (notifyWitnessMerged) in
	// tests. Nil in production, where the real mail send runs; a test that
	// leaves it nil would reach a live witness.
	notifyMergedFn func(mr *MRInfo, mergeCommit string)

	// mrRejectionPollInterval overrides defaultMRRejectionPollInterval for
	// tests, so a rejection lands within one gate run without a real test
	// sleeping out the production interval.
	mrRejectionPollInterval time.Duration
}

// NewEngineer creates a new Engineer for the given rig.
func NewEngineer(r *rig.Rig) *Engineer {
	cfg := DefaultMergeQueueConfig()

	// Determine the git working directory for refinery operations.
	// Prefer refinery/rig worktree, fall back to mayor/rig (legacy architecture).
	// Using rig.Path directly would find town's .git with rig-named remotes instead of "origin".
	gitDir := filepath.Join(r.Path, "refinery", "rig")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		gitDir = filepath.Join(r.Path, "mayor", "rig")
	}
	beadsClient := beads.New(r.Path)

	e := &Engineer{
		rig:     r,
		beads:   beadsClient,
		git:     git.NewGit(gitDir),
		config:  cfg,
		workDir: gitDir,
		output:  os.Stdout,
		router:  mail.NewRouter(r.Path),
		mergeSlotEnsureExists: func() (string, error) {
			return beadsClient.MergeSlotEnsureExists()
		},
		mergeSlotAcquire: func(holder string, addWaiter bool) (*beads.MergeSlotStatus, error) {
			return beadsClient.MergeSlotAcquire(holder, addWaiter)
		},
		mergeSlotRelease: func(holder string) error {
			return beadsClient.MergeSlotRelease(holder)
		},
		mergeSlotMaxRetries:   10,
		mergeSlotRetryBackoff: 500 * time.Millisecond,
		editorialExec:         editorial.RunGateScript,
	}
	e.recoverDeadWorker = func(req deadWorkerRecoveryRequest) bool {
		// Read e.router/e.output/e.beads fresh on each call (SetOutput may run
		// after construction, and tests point e.beads at an in-process store),
		// matching newDeadWorkerRecoverer's shared wiring used by the
		// Manager's manual `gt mq reject` path (gt-2usm). Passing e.beads
		// keeps recovery on the same injected beads client as the rest of
		// the Engineer instead of shelling out to a separate bd subprocess.
		return newDeadWorkerRecoverer(r, e.router, e.output, e.beads)(req)
	}
	e.currentGateSetSHAFn = func() string {
		townRoot := filepath.Dir(r.Path)
		mq := rig.ResolveMergeQueueConfig(townRoot, r.Name)
		namedGates := make(map[string]string, len(e.config.Gates))
		for name, gc := range e.config.Gates {
			namedGates[name] = gc.Cmd
		}
		// config.CombineGateSetSHA is the same function `gt done` stamps
		// pre_verified_gates with (internal/cmd/done.go); the two values must
		// agree exactly or the fast-path never fires (om-gate T8).
		return config.CombineGateSetSHA(mq, namedGates)
	}
	e.findOrphanDoltServersFn = func() ([]util.DoltOrphanServer, error) {
		return util.FindOrphanDoltServers(filepath.Dir(r.Path))
	}
	return e
}

// SetOutput sets the output writer for user-facing messages.
// This is useful for testing or redirecting output.
func (e *Engineer) SetOutput(w io.Writer) {
	e.output = w
}

// LoadConfig loads merge queue configuration from the rig's config.json.
func (e *Engineer) LoadConfig() error {
	configPath := filepath.Join(e.rig.Path, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Use defaults if no config file
			return nil
		}
		return fmt.Errorf("reading config: %w", err)
	}

	// Parse config file to extract merge_queue section
	var rawConfig struct {
		MergeQueue json.RawMessage `json:"merge_queue"`
	}
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	if rawConfig.MergeQueue == nil {
		// No merge_queue section, use defaults
		return nil
	}

	// Parse merge_queue section into our config struct
	// We need special handling for poll_interval (string -> Duration)
	var mqRaw struct {
		Enabled              *bool                     `json:"enabled"`
		OnConflict           *string                   `json:"on_conflict"`
		RunTests             *bool                     `json:"run_tests"`
		TestCommand          *string                   `json:"test_command"`
		DeleteMergedBranches *bool                     `json:"delete_merged_branches"`
		RetryFlakyTests      *int                      `json:"retry_flaky_tests"`
		PollInterval         *string                   `json:"poll_interval"`
		MaxConcurrent        *int                      `json:"max_concurrent"`
		StaleClaimTimeout    *string                   `json:"stale_claim_timeout"`
		Gates                map[string]*gateConfigRaw `json:"gates"`
		GatesParallel        *bool                     `json:"gates_parallel"`
		AutoPush             *bool                     `json:"auto_push"`
		MergeStrategy        *string                   `json:"merge_strategy"`
		VCSProvider          *string                   `json:"vcs_provider"`
		RequireReview        *bool                     `json:"require_review"`
		Editorial            *config.EditorialConfig   `json:"editorial"`
	}

	if err := json.Unmarshal(rawConfig.MergeQueue, &mqRaw); err != nil {
		return fmt.Errorf("parsing merge_queue config: %w", err)
	}

	// Apply non-nil values to config (preserving defaults for missing fields)
	if mqRaw.Enabled != nil {
		e.config.Enabled = *mqRaw.Enabled
	}
	if mqRaw.OnConflict != nil {
		e.config.OnConflict = *mqRaw.OnConflict
	}
	if mqRaw.RunTests != nil {
		e.config.RunTests = *mqRaw.RunTests
	}
	if mqRaw.TestCommand != nil {
		e.config.TestCommand = *mqRaw.TestCommand
	}
	if mqRaw.DeleteMergedBranches != nil {
		e.config.DeleteMergedBranches = *mqRaw.DeleteMergedBranches
	}
	if mqRaw.RetryFlakyTests != nil {
		e.config.RetryFlakyTests = *mqRaw.RetryFlakyTests
	}
	if mqRaw.MaxConcurrent != nil {
		e.config.MaxConcurrent = *mqRaw.MaxConcurrent
	}
	if mqRaw.PollInterval != nil {
		dur, err := time.ParseDuration(*mqRaw.PollInterval)
		if err != nil {
			return fmt.Errorf("invalid poll_interval %q: %w", *mqRaw.PollInterval, err)
		}
		e.config.PollInterval = dur
	}
	if mqRaw.StaleClaimTimeout != nil {
		dur, err := time.ParseDuration(*mqRaw.StaleClaimTimeout)
		if err != nil {
			return fmt.Errorf("invalid stale_claim_timeout %q: %w", *mqRaw.StaleClaimTimeout, err)
		}
		if dur <= 0 {
			return fmt.Errorf("stale_claim_timeout must be positive, got %v", dur)
		}
		e.config.StaleClaimTimeout = dur
	}

	// Parse gates configuration
	if mqRaw.Gates != nil {
		e.config.Gates = make(map[string]*GateConfig, len(mqRaw.Gates))
		for name, raw := range mqRaw.Gates {
			gc := &GateConfig{Cmd: raw.Cmd}
			if raw.Timeout != "" {
				dur, err := time.ParseDuration(raw.Timeout)
				if err != nil {
					return fmt.Errorf("invalid timeout for gate %q: %w", name, err)
				}
				if dur <= 0 {
					return fmt.Errorf("gate %q timeout must be positive, got %v", name, dur)
				}
				gc.Timeout = dur
			}
			switch raw.Phase {
			case "", "pre-merge":
				gc.Phase = GatePhasePreMerge
			case "post-squash":
				gc.Phase = GatePhasePostSquash
			default:
				return fmt.Errorf("gate %q has invalid phase %q: must be \"pre-merge\" or \"post-squash\"", name, raw.Phase)
			}
			e.config.Gates[name] = gc
		}
	}
	if mqRaw.GatesParallel != nil {
		e.config.GatesParallel = *mqRaw.GatesParallel
	}
	if mqRaw.AutoPush != nil {
		e.config.AutoPush = *mqRaw.AutoPush
	}
	if mqRaw.MergeStrategy != nil {
		e.config.MergeStrategy = *mqRaw.MergeStrategy
	}
	if mqRaw.VCSProvider != nil {
		e.config.VCSProvider = *mqRaw.VCSProvider
	}
	if mqRaw.RequireReview != nil {
		e.config.RequireReview = mqRaw.RequireReview
	}
	if mqRaw.Editorial != nil {
		defaulted := mqRaw.Editorial.WithDefaults()
		e.config.Editorial = &defaulted
	}

	// Initialize the PR provider when merge_strategy=pr.
	if e.config.MergeStrategy == "pr" {
		if err := e.initPRProvider(); err != nil {
			return fmt.Errorf("initializing PR provider: %w", err)
		}
	}

	return nil
}

// initPRProvider creates the appropriate PRProvider based on vcs_provider config.
// Defaults to GitHub when vcs_provider is empty or "github".
func (e *Engineer) initPRProvider() error {
	switch e.config.VCSProvider {
	case "", "github":
		e.prProvider = newGitHubPRProvider(e.git)
	case "bitbucket":
		p, err := newBitbucketPRProvider(e.git)
		if err != nil {
			return err
		}
		e.prProvider = p
	default:
		return fmt.Errorf("unknown vcs_provider %q (supported: github, bitbucket)", e.config.VCSProvider)
	}
	return nil
}

// gateConfigRaw is the JSON-friendly representation of a gate config
// with timeout as a string duration.
type gateConfigRaw struct {
	Cmd     string `json:"cmd"`
	Timeout string `json:"timeout"`
	Phase   string `json:"phase"`
}

// Config returns the current merge queue configuration.
func (e *Engineer) Config() *MergeQueueConfig {
	return e.config
}

// ProcessResult contains the result of processing a merge request.
type ProcessResult struct {
	Success        bool
	MergeCommit    string
	Error          string
	Conflict       bool
	TestsFailed    bool
	SlotTimeout    bool // Merge slot contention timeout (distinct from build/test failure)
	BranchNotFound bool // Source branch no longer exists (e.g. cleaned up after cherry-pick)
	// OriginUnreadable marks a refusal to stage a merge because origin could
	// not be read at all (ls-remote/fetch failure), so the submission itself is
	// unjudged — like SlotTimeout this is transient, the MR stays queued, and
	// no polecat is nudged to push a branch that may already be pushed.
	OriginUnreadable bool
	NoMerge          bool // MR/source is intentionally not merge-eligible, not a build failure
	NeedsApproval    bool // PR exists but lacks required approving review (merge_strategy=pr)
	// EditorialRefused marks a refusal by the om editorial push precondition
	// (om-gate T6): no approve note exists whose patch-id still matches the
	// range about to land. Like NeedsApproval it is not a build/test/conflict
	// failure — the MR stays queued and is retried next cycle — but unlike
	// NeedsApproval the refusal is a gate verdict about this exact diff, so
	// callers must neither nudge the worker nor start dead-worker recovery
	// for it (HandleMRInfoFailure). EditorialReason carries the classified
	// reason (editorial.PreconditionReason) so callers route on it rather
	// than on Error's message text.
	EditorialRefused bool
	EditorialReason  editorial.PreconditionReason
	// WorktreeExternallyDirty marks a merge refused because the Refinery's own
	// rig worktree carried uncommitted or staged changes this process did not
	// create (gt-nnwy). Nothing about the MR is wrong, so like EditorialRefused
	// it leaves the MR queued and the worker unnudged — but the cause is
	// environmental, so the refusal goes to the witness for a human to clear
	// rather than being retried silently forever (HandleMRInfoFailure).
	WorktreeExternallyDirty bool
}

// doMerge performs the actual git merge operation.
func (e *Engineer) doMerge(ctx context.Context, mr *MRInfo, skipGates ...bool) ProcessResult {
	if mr == nil {
		return ProcessResult{Success: false, Error: "merge request is missing"}
	}
	branch, target := mr.Branch, mr.Target

	// Step 0: refuse to gate a tree an external writer has staged or edited
	// (gt-nnwy). This must precede restoreTargetToOrigin below — its reset is
	// the first mutation of the merge and would discard that work silently.
	cleanupWorktree, err := e.beginWorktreeOwnedMerge("merge")
	if err != nil {
		return ProcessResult{
			Success:                 false,
			WorktreeExternallyDirty: errors.Is(err, ErrExternallyDirtyWorktree),
			Error:                   err.Error(),
		}
	}
	defer cleanupWorktree()

	// Realign local target to origin before staging: a prior run may have
	// exited before restoring it (gt-032w, gt-u093). prepareMergeTarget below
	// discards whatever was there anyway, so this never blocks the merge —
	// it just makes the discard happen up front, and surfaces a real error
	// instead of silently building on unresolved state.
	if err := e.restoreTargetToOrigin(target); err != nil {
		return ProcessResult{Success: false, Error: fmt.Errorf("realign %s to origin before merge: %w", target, err).Error()}
	}

	if eligibility := e.recheckMRStillMergeable(mr, target, true); !eligibility.Success {
		if eligibility.NoMerge {
			_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s is not merge-eligible — skipping merge: %s\n", mr.ID, eligibility.Error)
		}
		return eligibility
	}

	// Step 1: Verify source branch exists locally (shared .repo.git with polecats)
	_, _ = fmt.Fprintf(e.output, "[Engineer] Checking local branch %s...\n", branch)
	exists, err := e.git.BranchExists(branch)
	if err != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to check branch %s: %v", branch, err),
		}
	}
	if !exists {
		return ProcessResult{
			Success:        false,
			BranchNotFound: true,
			Error:          fmt.Sprintf("branch %s not found locally", branch),
		}
	}
	mergeRef, err := e.submittedBranchHead(mr)
	if err != nil {
		return ProcessResult{Success: false, Error: err.Error()}
	}
	if refusal := e.assertSubmittedHeadReachableOnOrigin(mr, mergeRef); refusal != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] ✗ Refusing to gate/merge MR %s: %v\n", mr.ID, refusal.Err)
		return refusal.result()
	}

	// Step 2: Stage the merge on the target branch.
	_, _ = fmt.Fprintf(e.output, "[Engineer] Staging the merge on %s...\n", target)
	if err := e.prepareMergeTarget(target); err != nil {
		return ProcessResult{Success: false, Error: err.Error()}
	}

	// Step 2.1: Recognize an MR a prior pass already merged and pushed before
	// dying between the push and its own bookkeeping (gt-wh66). This must run
	// before the empty-merge check below: when nothing else has landed on
	// target since, the merge result's tree is byte-identical to mergeRef's
	// own tree, which is exactly what an empty merge looks like too — without
	// this check first, a crashed-but-landed MR reads as "changes nothing"
	// and gets closed ineligible instead of completed as merged.
	if e.mergeAlreadyLanded(target, mergeRef) {
		return e.resumeLandedMerge(mr, target, mergeRef)
	}

	// Step 2.5: Refuse a submission that changes nothing. Gating a no-op MR
	// spends the suite on the target's own tree and then lands it as merged.
	if empty := e.checkSubmittedHeadAddsChange(mr, target, mergeRef); !empty.Success {
		return empty
	}

	// Step 3: Check for merge conflicts against the staged baseline.
	_, _ = fmt.Fprintf(e.output, "[Engineer] Checking for conflicts...\n")
	conflicts, err := e.git.CheckConflictsAtHead(mergeRef)
	if err != nil {
		return ProcessResult{
			Success:  false,
			Conflict: true,
			Error:    fmt.Sprintf("conflict check failed: %v", err),
		}
	}
	if len(conflicts) > 0 {
		return ProcessResult{
			Success:  false,
			Conflict: true,
			Error:    fmt.Sprintf("merge conflicts in: %v", conflicts),
		}
	}

	// Step 3.5: Push submodule commits if the branch changes submodule pointers.
	// The refinery owns all remote pushes — submodule commits must land before the
	// parent pointer is merged, otherwise main gets dangling submodule references.
	subChanges, err := e.git.SubmoduleChanges("origin/"+target, mergeRef)
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not check submodule changes: %v\n", err)
	}
	if len(subChanges) > 0 {
		// Ensure submodules are initialized in the refinery worktree
		// Use mayor/rig as reference to avoid re-fetching from remote
		mayorRig := filepath.Join(e.rig.Path, "mayor", "rig")
		if initErr := git.InitSubmodules(e.git.WorkDir(), mayorRig); initErr != nil {
			return ProcessResult{
				Success: false,
				Error:   fmt.Sprintf("failed to init submodules in refinery worktree: %v", initErr),
			}
		}
		for _, sc := range subChanges {
			if sc.NewSHA == "" {
				continue // Submodule removed, nothing to push
			}
			if eligibility := e.recheckMRStillMergeable(mr, target, true); !eligibility.Success {
				return eligibility
			}
			_, _ = fmt.Fprintf(e.output, "[Engineer] Pushing submodule %s (commit %s)...\n", sc.Path, shortSHA(sc.NewSHA))
			if pushErr := e.git.PushSubmoduleCommit(sc.Path, sc.NewSHA, "origin"); pushErr != nil {
				return ProcessResult{
					Success: false,
					Error:   fmt.Sprintf("failed to push submodule %s: %v", sc.Path, pushErr),
				}
			}
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] Pushed %d submodule(s)\n", len(subChanges))
	}

	// Step 4: Run quality gates (or legacy tests) if configured.
	// Phase 3 fast-path: if skipGates is true (pre-verified MR with matching base),
	// skip all gate execution — the polecat already ran gates after rebasing.
	shouldSkipGates := len(skipGates) > 0 && skipGates[0]
	if shouldSkipGates {
		_, _ = fmt.Fprintln(e.output, "[Engineer] Skipping gates (pre-verified by polecat)")
	} else {
		gateCtx, cancelGateWatch := e.watchMRRejection(ctx, mr.ID)
		defer cancelGateWatch() // panic-safety backstop; cancel is idempotent
		verification, ran := e.runVerification(gateCtx)
		midRun := rejectedMidRun(ctx, gateCtx)
		cancelGateWatch()
		if ran {
			if !verification.Success {
				if midRun {
					if eligibility := e.recheckMRStillMergeable(mr, target, true); !eligibility.Success {
						return eligibility
					}
				}
				return verification
			}
			_, _ = fmt.Fprintln(e.output, "[Engineer] Verification passed")
		}
	}

	// PR merge path: when merge_strategy=pr, use the VCS provider's merge API
	// instead of local merge + direct push. This respects branch
	// protection/restriction rules and preserves the PR audit trail.
	// The VCS provider (GitHub, Bitbucket) is selected via vcs_provider config.
	if e.config.MergeStrategy == "pr" {
		return e.doMergePR(ctx, mr)
	}

	// Step 5: Perform the actual merge, preserving the submitted head in target ancestry.
	// Get the original commit message from the polecat branch to preserve the
	// conventional commit format (feat:/fix:) in the merge commit message.
	mergeMsg, err := e.git.GetBranchCommitMessage(branch)
	if err != nil {
		// Fallback to a descriptive message if we can't get the original
		mergeMsg = fmt.Sprintf("Merge %s into %s", branch, target)
		if mr.SourceIssue != "" {
			mergeMsg = fmt.Sprintf("Merge %s into %s (%s)", branch, target, mr.SourceIssue)
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not get original commit message: %v\n", err)
	}
	// gt-rswr: a branch can still carry a raw checkpoint_dog/gt-pvx commit
	// into the merge queue — e.g. gt done's own squash step (gt-3wf) skips a
	// branch that was already pushed to origin in an earlier session.
	// Merging such a branch with --no-ff would carry that commit, and its
	// generic "WIP: checkpoint (auto)" subject, straight onto target's
	// history. Squash instead so target never gains one, whether or not it
	// ended up as the branch tip.
	hasAutoSave, asErr := checkpoint.HasAutoSaveCommits(e.git.WorkDir(), "origin/"+target, mergeRef)
	if asErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not check for auto-save commits: %v\n", asErr)
	}

	if hasAutoSave {
		if checkpoint.IsAutoSaveSubject(strings.TrimSpace(mergeMsg)) {
			mergeMsg = fmt.Sprintf("Merge %s into %s", branch, target)
			if mr.SourceIssue != "" {
				mergeMsg = fmt.Sprintf("Merge %s into %s (%s)", branch, target, mr.SourceIssue)
			}
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] Branch has auto-save/checkpoint commit(s); squashing to keep them off %s: %s\n", target, strings.TrimSpace(mergeMsg))
		if err := e.git.MergeSquash(mergeRef, mergeMsg); err != nil {
			// git merge --squash never sets MERGE_HEAD, so --abort isn't available;
			// reset the worktree back to the target baseline staged above instead.
			conflicts, conflictErr := e.git.GetConflictingFiles()
			isConflict := conflictErr == nil && len(conflicts) > 0
			if resetErr := e.git.ResetHard("HEAD"); resetErr != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset after squash failure: %v\n", resetErr)
			}
			if isConflict {
				return ProcessResult{
					Success:  false,
					Conflict: true,
					Error:    "merge conflict during actual merge",
				}
			}
			return ProcessResult{
				Success: false,
				Error:   fmt.Sprintf("merge failed: %v", err),
			}
		}
	} else {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Merging with message: %s\n", strings.TrimSpace(mergeMsg))
		if err := e.git.MergeNoFF(mergeRef, mergeMsg); err != nil {
			// ZFC: Use git's porcelain output to detect conflicts instead of parsing stderr.
			// GetConflictingFiles() uses `git diff --diff-filter=U` which is proper.
			conflicts, conflictErr := e.git.GetConflictingFiles()
			if conflictErr == nil && len(conflicts) > 0 {
				_ = e.git.AbortMerge()
				return ProcessResult{
					Success:  false,
					Conflict: true,
					Error:    "merge conflict during actual merge",
				}
			}
			// Non-conflict failure: still need to abort to clean up dirty merge state
			_ = e.git.AbortMerge()
			return ProcessResult{
				Success: false,
				Error:   fmt.Sprintf("merge failed: %v", err),
			}
		}
	}

	// Step 5.5: Run post-squash gates on the merged result.
	// These validate the actual combined code before it goes anywhere.
	// On failure, reset the merge to undo the local merge commit.
	if !shouldSkipGates {
		postCtx, cancelPostWatch := e.watchMRRejection(ctx, mr.ID)
		defer cancelPostWatch() // panic-safety backstop; cancel is idempotent
		postResult := e.runGatesForPhase(postCtx, GatePhasePostSquash)
		midRun := rejectedMidRun(ctx, postCtx)
		cancelPostWatch()
		if !postResult.Success {
			if resetErr := e.restoreTargetToOrigin(target); resetErr != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after post-squash gate failure: %v\n", target, resetErr)
			}
			if midRun {
				if eligibility := e.recheckMRStillMergeable(mr, target, true); !eligibility.Success {
					return eligibility
				}
			}
			return postResult
		}
	}

	// Step 5.6: Refuse a local merge that added nothing to what origin/target
	// already holds — the one check the submitted head cannot make, since a
	// branch whose content the target already carries has a tree of its own
	// that differs from the target's (gt-j5cc).
	if landed := e.checkLandedMergeAddsChange(mr, target, mergeRef); !landed.Success {
		if resetErr := e.restoreTargetToOrigin(target); resetErr != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after empty merge: %v\n", target, resetErr)
		}
		return landed
	}

	// Step 6: Get the merge commit SHA
	mergeCommit, err := e.git.Rev("HEAD")
	if err != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to get merge commit SHA: %v", err),
		}
	}

	// Step 7-8: Push to origin (when auto_push is enabled).
	if e.config.AutoPush {
		// Editorial push precondition: no approve note with a matching
		// patch-id, no push. Runs before the push slot is acquired so a
		// refusal never holds it, and unconditionally — skipGates does
		// not bypass this. No-op when the rig hasn't set
		// merge_queue.editorial.required.
		notes, landed, cerr := e.editorialPrecondition("[Engineer]", []*MRInfo{mr}, target)
		if cerr != nil {
			if resetErr := e.restoreTargetToOrigin(target); resetErr != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after editorial precondition failure: %v\n", target, resetErr)
			}
			return editorialRefusalResult(cerr)
		}

		// Acquire merge slot before push to serialize writes to the default branch.
		// Only serialize pushes to the rig's default branch (typically main).
		// Integration-branch and feature-branch pushes don't need serialization.
		var pushHolder string
		if target == e.rig.DefaultBranch() {
			var slotErr error
			pushHolder, slotErr = e.acquireMainPushSlot(ctx)
			if slotErr != nil {
				// Undo the local merge commit staged onto target in Step 2.
				if resetErr := e.restoreTargetToOrigin(target); resetErr != nil {
					_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after slot failure: %v\n", target, resetErr)
				}
				// Only classify as SlotTimeout for actual contention (retries exhausted).
				// Infrastructure errors (beads down, permission errors) should surface
				// through the normal failure/notification path for operator visibility.
				return ProcessResult{
					Success:     false,
					SlotTimeout: errors.Is(slotErr, errMergeSlotTimeout),
					Error:       fmt.Sprintf("failed to acquire merge slot before push: %v", slotErr),
				}
			}
			defer func() {
				// pushHolder is empty when the self-conflict bypass fires — conflict-resolution
				// owns the slot, so we must not release it here.
				if pushHolder != "" {
					if releaseErr := e.mergeSlotRelease(pushHolder); releaseErr != nil {
						_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to release merge slot for push (%s): %v\n", pushHolder, releaseErr)
					}
				}
			}()
		}

		if eligibility := e.recheckMRStillMergeable(mr, target, true); !eligibility.Success {
			if resetErr := e.restoreTargetToOrigin(target); resetErr != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after pre-push eligibility failure: %v\n", target, resetErr)
			}
			return eligibility
		}

		// The pre-push hook refuses default-branch pushes from a polecat-shaped
		// context unless GT_REFINERY_MERGE=1 is set AND corroborated by a
		// Refinery identity signal, GT_REFINERY=1 or GT_ROLE=*/refinery
		// (gt-ibt8, gt-9tf9); this process inherits GT_REFINERY=1 from the
		// Refinery's tmux session env (internal/refinery/manager.go).
		_, _ = fmt.Fprintf(e.output, "[Engineer] Pushing to origin/%s...\n", target)
		if err := e.git.PushWithEnv("origin", mergePushRef(target), false, []string{git.EnvRefineryMerge}); err != nil {
			// Undo the local merge commit. Without this, the next retry could
			// see stale local state from the failed push.
			if resetErr := e.restoreTargetToOrigin(target); resetErr != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after push failure: %v\n", target, resetErr)
			}
			return ProcessResult{
				Success: false,
				Error:   fmt.Sprintf("failed to push to origin: %v", err),
			}
		}
		if err := e.git.VerifyPushedCommit("origin", target, mergeCommit); err != nil {
			if resetErr := e.restoreTargetToOrigin(target); resetErr != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after verified-push failure: %v\n", target, resetErr)
			}
			return ProcessResult{
				Success: false,
				Error:   err.Error(),
			}
		}
		e.copyEditorialNotes("[Engineer]", landed, notes, []*MRInfo{mr})
	} else {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Auto-push disabled, skipping push to origin/%s\n", target)
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Successfully merged: %s\n", shortSHA(mergeCommit))
	return ProcessResult{
		Success:     true,
		MergeCommit: mergeCommit,
	}
}

// doMergePR handles merging via the VCS provider's PR merge API (merge_strategy=pr).
// This respects branch protection/restriction rules including required reviews.
// The VCS provider (GitHub, Bitbucket) is selected via vcs_provider config.
// Called from doMerge after quality gates have passed.
//
//nolint:unparam // ctx is reserved for future use when git methods accept context
func (e *Engineer) doMergePR(ctx context.Context, mr *MRInfo) ProcessResult {
	_ = ctx
	if mr == nil {
		return ProcessResult{Success: false, Error: "merge request is missing"}
	}
	branch, target := mr.Branch, mr.Target
	provider := e.config.VCSProvider
	if provider == "" {
		provider = "github"
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Using PR merge strategy (vcs_provider=%s)\n", provider)

	if e.prProvider == nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("no PR provider configured for vcs_provider=%s", provider),
		}
	}
	// Step PR.1: Find the PR for this branch
	pr, err := e.prProvider.FindPullRequest(branch, mr.PRURL, mr.PRNumber, mr.CommitSHA)
	if err != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to find PR for branch %s: %v", branch, err),
		}
	}
	if pr == nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("no open PR found for branch %s — merge_strategy=pr requires a PR", branch),
		}
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Found PR #%d for branch %s\n", pr.Number, branch)
	if strings.TrimSpace(mr.CommitSHA) != "" {
		if err := requirePullRequestHead(pr, mr.CommitSHA); err != nil {
			return ProcessResult{Success: false, Error: err.Error()}
		}
	}

	// Step PR.2: Check approval status if require_review is enabled
	requireReview := e.config.RequireReview != nil && *e.config.RequireReview
	if requireReview {
		approved, err := e.prProvider.IsPRApproved(pr)
		if err != nil {
			return ProcessResult{
				Success: false,
				Error:   fmt.Sprintf("failed to check PR #%d approval status: %v", pr.Number, err),
			}
		}
		if !approved {
			_, _ = fmt.Fprintf(e.output, "[Engineer] PR #%d awaiting human approval — deferring merge\n", pr.Number)
			return ProcessResult{
				Success:       false,
				NeedsApproval: true,
				Error:         fmt.Sprintf("PR #%d requires approving review before merge", pr.Number),
			}
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] PR #%d has approving review\n", pr.Number)
	}

	if eligibility := e.recheckMRStillMergeable(mr, target, true); !eligibility.Success {
		return eligibility
	}
	if err := e.ensureMRInfoCommitSHA(mr); err != nil {
		return ProcessResult{Success: false, Error: err.Error()}
	}
	// Re-read immediately before merge so a PR head advance cannot sneak between
	// approval/recheck and the provider merge call.
	pr, err = e.prProvider.FindPullRequest(branch, mr.PRURL, mr.PRNumber, mr.CommitSHA)
	if err != nil {
		return ProcessResult{Success: false, Error: fmt.Sprintf("failed to refresh PR for branch %s: %v", branch, err)}
	}
	if pr == nil {
		return ProcessResult{Success: false, Error: fmt.Sprintf("no open PR found for branch %s — merge_strategy=pr requires a PR", branch)}
	}
	if err := requirePullRequestHead(pr, mr.CommitSHA); err != nil {
		return ProcessResult{Success: false, Error: err.Error()}
	}

	// Step PR.3: Editorial push precondition (om-gate T6). On the local-merge
	// path this runs immediately before the push; here the landing push IS the
	// provider's merge, so it runs immediately before MergePR — after the
	// approval/head checks that can still defer the merge. Refusal leaves the
	// MR queued (see HandleMRInfoFailure's EditorialRefused branch); the
	// provider is never called.
	notes, landed, cerr := e.editorialPreconditionPR("[Engineer]", mr, target)
	if cerr != nil {
		return editorialRefusalResult(cerr)
	}

	// Step PR.4: Merge via VCS provider API with a merge commit so the submitted
	// head remains in target ancestry for post-merge proof.
	_, _ = fmt.Fprintf(e.output, "[Engineer] Merging PR #%d via %s API (merge)...\n", pr.Number, provider)
	mergeCommit, err := e.prProvider.MergePR(pr, "merge")
	if err != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("PR merge failed for PR #%d: %v", pr.Number, err),
		}
	}

	// Step PR.5: Sync local target branch after remote merge
	if err := e.git.Checkout(target); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to checkout %s after PR merge: %v\n", target, err)
	} else if err := e.git.Pull("origin", target); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to pull %s after PR merge: %v\n", target, err)
	}

	if mergeCommit == "" {
		if sha, err := e.git.Rev("HEAD"); err == nil {
			mergeCommit = sha
		}
	}
	if err := e.git.VerifyPushedCommit("origin", target, mergeCommit); err != nil {
		return ProcessResult{
			Success: false,
			Error:   err.Error(),
		}
	}

	// The provider has landed the PR, so the commit that will carry the note is
	// known: fill it into the descriptor from the precondition and copy the note
	// onto the commit target's history actually shows. A no-op when the reviewed
	// head and the landed merge commit coincide. Like the local-merge path this
	// runs after the push, so a failure here cannot refuse the merge — it
	// escalates instead (copyEditorialNotes).
	if len(landed) > 0 {
		landed[0].LandedCommit = mergeCommit
		e.copyEditorialNotes("[Engineer]", landed, notes, []*MRInfo{mr})
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Successfully merged PR #%d: %s\n", pr.Number, shortSHA(mergeCommit))
	return ProcessResult{
		Success:     true,
		MergeCommit: mergeCommit,
	}
}

func mergeIneligibleResult(format string, args ...interface{}) ProcessResult {
	return ProcessResult{
		Success: false,
		NoMerge: true,
		Error:   fmt.Sprintf(format, args...),
	}
}

// defaultMRRejectionPollInterval is how often watchMRRejection re-checks an
// in-flight gate's MR bead against beads while the gate runs.
const defaultMRRejectionPollInterval = 3 * time.Second

// watchMRRejection returns a context derived from parent that is canceled the
// moment any of ids stops being an open beads issue. runGate's
// exec.CommandContext already kills a gate's whole process group when its
// context is canceled (util.SetProcessGroup) — this just gives that hook
// something to react to before the gate finishes on its own.
//
// Without it, `gt mq reject` on an MR whose gate is already running only
// closes the MR bead; the gate keeps running for however long it takes, the
// caller doesn't recheck MR status until the gate finishes, and a gate slow
// enough (make test can run for minutes) merges and pushes a branch the
// operator already rejected before the recheck ever runs (gt-xp2b4, gt-55fvl:
// gt-wisp-dz1 rejected at 16:27:10Z, refinery gate for it merged+pushed at
// 16:36:40Z anyway).
//
// The returned cancel must be called once the watched section ends, whether
// or not it was this watcher that ended it — the caller checks watchCtx.Err()
// immediately after, so calling cancel first would erase the very signal it
// is checking for.
func (e *Engineer) watchMRRejection(parent context.Context, ids ...string) (context.Context, context.CancelFunc) {
	watchCtx, cancel := context.WithCancel(parent)
	live := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			live = append(live, id)
		}
	}
	if len(live) == 0 || e.beads == nil {
		return watchCtx, cancel
	}

	interval := e.mrRejectionPollInterval
	if interval <= 0 {
		interval = defaultMRRejectionPollInterval
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				for _, id := range live {
					issue, err := e.beads.Show(id)
					if err != nil || issue == nil {
						// A lookup hiccup or a since-deleted bead is not
						// itself a rejection signal — don't kill a gate on it.
						continue
					}
					if beads.IssueStatus(strings.TrimSpace(issue.Status)) != beads.StatusOpen {
						cancel()
						return
					}
				}
			}
		}
	}()
	return watchCtx, cancel
}

// rejectedMidRun reports whether watchCtx was canceled by watchMRRejection
// rather than by parent itself — the distinction that tells the caller a gate
// failure means "the MR was rejected while this ran" versus an ordinary
// cancellation the caller already knows about (e.g. a shutdown).
func rejectedMidRun(parent, watchCtx context.Context) bool {
	return watchCtx.Err() != nil && parent.Err() == nil
}

// recheckMRStillMergeable re-verifies mr is still eligible to merge into
// target. closeOnReject controls what happens to the MR bead when it is
// not: true (the normal single-MR/pre-push path) closes it with a
// "rejected: ..." reason via rejectMRBeforeMerge, since those checks run
// immediately before an actual merge attempt and a failure there is a real
// verdict. false (used by the batch path's first-pass eligibility recheck,
// before any MR has been touched) leaves the bead completely untouched —
// the caller is expected to just drop the MR from this batch and let it be
// reconsidered later, since an eligibility miss this early is not yet a
// verdict and closing here was destructive for a routine, later-tolerated
// case (gt-di2t: a transient polecat's own pending_mr self-close).
func (e *Engineer) recheckMRStillMergeable(mr *MRInfo, target string, closeOnReject bool) ProcessResult {
	if mr == nil {
		return ProcessResult{Success: false, Error: "merge request is missing"}
	}
	reject := func(reason string) ProcessResult { return e.rejectMRBeforeMerge(mr, reason, closeOnReject) }

	// gt-w2jc: independent defense-in-depth check, at merge time rather than
	// only at submit time (done.go/mq_submit.go's resolveMRTarget). Runs
	// first, ahead of every other check, so any future path that constructs
	// an MR bead without going through those submit paths still can't merge
	// a branch into itself.
	if branch := strings.TrimSpace(mr.Branch); branch != "" && branch == strings.TrimSpace(target) {
		return reject(fmt.Sprintf("MR target %s equals source branch %s", target, branch))
	}

	sourceIssue := strings.TrimSpace(mr.SourceIssue)
	if sourceIssue == "" {
		if e.isSyntheticMergeMechanicsMR(mr) {
			return ProcessResult{Success: true}
		}
		return reject("MR has missing source_issue")
	}

	fieldCommit := ""
	if mrID := strings.TrimSpace(mr.ID); mrID != "" && !e.isSyntheticMergeMechanicsMR(mr) {
		mrIssue, err := e.beads.Show(mrID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return mergeIneligibleResult("MR %s no longer exists", mrID)
			}
			return ProcessResult{Success: false, Error: fmt.Sprintf("pre-push recheck MR %s: %v", mrID, err)}
		}
		if mrIssue == nil {
			return mergeIneligibleResult("MR %s no longer exists", mrID)
		}
		if beads.IssueStatus(strings.TrimSpace(mrIssue.Status)) != beads.StatusOpen {
			return mergeIneligibleResult("MR %s status is %s", mrID, mrIssue.Status)
		}
		if beads.HasLabel(mrIssue, "gt:owned-direct") {
			return reject("MR is owned-direct")
		}

		fields := beads.ParseMRFields(mrIssue)
		if fields == nil {
			return reject("MR has missing merge-request fields")
		}
		if closeReason := strings.TrimSpace(fields.CloseReason); closeReason != "" {
			if strings.EqualFold(closeReason, string(CloseReasonMerged)) {
				if err := e.closeMRWithReason(mr, string(CloseReasonMerged)); err != nil {
					return ProcessResult{Success: false, Error: fmt.Sprintf("failed to close already-merged MR %s: %v", mrID, err)}
				}
				return mergeIneligibleResult("MR close_reason is %s", closeReason)
			}
			return reject(fmt.Sprintf("MR close_reason is %s", closeReason))
		}
		if fields.Branch != "" && mr.Branch != "" && fields.Branch != mr.Branch {
			return reject(fmt.Sprintf("MR branch changed from %s to %s", mr.Branch, fields.Branch))
		}
		if strings.TrimSpace(fields.Target) == "" {
			return reject("MR has missing target")
		}
		if fields.Target != target {
			return reject(fmt.Sprintf("MR target changed from %s to %s", target, fields.Target))
		}
		if fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name) {
			return reject(fmt.Sprintf("MR belongs to rig %s", fields.Rig))
		}
		if strings.TrimSpace(fields.SourceIssue) == "" {
			return reject("MR has missing source_issue")
		}
		if fields.SourceIssue != sourceIssue {
			return reject(fmt.Sprintf("MR source_issue changed from %s to %s", sourceIssue, fields.SourceIssue))
		}
		fieldCommit = strings.TrimSpace(fields.CommitSHA)
		sourceIssue = fields.SourceIssue
	}

	if eligibility := e.recheckMRSourceStillMergeable(mr, sourceIssue, closeOnReject); !eligibility.Success {
		return eligibility
	}
	if strings.TrimSpace(mr.ID) != "" && !e.isSyntheticMergeMechanicsMR(mr) {
		if fieldCommit == "" {
			return reject("MR has missing commit_sha")
		}
		if mr.CommitSHA == "" {
			mr.CommitSHA = fieldCommit
		} else if fieldCommit != strings.TrimSpace(mr.CommitSHA) {
			return reject(fmt.Sprintf("MR commit_sha changed from %s to %s", shortSHA(mr.CommitSHA), shortSHA(fieldCommit)))
		}
	}
	return ProcessResult{Success: true}
}

func (e *Engineer) isSyntheticMergeMechanicsMR(mr *MRInfo) bool {
	return e.testAllowSyntheticMRs && mr != nil && strings.HasPrefix(strings.TrimSpace(mr.ID), "mr-") && strings.TrimSpace(mr.SourceIssue) == ""
}

// rejectMRBeforeMerge reports mr as ineligible to merge for reason. When
// closeOnReject is true it also closes the MR bead ("rejected: <reason>") —
// see recheckMRStillMergeable's doc comment for when that is and isn't
// appropriate. When false, the bead is left untouched and this is purely a
// reporting call.
func (e *Engineer) rejectMRBeforeMerge(mr *MRInfo, reason string, closeOnReject bool) ProcessResult {
	if !closeOnReject {
		return mergeIneligibleResult("%s", reason)
	}
	if err := e.closeIneligibleMR(mr, reason); err != nil {
		mrID := "<missing>"
		if mr != nil && mr.ID != "" {
			mrID = mr.ID
		}
		return ProcessResult{Success: false, Error: fmt.Sprintf("failed to close ineligible MR %s: %v", mrID, err)}
	}
	return mergeIneligibleResult("%s", reason)
}

func (e *Engineer) recheckMRSourceStillMergeable(mr *MRInfo, sourceIssue string, closeOnReject bool) ProcessResult {
	reject := func(reason string) ProcessResult { return e.rejectMRBeforeMerge(mr, reason, closeOnReject) }
	issue, err := e.beads.Show(sourceIssue)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return reject(fmt.Sprintf("source_issue %s is missing", sourceIssue))
		}
		return ProcessResult{Success: false, Error: fmt.Sprintf("pre-push recheck source_issue %s: %v", sourceIssue, err)}
	}
	if issue == nil {
		return reject(fmt.Sprintf("source_issue %s is missing", sourceIssue))
	}
	if beads.IssueStatus(issue.Status).IsTerminal() && !beads.IsPendingMergeCloseReason(issue.CloseReason, mr.ID) {
		// A closed source issue is not automatically a rejection: every
		// polecat on a rig can be transient, so gt done routinely closes the
		// source issue right after creating this exact MR, seconds before
		// the MR ever reaches the queue (gt-di2t). IsPendingMergeCloseReason
		// only tolerates that specific self-close — closed for any other
		// reason (wontfix, duplicate, superseded by a different MR) still
		// rejects here.
		return reject(fmt.Sprintf("source_issue %s status is %s", sourceIssue, issue.Status))
	}
	if reason := beads.ConcreteWorkIssueRejectReason(issue); reason != "" {
		return reject(fmt.Sprintf("source_issue %s is not concrete (%s)", sourceIssue, reason))
	}
	if unchecked := beads.HasUncheckedCriteria(issue); unchecked > 0 {
		return reject(fmt.Sprintf("source_issue %s has %d unchecked acceptance criteria", sourceIssue, unchecked))
	}
	if af := beads.ParseAttachmentFields(issue); af != nil {
		switch {
		case af.NoMerge:
			return reject(fmt.Sprintf("source_issue %s has no_merge=true", sourceIssue))
		case af.ReviewOnly:
			return reject(fmt.Sprintf("source_issue %s has review_only=true", sourceIssue))
		case strings.EqualFold(strings.TrimSpace(af.MergeStrategy), "local"):
			return reject(fmt.Sprintf("source_issue %s has merge_strategy=local", sourceIssue))
		}
	}
	return ProcessResult{Success: true}
}

func (e *Engineer) acquireMainPushSlot(ctx context.Context) (string, error) {
	slotID, err := e.mergeSlotEnsureExists()
	if err != nil {
		return "", fmt.Errorf("ensure merge slot exists: %w", err)
	}

	// The conflict-resolution path holds the slot with holder "rigName/refinery".
	// Both push and conflict-resolution run in the same single-threaded refinery
	// agent, so if our own rig holds the slot for conflict resolution, we can
	// safely proceed without re-acquiring — no concurrent push is possible.
	selfConflictHolder := e.rig.Name + "/refinery"

	backoff := e.mergeSlotRetryBackoff
	if backoff == 0 {
		backoff = 500 * time.Millisecond
	}

	// acquireOnce performs one attempt: it reports the holder to release, or
	// the holder that refused us, or neither when the conflict-resolution path
	// owns the slot. The holder is stamped per attempt so its encoded age is
	// the lease's real age, retries included.
	acquireOnce := func() (acquired string, refusedBy string, ok bool, err error) {
		seq := atomic.AddUint64(&mergeSlotSeq, 1)
		holder := fmt.Sprintf("%s/refinery/push/%d-%d", e.rig.Name, time.Now().UnixNano(), seq)

		status, err := e.mergeSlotAcquire(holder, false)
		if err != nil {
			return "", "", false, fmt.Errorf("acquire merge slot %s (%s): %w", slotID, holder, err)
		}
		if status == nil {
			return "", "", false, fmt.Errorf("acquire merge slot %s (%s): empty status", slotID, holder)
		}
		if status.Available || status.Holder == holder {
			return holder, "", true, nil
		}
		// Slot held by our own conflict-resolution path — safe to proceed.
		if status.Holder == selfConflictHolder {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Merge slot held by conflict-resolution path, proceeding\n")
			return "", "", true, nil // No holder to release — conflict-resolution owns the slot
		}
		return "", status.Holder, false, nil
	}

	for attempt := 0; attempt <= e.mergeSlotMaxRetries; attempt++ {
		if attempt > 0 {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Merge slot held, retrying in %v (attempt %d/%d)...\n", backoff, attempt, e.mergeSlotMaxRetries)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			backoff = min(backoff*2, 10*time.Second)
		}

		acquired, refusedBy, ok, err := acquireOnce()
		if err != nil {
			return "", err
		}
		if ok {
			return acquired, nil
		}

		// A lease its owner abandoned mid-push never clears on its own, so
		// take it now instead of waiting out a backoff for it (gt-pp44).
		if e.reclaimStalePushLease(refusedBy) {
			acquired, _, ok, err = acquireOnce()
			if err != nil {
				return "", err
			}
			if ok {
				return acquired, nil
			}
		}
	}

	return "", fmt.Errorf("merge slot %s: %w after %d retries", slotID, errMergeSlotTimeout, e.mergeSlotMaxRetries)
}

// ValidateTestCommand validates that a test command is safe to execute.
// TestCommand comes from the rig's operator-controlled config.json, not from
// user input or PR branches. This validation provides defense-in-depth for the
// trusted infrastructure config path.
func ValidateTestCommand(cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("test command must not be empty")
	}
	return nil
}

// checkGateContamination scans for orphaned test 'dolt sql-server' processes
// and, if any are present, writes a warning to e.output naming them. It
// returns true when contamination was detected, so callers can prefix a
// subsequent failure with a "CONTAMINATED:" marker — evidence that a
// timeout or hang below may not be attributable to the diff under test
// (gt-twil). Best-effort: a scan error is logged, not treated as
// contamination.
func (e *Engineer) checkGateContamination() bool {
	if e.findOrphanDoltServersFn == nil {
		return false
	}
	orphans, err := e.findOrphanDoltServersFn()
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: dolt orphan server scan failed: %v\n", err)
		return false
	}
	if len(orphans) == 0 {
		return false
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] WARNING: %d orphaned dolt sql-server process(es) present before gate run — "+
		"CONTAMINATED: a hang, timeout, or failure below may be caused by this, not the diff under test\n", len(orphans))
	for _, o := range orphans {
		_, _ = fmt.Fprintf(e.output, "[Engineer]   PID %d ppid=%d (%s): %s\n", o.PID, o.PPID, o.Reason, o.ConfigPath)
	}
	return true
}

// runTests runs the configured test command and returns the result.
func (e *Engineer) runTests(ctx context.Context) ProcessResult {
	if err := ValidateTestCommand(e.config.TestCommand); err != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("invalid test command: %v", err),
		}
	}

	contaminated := e.checkGateContamination()

	// Run the test command with retries for flaky tests
	maxRetries := e.config.RetryFlakyTests
	if maxRetries < 1 {
		maxRetries = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Retrying tests (attempt %d/%d)...\n", attempt, maxRetries)
		}

		// Trust boundary: TestCommand comes from rig's config.json (operator-controlled
		// infrastructure config), not from PR branches or user input. Shell execution
		// is intentional for flexibility (pipes, env vars, etc).
		_, _ = fmt.Fprintf(e.output, "[Engineer] Executing test command: %s\n", e.config.TestCommand)
		cmd := exec.CommandContext(ctx, "sh", "-c", e.config.TestCommand) //nolint:gosec // G204: TestCommand is from trusted rig config
		// SetProcessGroup, not SetDetachedProcessGroup: the group's Cancel hook
		// is what reaches the test binary below the shell, which would
		// otherwise outlive the deadline and compete with the retry for the
		// same containers (gt-6t43).
		util.SetProcessGroup(cmd)
		cmd.Dir = e.workDir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		if err == nil {
			return ProcessResult{Success: true}
		}
		lastErr = err

		// Check if context was canceled
		if ctx.Err() != nil {
			return ProcessResult{
				Success: false,
				Error:   "test run canceled",
			}
		}
	}

	errMsg := fmt.Sprintf("tests failed after %d attempts: %v", maxRetries, lastErr)
	if contaminated {
		errMsg = "CONTAMINATED: " + errMsg
	}
	return ProcessResult{
		Success:     false,
		TestsFailed: true,
		Error:       errMsg,
	}
}

// runGate executes a single quality gate command and returns the result.
func (e *Engineer) runGate(ctx context.Context, name string, gate *GateConfig) GateResult {
	start := time.Now()

	if strings.TrimSpace(gate.Cmd) == "" {
		return GateResult{
			Name:    name,
			Success: false,
			Error:   "gate command is empty",
			Elapsed: time.Since(start),
		}
	}

	// Apply per-gate timeout if configured
	gateCtx := ctx
	if gate.Timeout > 0 {
		var cancel context.CancelFunc
		gateCtx, cancel = context.WithTimeout(ctx, gate.Timeout)
		defer cancel()
	}

	// golangci-lint shares one module-wide lock, so a gate running it gets
	// waited out rather than reported as the MR's own failure (gt-ijqw).
	if isGolangciLintGate(name, gate) {
		return e.runGolangciLintGate(gateCtx, name, gate, start)
	}

	cmd := exec.CommandContext(gateCtx, "sh", "-c", gate.Cmd) //nolint:gosec // G204: Gate commands are from trusted rig config
	// SetProcessGroup, not SetDetachedProcessGroup: a gate killed at its
	// timeout must take the children of its shell with it, or a live suite
	// keeps the gate's pipes and its containers past the release (gt-6t43).
	util.SetProcessGroup(cmd)
	cmd.Dir = e.workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	elapsed := time.Since(start)

	if err == nil {
		return GateResult{
			Name:    name,
			Success: true,
			Elapsed: elapsed,
		}
	}

	errMsg := fmt.Sprintf("%v", err)
	if gateCtx.Err() == context.DeadlineExceeded {
		errMsg = fmt.Sprintf("timed out after %v", gate.Timeout)
	}
	if stderrStr := strings.TrimSpace(stderr.String()); stderrStr != "" {
		// Cap stderr to avoid huge error messages
		if len(stderrStr) > 500 {
			stderrStr = stderrStr[:500] + "..."
		}
		errMsg = fmt.Sprintf("%s: %s", errMsg, stderrStr)
	}

	return GateResult{
		Name:    name,
		Success: false,
		Error:   errMsg,
		Elapsed: elapsed,
	}
}

// isGolangciLintGate reports whether a gate runs golangci-lint, the one gate
// command with a module-wide lock. The command names it when it can; the name
// is a fallback for a rig whose command is a wrapper (`make lint` here),
// matched exactly rather than by substring so an `eslint` or `lintian` gate is
// not routed through golangci-lint's retry and wording (om-gate attempt 1
// minor).
func isGolangciLintGate(name string, gate *GateConfig) bool {
	if strings.Contains(strings.ToLower(gate.Cmd), "golangci-lint") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(name), "lint")
}

// runGolangciLintGate runs a gate whose command is golangci-lint, waiting the
// module lock out instead of reporting a contended lint as the MR's own
// failure.
//
// ctx carries the gate's own budget, which bounds every attempt and every wait
// together, rather than each attempt getting a fresh one (gt-xsty).
func (e *Engineer) runGolangciLintGate(ctx context.Context, name string, gate *GateConfig, start time.Time) GateResult {
	attempt := func() lintlock.Attempt {
		cmd := exec.CommandContext(ctx, "sh", "-c", gate.Cmd) //nolint:gosec // G204: Gate commands are from trusted rig config
		// SetProcessGroup, not SetDetachedProcessGroup: retrying under a held
		// lint lock must not leave the previous attempt's children running
		// beside the new one (gt-6t43).
		util.SetProcessGroup(cmd)
		cmd.Dir = e.workDir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		return lintlock.Attempt{Err: err, Output: strings.TrimSpace(stderr.String() + stdout.String())}
	}

	onRetry := func(attemptNo, attempts int, wait time.Duration) {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Gate %q: another golangci-lint holds the lock (attempt %d/%d); retrying in %s\n", name, attemptNo, attempts, wait.Round(time.Second))
	}

	outcome := lintlock.Retry(ctx, attempt, onRetry)
	elapsed := time.Since(start)

	if outcome.Err == nil {
		return GateResult{Name: name, Success: true, Elapsed: elapsed}
	}

	return GateResult{
		Name:    name,
		Success: false,
		Error:   gateFailureDetail(gate, ctx, outcome),
		Elapsed: elapsed,
	}
}

// gateFailureDetail is what a failed golangci-lint gate reports. Only a lint
// that ran to completion and reported findings may ask for findings to be
// fixed; the other outcomes linted nothing, and a culprit MR blamed for one is
// the misattribution this retry exists to prevent (gt-ijqw).
//
// The verdict comes from the FINAL attempt's own output rather than from the
// retry count, so a lint that contended once and then found real errors is
// forwarded as findings — the opposite report sends the agent to re-run it into
// the same errors (om-gate attempt 1 major).
func gateFailureDetail(gate *GateConfig, ctx context.Context, outcome lintlock.Outcome) string {
	switch {
	case outcome.Contended:
		return fmt.Sprintf("another golangci-lint held the lock %s (a concurrent gate, polecat, or the refinery's own lint) — nothing was linted and no finding is reported; re-run the gate once the other lint finishes", outcome.LockWait())
	case outcome.Unfinished:
		return "golangci-lint stopped without reporting findings — nothing was linted and no finding is reported; a concurrent golangci-lint holding the module lock is the likeliest reason it never finished, so re-run the gate once other lints have"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("the lint was killed at its %s budget without finishing — nothing was linted and no finding is reported; a concurrent golangci-lint holding the module lock is the likeliest reason it never finished, so re-run the gate once other lints have", gate.Timeout.Round(time.Second))
	default:
		return withGateOutput("fix the lint findings before resubmitting", outcome.Output)
	}
}

// withGateOutput appends a failed gate's own output to detail, capped so one
// noisy gate cannot bury the merge queue's log.
func withGateOutput(detail, output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return detail
	}
	const max = 500
	if len(output) > max {
		output = output[:max] + "..."
	}
	return fmt.Sprintf("%s: %s", detail, output)
}

// runGates executes all pre-merge gates (backward-compatible entry point).
func (e *Engineer) runGates(ctx context.Context) ProcessResult {
	return e.runGatesForPhase(ctx, GatePhasePreMerge)
}

// runGatesForPhase executes gates matching the given phase.
// Gates run in parallel if GatesParallel is true; otherwise sequentially.
// Any single gate failure means overall failure.
func (e *Engineer) runGatesForPhase(ctx context.Context, phase GatePhase) ProcessResult {
	// Filter gates for this phase. Empty phase is treated as pre-merge (default).
	gates := make(map[string]*GateConfig)
	for name, gc := range e.config.Gates {
		gatePhase := gc.Phase
		if gatePhase == "" {
			gatePhase = GatePhasePreMerge
		}
		if gatePhase == phase {
			gates[name] = gc
		}
	}
	if len(gates) == 0 {
		return ProcessResult{Success: true}
	}

	contaminated := e.checkGateContamination()

	// Sort gate names for deterministic ordering
	names := make([]string, 0, len(gates))
	for name := range gates {
		names = append(names, name)
	}
	sort.Strings(names)

	parallel := e.config.GatesParallel && phase == GatePhasePreMerge // post-squash always sequential
	_, _ = fmt.Fprintf(e.output, "[Engineer] Running %d %s gate(s) (parallel=%v)\n", len(names), phase, parallel)

	var results []GateResult

	if parallel {
		results = make([]GateResult, len(names))
		var wg sync.WaitGroup
		for i, name := range names {
			wg.Add(1)
			go func(idx int, gateName string) {
				defer wg.Done()
				_, _ = fmt.Fprintf(e.output, "[Engineer] Gate %q: starting (%s)\n", gateName, gates[gateName].Cmd)
				results[idx] = e.runGate(ctx, gateName, gates[gateName])
			}(i, name)
		}
		wg.Wait()
	} else {
		for _, name := range names {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Gate %q: starting (%s)\n", name, gates[name].Cmd)
			result := e.runGate(ctx, name, gates[name])
			results = append(results, result)
			if !result.Success {
				// Sequential mode: stop on first failure
				break
			}
		}
	}

	// Report results
	var failures []string
	for _, r := range results {
		if r.Success {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Gate %q: passed (%v)\n", r.Name, r.Elapsed.Truncate(time.Millisecond))
		} else {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Gate %q: FAILED (%v) - %s\n", r.Name, r.Elapsed.Truncate(time.Millisecond), r.Error)
			failures = append(failures, fmt.Sprintf("%s: %s", r.Name, r.Error))
		}
	}

	if len(failures) > 0 {
		errMsg := fmt.Sprintf("quality gates failed: %s", strings.Join(failures, "; "))
		if contaminated {
			errMsg = "CONTAMINATED: " + errMsg
		}
		return ProcessResult{
			Success:     false,
			TestsFailed: true,
			Error:       errMsg,
		}
	}

	_, _ = fmt.Fprintln(e.output, "[Engineer] All quality gates passed")
	return ProcessResult{Success: true}
}

// syncCrewWorkspaces pulls latest changes to all crew workspaces.
// This ensures crew members have access to newly merged code without manual sync.
func (e *Engineer) syncCrewWorkspaces() {
	crewGit := git.NewGit(e.rig.Path)
	crewMgr := crew.NewManager(e.rig, crewGit)

	workers, err := crewMgr.List()
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to list crew workspaces: %v\n", err)
		return
	}

	if len(workers) == 0 {
		return
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Syncing %d crew workspace(s)...\n", len(workers))

	for _, worker := range workers {
		result, err := crewMgr.Pristine(worker.Name)
		if err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to sync crew/%s: %v\n", worker.Name, err)
			continue
		}
		if result.Pulled {
			_, _ = fmt.Fprintf(e.output, "[Engineer] ✓ Synced crew/%s\n", worker.Name)
		} else if result.PullError != "" {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: crew/%s pull failed: %s\n", worker.Name, result.PullError)
		}
	}
}

// ProcessMRInfo processes a merge request from MRInfo.
func (e *Engineer) ProcessMRInfo(ctx context.Context, mr *MRInfo) ProcessResult {
	// MR fields are directly on the struct
	_, _ = fmt.Fprintln(e.output, "[Engineer] Processing MR:")
	_, _ = fmt.Fprintf(e.output, "  Branch: %s\n", mr.Branch)
	_, _ = fmt.Fprintf(e.output, "  Target: %s\n", mr.Target)
	_, _ = fmt.Fprintf(e.output, "  Worker: %s\n", mr.Worker)
	_, _ = fmt.Fprintf(e.output, "  Source: %s\n", mr.SourceIssue)

	// Phase 3: Check pre-verification fast-path.
	// If the polecat already rebased onto the target and ran gates, and the target
	// hasn't moved since, we can skip running gates entirely (~5s merge).
	skipGates := e.resolveFastPath(mr)

	// Use the shared merge logic
	return e.doMerge(ctx, mr, skipGates)
}

// resolveFastPath decides whether the pre-verification fast-path applies to
// mr: mechanical gates are skipped only when the polecat's verification is
// (a) against the target's current HEAD, (b) against the rig's current
// gate-set (config.GateSetSHA) — a rig that changes its gate commands after a
// polecat verified must not honor the stale claim (om-gate T8) — and (c)
// against a gate set the stamp can actually cover. This governs mechanical
// gates only: editorialPrecondition always runs regardless of skipGates (T6).
func (e *Engineer) resolveFastPath(mr *MRInfo) bool {
	if !mr.PreVerified || mr.PreVerifiedBase == "" {
		return false
	}
	_, _ = fmt.Fprintf(e.output, "  Pre-verified: yes (base=%s)\n", shortSHA(mr.PreVerifiedBase))

	// (c): the polecat's stamp covers the five *_command gates gt done ran, not
	// the named merge_queue.gates that doMerge runs. Post-squash gates are the
	// sharpest case — they validate the merged result, which no polecat-side run
	// reproduces — but a pre-merge named gate is equally uncovered, so any
	// named gate disqualifies the fast-path (gt-ypkc).
	if n := len(e.config.Gates); n > 0 {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Pre-verification cannot cover this rig's %d named gate(s) — running gates normally\n", n)
		return false
	}

	targetHead, err := e.git.Rev("origin/" + mr.Target)
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not resolve origin/%s HEAD: %v (falling through to normal gates)\n", mr.Target, err)
		return false
	}
	if targetHead != mr.PreVerifiedBase {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Pre-verification stale — target moved (%s → %s), running gates normally\n",
			shortSHA(mr.PreVerifiedBase), shortSHA(targetHead))
		return false
	}

	currentGateSHA := e.currentGateSetSHAFn()
	if mr.PreVerifiedGates != currentGateSHA {
		_, _ = fmt.Fprintln(e.output, "[Engineer] Pre-verification stale — gate set changed, running gates normally")
		return false
	}

	_, _ = fmt.Fprintln(e.output, "[Engineer] Pre-verification valid — target unchanged, skipping gates (fast-path)")
	return true
}

// HandleMRInfoSuccess handles a successful merge from MRInfo.
func (e *Engineer) HandleMRInfoSuccess(mr *MRInfo, result ProcessResult) bool {
	workBeadID := resolveMergedWorkBead(e.beads.ForAgentBead(), mergedWorkBeadCloseRequest{
		MRID:        mr.ID,
		Branch:      mr.Branch,
		SourceIssue: mr.SourceIssue,
		AgentBead:   mr.AgentBead,
	})

	// Release the conflict-resolution lease. The bare rig/refinery identity is
	// createConflictResolutionTaskForMR's, and it is held across a dispatched
	// task, so this merge is its only release point; a refusal means this merge
	// held no conflict lease, which is the ordinary case — a push releases the
	// different per-push identity it acquired (gt-pp44).
	holder := e.rig.Name + "/refinery"
	if err := e.mergeSlotRelease(holder); err != nil && !errors.Is(err, beads.ErrMergeSlotNotHolder) {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not release conflict-resolution merge slot: %v\n", err)
	}
	if err := e.verifyMRInfoPostMergeProof(mr); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Post-merge proof failed for %s: %v\n", mr.ID, err)
		return false
	}

	e.checkAndEscalateRubricChange(mr, result)

	// Update and close the MR bead
	if mr.ID != "" && !e.isSyntheticMergeMechanicsMR(mr) {
		if err := e.closeMRWithReason(mr, string(CloseReasonMerged), result.MergeCommit); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Post-merge cleanup failed for %s: %v\n", mr.ID, err)
			return false
		}
	}

	// 1. Close source issue with reference to MR. Resolve before MR close clears
	// active_mr, then close after the real merge success has been recorded.
	closeMergedWorkBead(e.beads, nil, e.output, mergedWorkBeadCloseRequest{
		MRID:        mr.ID,
		Target:      mr.Target,
		SourceIssue: workBeadID,
		MergeCommit: result.MergeCommit,
	})

	// 1.2. Close conflict-resolution tasks that this land has made moot (hq-jnap).
	// Conflict beads otherwise outlive the successful re-land of their content
	// and rot as open issues (re-dlcs/re-4i3b/re-gcii pattern).
	e.closeSupersededConflictArtifacts(mr)

	// 2. Delete source branch (remote first, then local only if still exact).
	// Polecat branches (polecat/*) are always cleaned up — they are ephemeral
	// work branches that should never persist after merge. Other branches
	// respect the DeleteMergedBranches config.
	isPolecat := strings.HasPrefix(mr.Branch, "polecat/")
	if mr.Branch != "" && (e.config.DeleteMergedBranches || isPolecat) {
		// Remote delete — only polecat branches. Non-polecat branches may belong
		// to contributor forks with open upstream PRs; deleting them from origin
		// causes GitHub to auto-close those PRs via head_ref_delete. (GH#2669)
		// gas-fk4: Also skip deletion for polecat branches that have open PRs.
		// When merge_strategy=pr, polecat branches have GitHub PRs that should
		// be closed via gh pr merge (showing "merged"), not via branch deletion
		// (which shows "closed" and destroys the PR audit trail).
		expectedHead := strings.TrimSpace(mr.CommitSHA)
		remoteDeleteSafe := true
		if isPolecat {
			if e.git.HasOpenPullRequest(git.PullRequestRef{URL: mr.PRURL, Number: mr.PRNumber, Branch: mr.Branch, HeadSHA: expectedHead}) {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Skipping remote branch delete for %s: open PR exists (gas-fk4)\n", mr.Branch)
			} else if err := e.git.DeleteRemoteBranchIfAt("origin", mr.Branch, expectedHead); err != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to delete remote branch %s: %v\n", mr.Branch, err)
				remoteDeleteSafe = false
			} else {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Deleted remote branch: %s\n", mr.Branch)
			}
		}
		if remoteDeleteSafe {
			if err := e.deleteLocalBranchIfAt(mr.Branch, expectedHead); err != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to delete local branch %s: %v\n", mr.Branch, err)
			} else {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Deleted local branch: %s\n", mr.Branch)
			}
		}
	}

	// 3. Check and auto-close completed convoys
	// After closing a source issue, its parent convoy may now be complete.
	// Run convoy check to auto-close and notify subscribers.
	e.postMergeConvoyCheck(mr)

	// 4. Nudge mayor about successful merge so dispatcher can unblock
	// dependent work. Without this, mayor only discovers completion by polling.
	// Uses nudge (not mail) to avoid permanent Dolt commits for routine signals (GH#2434).
	nudgeMsg := fmt.Sprintf("MERGED: %s issue=%s branch=%s", mr.ID, mr.SourceIssue, mr.Branch)
	nudgeCmd := exec.Command("gt", "nudge", "mayor/", nudgeMsg)
	util.SetDetachedProcessGroup(nudgeCmd)
	nudgeCmd.Dir = e.workDir
	if err := nudgeCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge mayor about merge: %v\n", err)
	}

	// 4.5. Tell the witness the branch landed, so it can complete the polecat's
	// cleanup wisp and reap the worktree (mail-protocol.md: MERGED,
	// Refinery -> Witness). This runs once per MR here, so both the single-MR
	// path (processSingleMR) and the multi-MR batch path (fastForwardBatch)
	// send it identically — they both funnel through this function (gt-9gjl).
	// Scoped to polecat branches: isPolecat is already computed above for the
	// branch-delete step, and a non-polecat worker (e.g. crew) has no
	// worktree for the witness to reap.
	if isPolecat {
		e.notifyWitnessMerged(mr, result.MergeCommit)
	}

	// 5. Log success
	_, _ = fmt.Fprintf(e.output, "[Engineer] ✓ Merged: %s (commit: %s)\n", mr.ID, result.MergeCommit)
	return true
}

// notifyWitnessMerged sends the documented MERGED message (mail-protocol.md:
// Refinery -> Witness) so the witness can complete the polecat's cleanup wisp
// and reap its worktree. notifyMergedFn overrides this in tests; nil in
// production, where the real mail send runs (mirrors escalateToWitness).
//
// Built with mail.NewMessage rather than protocol.NewMergedMessage to avoid
// an import cycle: refinery -> protocol -> witness -> refinery
// (witness/state_collapse.go imports refinery).
func (e *Engineer) notifyWitnessMerged(mr *MRInfo, mergeCommit string) {
	if e.notifyMergedFn != nil {
		e.notifyMergedFn(mr, mergeCommit)
		return
	}
	if e.router == nil {
		return
	}
	polecatName := strings.TrimPrefix(mr.Worker, "polecats/")
	body := fmt.Sprintf("Branch: %s\nIssue: %s\nPolecat: %s\nRig: %s\nTarget: %s\nMerged-At: %s\nMerge-Commit: %s\n",
		mr.Branch, mr.SourceIssue, polecatName, e.rig.Name, mr.Target, time.Now().Format(time.RFC3339), mergeCommit)
	mergedMsg := mail.NewMessage(
		fmt.Sprintf("%s/refinery", e.rig.Name),
		fmt.Sprintf("%s/witness", e.rig.Name),
		fmt.Sprintf("MERGED %s", polecatName),
		body,
	)
	mergedMsg.Priority = mail.PriorityHigh
	if err := e.router.Send(mergedMsg); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to send MERGED to witness for %s: %v\n", polecatName, err)
	}
}

// checkAndEscalateRubricChange detects whether the merge just landed by mr
// touched the rig's deployed rubric (.om.json), and if so escalates to the
// operator rather than restamping the harness manifest — the batch path's
// half of the CLI post-merge protection (mq.go's
// detectRubricChangeAfterMerge/escalateRubricChange) (gt-7bvf).
func (e *Engineer) checkAndEscalateRubricChange(mr *MRInfo, result ProcessResult) {
	manifest, err := editorial.LoadManifest(e.rig.Path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: loading harness manifest for %s: %v\n", mr.ID, err)
		}
		return
	}
	landedArg := strings.TrimSpace(result.MergeCommit)
	if landedArg == "" {
		landedArg = strings.TrimSpace(mr.CommitSHA)
	}
	touched, rel, sha, err := editorial.RubricChangeAfterMerge(e.git, e.workDir, manifest, landedArg, mr.Target)
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: rubric change check for %s: %v\n", mr.ID, err)
	}
	// touched can be true alongside a non-nil err: an unresolvable rubric
	// path fails closed rather than being read as untouched (gt-7bvf).
	if !touched {
		return
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Rubric changed by %s; escalated to the operator (the manifest is not re-stamped automatically)\n", mr.ID)
	msg := fmt.Sprintf("rubric changed on %s: re-stamp the harness manifest from main content (rig=%s rubric=%s sha256=%s MR=%s)", mr.Target, e.rig.Name, rel, sha, mr.ID)
	escalateCmd := exec.Command("gt", "escalate", "--severity", "medium", "--reason", "rubric-changed", msg)
	util.SetDetachedProcessGroup(escalateCmd)
	escalateCmd.Dir = e.workDir
	if err := escalateCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: rubric-change escalation failed: %v\n", err)
	}
	if mr.ID != "" {
		if commentErr := e.beads.AddComment(mr.ID, fmt.Sprintf("rubric_changed_escalated: %s sha256=%s", rel, sha)); commentErr != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not record rubric_changed_escalated comment on %s: %v\n", mr.ID, commentErr)
		}
	}
}

func (e *Engineer) ensureMRInfoCommitSHA(mr *MRInfo) error {
	if mr == nil {
		return fmt.Errorf("merge request is missing")
	}
	if strings.TrimSpace(mr.CommitSHA) != "" {
		// gt-pwa1: a resolved merge conflict necessarily moves the source
		// branch head, so the SHA recorded at submission time is stale by
		// construction once the conflict-resolution task closes. Adopt the
		// live head in that one case; every other MR keeps the strict check.
		return e.adoptConflictResolvedHead(mr)
	}
	if !e.isSyntheticMergeMechanicsMR(mr) {
		return fmt.Errorf("missing submitted commit_sha")
	}
	if e.git == nil {
		return fmt.Errorf("missing submitted commit_sha and git client is missing")
	}
	branch := strings.TrimSpace(mr.Branch)
	if branch == "" {
		return fmt.Errorf("missing submitted commit_sha and source branch")
	}
	sha, err := e.git.Rev(branch)
	if err != nil {
		return fmt.Errorf("resolve submitted head for %s: %w", branch, err)
	}
	mr.CommitSHA = strings.TrimSpace(sha)
	return nil
}

// adoptConflictResolvedHead refreshes the recorded commit_sha to the live source
// branch head when the conflict-resolution task this refinery dispatched for the
// MR has been closed (gt-pwa1).
//
// Resolving a conflict necessarily rewrites the source branch, but nothing ever
// wrote the new head back to the MR bead: recordConflictTaskOnMR preserves
// commit_sha (engineer.go), and ensureMRInfoCommitSHA's backfill only fires for
// synthetic merge-mechanics MRs. submittedBranchHead then compared the stale SHA
// against the live branch tip and rejected the retry with "source branch X
// changed from submitted head A to B" on every cycle — so the flow the conflict
// task itself documents ("push the resolved branch ... The Refinery will
// automatically retry the merge after you push") could never converge, and
// HandleMRInfoFailure classified the rejection as an ordinary failure
// (Conflict=false), so no conflict task was re-created and no polecat nudged.
//
// The closed conflict task is what authorizes the head to have moved, and only
// that: the task must exist, be closed, and verify as belonging to this MR
// (isConflictTaskForMR). Anything unverifiable leaves the original strict
// comparison in force rather than silently trusting a moved branch.
//
// No gates are skipped by adopting the head — recording a conflict clears the
// pre-verification stamp (gt-nao), so the adopted head is re-gated exactly like
// any other submission. If the "resolution" did not actually resolve anything,
// the merge attempt still reports a conflict and a fresh task is created.
func (e *Engineer) adoptConflictResolvedHead(mr *MRInfo) error {
	// Only the explicit conflict_task_id field authorizes a head move. The
	// BlockedBy fallback used by conflictTaskIDForMR is for post-merge cleanup
	// and would match any unrelated blocker that happens to have closed.
	taskID := strings.TrimSpace(mr.ConflictTaskID)
	if taskID == "" || e.beads == nil || e.git == nil {
		return nil
	}
	branch := strings.TrimSpace(mr.Branch)
	if branch == "" {
		return nil
	}

	task, err := e.beads.Show(taskID)
	if err != nil || task == nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: cannot verify conflict task %s for MR %s (%v) — keeping submitted head %s in force\n",
			taskID, mr.ID, err, shortSHA(mr.CommitSHA))
		return nil
	}
	if task.Status != "closed" {
		return nil
	}
	if !isConflictTaskForMR(task, mr.ID, mr.SourceIssue) {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: refusing to adopt moved head for MR %s: task %s is not a verified conflict task for it\n",
			mr.ID, taskID)
		return nil
	}

	head, err := e.git.Rev("refs/heads/" + branch + "^{commit}")
	if err != nil {
		return fmt.Errorf("resolve conflict-resolved head for %s: %w", branch, err)
	}
	head = strings.TrimSpace(head)
	recorded := strings.TrimSpace(mr.CommitSHA)

	if head == recorded {
		// Branch never moved (or already adopted on an earlier cycle). Drop the
		// spent task link so later calls short-circuit without touching beads.
		return e.setMRConflictHead(mr, head, "")
	}

	if err := e.setMRConflictHead(mr, head, ""); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: adopted conflict-resolved head %s (was %s) from closed task %s\n",
		mr.ID, shortSHA(head), shortSHA(recorded), taskID)
	return nil
}

// setMRConflictHead writes commit_sha and conflict_task_id back to the MR bead
// and mirrors both onto the in-memory MRInfo. Passing an empty taskID clears the
// "open conflict-resolution task" link, since the field only ever names a task
// that is still unresolved.
func (e *Engineer) setMRConflictHead(mr *MRInfo, commitSHA, taskID string) error {
	mrBead, err := e.beads.Show(mr.ID)
	if err != nil {
		return fmt.Errorf("show MR %s to refresh commit_sha: %w", mr.ID, err)
	}
	fields := beads.ParseMRFields(mrBead)
	if fields == nil {
		fields = &beads.MRFields{}
	}
	if strings.TrimSpace(fields.CommitSHA) == commitSHA && fields.ConflictTaskID == taskID {
		mr.CommitSHA = commitSHA
		mr.ConflictTaskID = taskID
		return nil
	}
	fields.CommitSHA = commitSHA
	fields.ConflictTaskID = taskID
	newDesc := beads.SetMRFields(mrBead, fields)
	if err := e.beads.Update(mr.ID, beads.UpdateOptions{Description: &newDesc}); err != nil {
		return fmt.Errorf("refresh commit_sha on MR %s: %w", mr.ID, err)
	}
	mr.CommitSHA = commitSHA
	mr.ConflictTaskID = taskID
	return nil
}

func (e *Engineer) submittedBranchHead(mr *MRInfo) (string, error) {
	if err := e.ensureMRInfoCommitSHA(mr); err != nil {
		return "", err
	}
	if e.git == nil {
		return "", fmt.Errorf("git client is missing")
	}
	branch := strings.TrimSpace(mr.Branch)
	if branch == "" {
		return "", fmt.Errorf("missing source branch")
	}
	commit := strings.TrimSpace(mr.CommitSHA)
	localHead, err := e.git.Rev("refs/heads/" + branch + "^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve source branch %s: %w", branch, err)
	}
	localHead = strings.TrimSpace(localHead)
	if localHead != commit {
		return "", fmt.Errorf("source branch %s changed from submitted head %s to %s", branch, shortSHA(commit), shortSHA(localHead))
	}
	return commit, nil
}

// headRefusal is a refusal to stage a merge because origin does not carry the
// head the MR declares (gt-sda9). The two flags classify it for callers:
// BranchMissing means origin has no such branch at all (the work may be lost —
// escalate), and OriginUnreadable means origin could not be read (transient
// infrastructure — keep the MR queued rather than blaming the submission).
type headRefusal struct {
	BranchMissing    bool
	OriginUnreadable bool
	Err              error
}

// result maps the refusal onto the ProcessResult fields callers route on.
func (r *headRefusal) result() ProcessResult {
	return ProcessResult{
		Success:          false,
		Error:            r.Err.Error(),
		BranchNotFound:   r.BranchMissing,
		OriginUnreadable: r.OriginUnreadable,
	}
}

// assertSubmittedHeadReachableOnOrigin refuses when origin's tip for mr's
// branch does not carry its declared head (gt-sda9). It runs wherever a merge
// path takes a ref from submittedBranchHead, whose local refs/heads check
// cannot see an unpushed head in a rig sharing .repo.git with the polecats.
//
// Only the declared head is asserted: adoptConflictResolvedHead has already
// adopted a head that moved through a closed conflict task, and a
// content-preserving rebase passes the same patch-preservation fallback every
// other push verification uses.
func (e *Engineer) assertSubmittedHeadReachableOnOrigin(mr *MRInfo, head string) *headRefusal {
	if mr == nil {
		return &headRefusal{Err: fmt.Errorf("merge request is missing")}
	}
	if e.git == nil {
		return &headRefusal{OriginUnreadable: true, Err: fmt.Errorf("git client is missing")}
	}
	branch := strings.TrimSpace(mr.Branch)
	if branch == "" {
		return &headRefusal{Err: fmt.Errorf("missing source branch")}
	}
	// Synthetic merge-mechanics MRs exist to exercise the merge paths on
	// branches origin has never heard of, so they carry no submission to guard.
	if e.isSyntheticMergeMechanicsMR(mr) {
		return nil
	}
	commit := strings.TrimSpace(head)
	if commit == "" {
		return &headRefusal{Err: fmt.Errorf("missing submitted commit_sha")}
	}
	tip, err := e.git.PushRemoteBranchTip("origin", branch)
	if err != nil {
		return &headRefusal{OriginUnreadable: true, Err: fmt.Errorf(
			"refusing to gate: cannot read origin/%s to check submitted head %s (%v) — "+
				"a read failure is not a submission failure; the MR stays queued and re-checks next cycle: %s",
			branch, shortSHA(commit), err, mr.ID)}
	}
	tip = strings.TrimSpace(tip)
	if tip == "" {
		return &headRefusal{BranchMissing: true, Err: fmt.Errorf(
			"refusing to gate: origin has no branch %s, so it cannot carry submitted head %s — "+
				"the branch is gone and the work may be lost; push origin %s to restore it, or escalate: %s",
			branch, shortSHA(commit), branch, mr.ID)}
	}
	if err := e.git.VerifyPushedCommitReachableFromPushTarget("origin", branch, commit); err != nil {
		return &headRefusal{Err: fmt.Errorf(
			"refusing to gate: submitted head %s is not reachable from origin/%s (tip %s) — "+
				"the polecat's push has not landed, or the branch moved underneath the MR; "+
				"push origin %s so the declared head lands: %s",
			shortSHA(commit), branch, shortSHA(tip), branch, mr.ID)}
	}
	return nil
}

func (e *Engineer) deleteLocalBranchIfAt(branch, expectedHead string) error {
	branch = strings.TrimSpace(branch)
	expectedHead = strings.TrimSpace(expectedHead)
	if branch == "" {
		return fmt.Errorf("missing source branch")
	}
	if expectedHead == "" {
		return fmt.Errorf("missing submitted commit_sha")
	}
	localHead, err := e.git.Rev("refs/heads/" + branch + "^{commit}")
	if err != nil {
		return fmt.Errorf("resolve local branch head: %w", err)
	}
	if strings.TrimSpace(localHead) != expectedHead {
		return fmt.Errorf("local branch head changed from submitted %s to %s", shortSHA(expectedHead), shortSHA(localHead))
	}
	return e.git.DeleteBranch(branch, false)
}

func requirePullRequestHead(pr *git.PullRequestInfo, expectedHead string) error {
	expectedHead = strings.TrimSpace(expectedHead)
	if expectedHead == "" {
		return fmt.Errorf("missing submitted commit_sha")
	}
	if pr == nil {
		return fmt.Errorf("pull request is missing")
	}
	actualHead := strings.TrimSpace(pr.HeadSHA)
	if actualHead == "" {
		return fmt.Errorf("PR #%d head SHA is missing", pr.Number)
	}
	if actualHead != expectedHead {
		return fmt.Errorf("PR #%d head changed from submitted %s to %s", pr.Number, shortSHA(expectedHead), shortSHA(actualHead))
	}
	return nil
}

func (e *Engineer) verifyMRInfoPostMergeProof(mr *MRInfo) error {
	if mr == nil {
		return fmt.Errorf("merge request is missing")
	}
	if e.git == nil {
		return fmt.Errorf("git client is missing")
	}
	target := strings.TrimSpace(mr.Target)
	if target == "" {
		return fmt.Errorf("missing target branch")
	}
	if source := strings.TrimSpace(mr.Branch); source != "" && source == target {
		return fmt.Errorf("source branch %s matches target branch", source)
	}
	commit := strings.TrimSpace(mr.CommitSHA)
	if commit == "" {
		return fmt.Errorf("missing submitted commit_sha")
	}
	if err := e.git.VerifyPushedCommitReachableFromPushTarget("origin", target, commit); err != nil {
		return fmt.Errorf("target %s does not contain submitted head %s: %w", target, commit, err)
	}
	return nil
}

// HandleMRInfoFailure handles a failed merge from MRInfo.
// For conflicts, creates a resolution task and blocks the MR until resolved.
// For slot timeouts, the MR stays in queue for automatic retry without notifying polecats.
// This enables non-blocking delegation: the queue continues to the next MR.
func (e *Engineer) HandleMRInfoFailure(mr *MRInfo, result ProcessResult) {
	// Slot timeout is transient infrastructure contention — not a build/test/conflict failure.
	// The MR stays in queue and will be retried on the next poll cycle.
	// No polecat notification needed since there's nothing for a worker to fix.
	if result.SlotTimeout {
		_, _ = fmt.Fprintf(e.output, "[Engineer] ✗ Slot timeout: %s - %s\n", mr.ID, result.Error)
		_, _ = fmt.Fprintln(e.output, "[Engineer] MR remains in queue for automatic retry (slot contention)")
		return
	}

	// OriginUnreadable (gt-sda9): the pre-gate reachability assertion could not
	// read origin, so the submission is unjudged. Same handling as a slot
	// timeout — keep the MR queued and retry, and nudge nobody.
	if result.OriginUnreadable {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Origin unreadable while verifying the submitted head of %s: %s\n", mr.ID, result.Error)
		_, _ = fmt.Fprintln(e.output, "[Engineer] MR remains in queue for automatic retry (origin unreadable)")
		return
	}

	// Policy ineligibility is intentional — not a build/test failure.
	// No polecat or mayor notification needed; close any still-open MR so it
	// cannot retry forever after a no-merge/review-only/rejected decision.
	if result.NoMerge {
		reason := strings.TrimSpace(result.Error)
		if reason == "" {
			reason = "merge request is not merge-eligible"
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: %s, dequeued\n", mr.ID, reason)
		if closeErr := e.closeIneligibleMR(mr, reason); closeErr != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to close ineligible MR %s: %v\n", mr.ID, closeErr)
		}
		return
	}

	// NeedsApproval: PR exists but lacks required approving review (merge_strategy=pr).
	// Not a failure — the MR stays in queue and will be retried on the next poll.
	// No polecat notification needed; the PR just needs a human review on GitHub.
	if result.NeedsApproval {
		_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: PR awaiting human approval, will retry next poll\n", mr.ID)
		return
	}

	// EditorialRefused: the om editorial push precondition (om-gate T6) refused
	// this MR — no approve note whose patch-id still matches the range about to
	// land (see editorial_gate.go). A verdict about the diff, not a build/test
	// failure, so the MR stays queued and the next cycle re-checks it; a
	// request_changes verdict has already closed the MR (rejectEditorialVerdict).
	// No polecat or mayor nudge — the worker has nothing to fix — and no
	// dead-worker recovery, which would otherwise re-dispatch reviewed work
	// every cycle the same unchanged refusal stands.
	if result.EditorialRefused {
		_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: editorial precondition refused (%s), will retry next poll\n", mr.ID, result.EditorialReason)
		return
	}

	// WorktreeExternallyDirty: the merge never started, so nothing here is a
	// verdict about the MR. Nudging the worker would send it to fix work that
	// is not broken, and dead-worker recovery would re-dispatch it; the MR
	// stays queued and the blockage goes to the witness until a human clears
	// the worktree.
	if result.WorktreeExternallyDirty {
		_, _ = fmt.Fprintf(e.output, "[Engineer] MERGES REFUSED: %s\n", result.Error)
		e.reportExternallyDirtyWorktree(result.Error)
		return
	}

	// Branch-not-found: the remote branch doesn't exist. This can mean either
	// the branch was cleanly cherry-picked to target, OR the polecat's work was
	// lost (e.g., worktree in /tmp wiped by reboot before gt done pushed).
	// Escalate to mayor so lost work can be re-dispatched (gas-556).
	if result.BranchNotFound {
		_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: branch %s not found on remote — escalating to mayor (possible work loss)\n", mr.ID, mr.Branch)
		mayorMsg := fmt.Sprintf("BRANCH_MISSING: MR %s branch=%s issue=%s worker=%s — branch not on origin, work may be lost; re-dispatch if needed",
			mr.ID, mr.Branch, mr.SourceIssue, mr.Worker)
		mayorCmd := exec.Command("gt", "nudge", "mayor/", mayorMsg)
		mayorCmd.Dir = e.workDir
		if err := mayorCmd.Run(); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge mayor about missing branch: %v\n", err)
		}
		return
	}

	// Nudge polecat directly about the merge failure.
	// Previously sent MERGE_FAILED mail to witness (which relayed to polecat),
	// but that created permanent Dolt commits for routine protocol signals.
	// The witness discovers merge failures from MR bead status during patrol.
	failureType := "build"
	if result.Conflict {
		failureType = "conflict"
	} else if result.TestsFailed {
		failureType = "tests"
	}
	polecatName := strings.TrimPrefix(mr.Worker, "polecats/")
	nudgeTarget := fmt.Sprintf("%s/%s", e.rig.Name, polecatName)
	nudgeMsg := fmt.Sprintf("MERGE_FAILED: branch=%s issue=%s type=%s error=%s — fix and resubmit with 'gt done'",
		mr.Branch, mr.SourceIssue, failureType, result.Error)
	nudgeCmd := exec.Command("gt", "nudge", nudgeTarget, nudgeMsg)
	util.SetDetachedProcessGroup(nudgeCmd)
	nudgeCmd.Dir = e.workDir
	if err := nudgeCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge %s about merge failure: %v\n", polecatName, err)
	} else {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Nudged %s about merge failure (%s)\n", polecatName, failureType)
	}

	// Nudge mayor about merge failure so dispatcher can unblock or reassign
	// dependent work immediately. Mirrors the success nudge in HandleMRInfoSuccess.
	mayorMsg := fmt.Sprintf("MERGE_FAILED: %s issue=%s branch=%s type=%s", mr.ID, mr.SourceIssue, mr.Branch, failureType)
	mayorCmd := exec.Command("gt", "nudge", "mayor/", mayorMsg)
	util.SetDetachedProcessGroup(mayorCmd)
	mayorCmd.Dir = e.workDir
	if err := mayorCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge mayor about merge failure: %v\n", err)
	}

	// gt-tc0: a transient polecat exits at gt done and its source bead is
	// closed, so the nudge above lands nowhere when the session is gone —
	// the MR would just re-queue and re-gate the unchanged branch forever.
	// If the worker has no live session, hand the source bead to the deacon
	// redispatch pipeline so the work actually resumes. Conflicts are
	// excluded: they already get a dispatchable conflict-resolution task.
	if !result.Conflict && e.recoverDeadWorker != nil {
		e.recoverDeadWorker(deadWorkerRecoveryRequest{
			MRID:          mr.ID,
			Branch:        mr.Branch,
			Target:        mr.Target,
			SourceIssue:   mr.SourceIssue,
			Worker:        mr.Worker,
			RigName:       e.rig.Name,
			FailureType:   failureType,
			ErrorMsg:      result.Error,
			AttemptNumber: mr.RetryCount + 1,
		})
	}

	// If this was a conflict, create a conflict-resolution task for dispatch
	// and block the MR until the task is resolved (non-blocking delegation)
	if result.Conflict {
		retryCount := mr.RetryCount + 1
		conflictSHA, revErr := e.git.Rev("origin/" + mr.Target)
		if revErr != nil {
			conflictSHA = "unknown-sha"
		}
		taskID, err := e.createConflictResolutionTaskForMR(mr, result)
		if err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to create conflict resolution task: %v\n", err)
		} else if taskID != "" {
			// Block the MR on the conflict resolution task using beads dependency
			// When the task closes, the MR unblocks and re-enters the ready queue
			if err := e.beads.AddDependency(mr.ID, taskID); err != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to block MR on task: %v\n", err)
			} else {
				if err := e.recordConflictTaskOnMR(mr, taskID, retryCount, conflictSHA); err != nil {
					_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record conflict task on MR %s: %v\n", mr.ID, err)
				} else {
					mr.ConflictTaskID = taskID
					mr.RetryCount = retryCount
				}
				_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s blocked on conflict task %s (non-blocking delegation)\n", mr.ID, taskID)
			}
		}
	}

	// Log the failure - MR stays in queue but may be blocked
	_, _ = fmt.Fprintf(e.output, "[Engineer] ✗ Failed: %s - %s\n", mr.ID, result.Error)
	if mr.BlockedBy != "" {
		_, _ = fmt.Fprintln(e.output, "[Engineer] MR blocked pending conflict resolution - queue continues to next MR")
	} else {
		_, _ = fmt.Fprintln(e.output, "[Engineer] MR remains in queue for retry")
	}
}

func (e *Engineer) closeIneligibleMR(mr *MRInfo, reason string) error {
	return e.closeMRWithReason(mr, "rejected: "+reason)
}

func (e *Engineer) closeMRWithReason(mr *MRInfo, closeReason string, mergeCommit ...string) error {
	_, err := e.closeMRWithReasonResult(mr, closeReason, mergeCommit...)
	return err
}

// closeMRWithReasonResult is closeMRWithReason reporting the close outcome.
// A caller that follows the close with work of its own — dead-worker recovery
// on a rejection (see rejectReviewedCandidate) — needs to know whether THIS
// call was the one that closed the MR, since another path that closed it first
// has already run whatever recovery it needed (gt-bsmp).
func (e *Engineer) closeMRWithReasonResult(mr *MRInfo, closeReason string, mergeCommit ...string) (*terminalMRCloseResult, error) {
	if mr == nil || strings.TrimSpace(mr.ID) == "" {
		return &terminalMRCloseResult{}, nil
	}
	var commit string
	if len(mergeCommit) > 0 {
		commit = mergeCommit[0]
	}
	var expected *MergeRequest
	if normalizedMRCloseReason(closeReason) == string(CloseReasonMerged) {
		expected = mergeRequestFromMRInfo(mr)
	}
	result, err := closeTerminalMR(e.beads, mr.ID, terminalMRCloseOptions{
		Reason:        closeReason,
		MergeCommit:   commit,
		AgentBeadHint: mr.AgentBead,
		MissingOK:     true,
		ExpectedMR:    expected,
	})
	if err != nil {
		return result, err
	}
	if result.Closed {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Closed MR bead: %s (%s)\n", mr.ID, closeReason)
	}
	if result.AgentActiveMRClearErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to clear agent bead %s active_mr: %v\n", result.AgentBead, result.AgentActiveMRClearErr)
	}
	return result, nil
}

func mergeRequestFromMRInfo(mr *MRInfo) *MergeRequest {
	if mr == nil {
		return nil
	}
	return &MergeRequest{
		ID:           mr.ID,
		Branch:       mr.Branch,
		Worker:       mr.Worker,
		AgentBead:    mr.AgentBead,
		IssueID:      mr.SourceIssue,
		TargetBranch: mr.Target,
		CommitSHA:    mr.CommitSHA,
		PRURL:        mr.PRURL,
		PRNumber:     mr.PRNumber,
	}
}

func normalizedMRCloseReason(closeReason string) string {
	closeReason = strings.TrimSpace(closeReason)
	lower := strings.ToLower(closeReason)
	if strings.HasPrefix(lower, "rejected:") {
		return string(CloseReasonRejected)
	}
	if strings.HasPrefix(lower, "superseded") {
		return string(CloseReasonSuperseded)
	}
	if strings.HasPrefix(lower, "conflict") {
		return string(CloseReasonConflict)
	}
	return closeReason
}

// createConflictResolutionTaskForMR creates a dispatchable task for resolving merge conflicts.
// This task will be picked up by bd ready and can be slung to a fresh polecat (spawned on demand).
// Returns the created task's ID for blocking the MR until resolution.
//
// Task format:
//
//	Title: Resolve merge conflicts: <original-issue-title>
//	Type: task
//	Priority: inherit from original (ZFC: agent decides boost strategy)
//	Parent: original MR bead
//	Description: metadata including branch, conflict SHA, etc.
//
// Merge Slot Integration:
// Before creating a conflict resolution task, we acquire the merge-slot for this rig.
// This serializes conflict resolution - only one polecat can resolve conflicts at a time.
// If the slot is already held, we skip creating the task and let the MR stay in queue.
// When the current resolution completes and merges, the slot is released.
func (e *Engineer) createConflictResolutionTaskForMR(mr *MRInfo, _ ProcessResult) (string, error) { // result unused but kept for future merge diagnostics
	// === MERGE SLOT GATE: Serialize conflict resolution ===
	// Ensure merge slot exists (idempotent)
	slotID, err := e.mergeSlotEnsureExists()
	slotHolder := "" // tracks acquired slot for cleanup on error
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not ensure merge slot: %v\n", err)
		// Continue anyway - slot is optional for now
	} else {
		// Try to acquire the merge slot
		holder := e.rig.Name + "/refinery"
		status, err := e.mergeSlotAcquire(holder, false)
		if status != nil && !status.Available && status.Holder != "" && status.Holder != holder {
			// A per-push lease its owner abandoned defers every conflict task
			// forever, and when no push follows, nothing else reclaims it
			// (gt-pp44) — so clear it and contend for real.
			if e.reclaimStalePushLease(status.Holder) {
				retryStatus, retryErr := e.mergeSlotAcquire(holder, false)
				switch {
				case retryErr != nil:
					err = retryErr
				case retryStatus != nil:
					status = retryStatus
				}
			}
		}
		switch {
		case err != nil:
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not acquire merge slot: %v\n", err)
			// Continue anyway - slot is optional
		case status == nil:
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: merge slot returned nil status\n")
			// Continue anyway - slot is optional
		case !status.Available && status.Holder != "" && status.Holder != holder:
			// Slot is held by someone else - skip creating the task
			// The MR stays in queue and will retry when slot is released
			_, _ = fmt.Fprintf(e.output, "[Engineer] Merge slot held by %s - deferring conflict resolution\n", status.Holder)
			_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s will retry after current resolution completes\n", mr.ID)
			return "", nil // Not an error - just deferred
		default:
			slotHolder = holder
			_, _ = fmt.Fprintf(e.output, "[Engineer] Acquired merge slot: %s\n", slotID)
		}
	}
	// Release slot on error to prevent permanent blockage
	releaseSlotOnError := func() {
		if slotHolder != "" {
			_ = e.mergeSlotRelease(slotHolder)
		}
	}

	// Get the current main SHA for conflict tracking
	mainSHA, err := e.git.Rev("origin/" + mr.Target)
	if err != nil {
		mainSHA = "unknown-sha"
	}

	// Get the original issue title if we have a source issue
	originalTitle := mr.SourceIssue
	if mr.SourceIssue != "" {
		if sourceIssue, err := e.beads.Show(mr.SourceIssue); err == nil && sourceIssue != nil {
			originalTitle = sourceIssue.Title
		}
	}

	// ZFC: pass raw priority. Agent decides boost strategy.

	// Increment retry count for tracking
	retryCount := mr.RetryCount + 1

	// Build the task description with metadata
	description := conflictTaskDescription(mr, mr.Branch, mr.Target, shortSHA(mainSHA), retryCount)

	// Create the conflict resolution task
	taskTitle := ConflictTaskTitlePrefix + originalTitle
	task, err := e.beads.Create(beads.CreateOptions{
		Title:       taskTitle,
		Labels:      []string{"gt:task"},
		Priority:    mr.Priority,
		Description: description,
		Actor:       e.rig.Name + "/refinery",
		Rig:         e.rig.Name, // Ensure task lands in the rig's database (gt-7y7)
	})
	if err != nil {
		releaseSlotOnError()
		return "", fmt.Errorf("creating conflict resolution task: %w", err)
	}

	// gt-gpy: Validate task bead landed in the rig's database (warning only).
	townRoot := filepath.Dir(e.rig.Path)
	if prefixErr := beads.ValidateRigPrefix(townRoot, e.rig.Name, task.ID); prefixErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] WARNING: conflict task prefix mismatch: %v\n", prefixErr)
	}

	// The conflict task's ID is returned so the MR can be blocked on it.
	// When the task closes, the MR unblocks and re-enters the ready queue.

	_, _ = fmt.Fprintf(e.output, "[Engineer] Created conflict resolution task: %s (P%d)\n", task.ID, task.Priority)

	return task.ID, nil
}

// conflictTaskDescription builds the markdown body of a conflict-resolution
// task. The "- Key: value" metadata lines are parsed back out by
// conflictTaskMetadata/isConflictTaskForMR, so they must stay in that shape;
// the instructions below them are prose and are deliberately colon-free so they
// cannot be mistaken for metadata.
//
// Step 6 is load-bearing for gt-pwa1: closing the task is the signal
// adoptConflictResolvedHead waits for before it adopts the branch's new head,
// and the MR stays blocked on this task until it closes.
func conflictTaskDescription(mr *MRInfo, branch, target, conflictSHA string, retryCount int) string {
	return fmt.Sprintf(`Resolve merge conflicts for branch %s

## Metadata
- Original MR: %s
- Branch: %s
- Conflict with: %s@%s
- Original issue: %s
- Retry count: %d

## Instructions
1. Check out the branch: git checkout %s
2. Merge target without rewriting branch history: git merge --no-ff origin/%s
3. Resolve conflicts in your editor
4. Complete the merge: git add . && git commit
5. Push the resolved branch: git push origin %s
6. Close this task: bd close <this-task-id>

Closing this task is what releases the MR. The Refinery adopts the branch's new
head once it sees this task closed, then retries the merge. Pushing without
closing leaves the MR blocked, and closing without pushing leaves the merge
failing against the old head. Both steps are required. The adopted head is
re-gated, because recording the conflict cleared any pre-verification stamp, so
this is not a way to skip checks.`,
		branch,
		mr.ID,
		branch,
		target, conflictSHA,
		mr.SourceIssue,
		retryCount,
		branch,
		target,
		branch,
	)
}

func (e *Engineer) recordConflictTaskOnMR(mr *MRInfo, taskID string, retryCount int, conflictSHA string) error {
	mrBead, err := e.beads.Show(mr.ID)
	if err != nil {
		return err
	}
	mrFields := beads.ParseMRFields(mrBead)
	if mrFields == nil {
		mrFields = &beads.MRFields{}
	}
	mrFields.ConflictTaskID = taskID
	mrFields.RetryCount = retryCount
	mrFields.LastConflictSHA = conflictSHA
	// A conflict means the target has diverged since any prior pre-verification
	// run. Clear it so a future fast-path never skips gates against a base that
	// no longer reflects reality (gt-nao: pre_verified was left stale here).
	mrFields.PreVerified = false
	mrFields.PreVerifiedAt = ""
	mrFields.PreVerifiedBase = ""
	newDesc := beads.SetMRFields(mrBead, mrFields)
	return e.beads.Update(mr.ID, beads.UpdateOptions{Description: &newDesc})
}

// RecordConflict is the CLI-facing entry point for recording a merge conflict
// against an MR bead: it creates a dispatchable conflict-resolution task,
// blocks the MR on that task, and writes conflict_task_id/last_conflict_sha/
// retry_count (and clears stale pre_verified metadata) onto the MR wisp.
//
// This mirrors the Conflict branch of HandleMRInfoFailure but is reachable
// independently of the fully-mechanized merge path, so the mol-refinery-patrol
// formula (which drives conflict handling today) can call it via
// `gt mq record-conflict` instead of hand-rolling `bd create` and leaving the
// MR wisp's conflict/pre-verification fields out of sync (gt-nao).
//
// Returns the created task ID, or "" with a nil error if the merge slot was
// busy and creation was deferred to a later cycle (not a failure).
func (e *Engineer) RecordConflict(mrID string) (string, error) {
	mrBead, err := e.beads.Show(mrID)
	if err != nil {
		return "", fmt.Errorf("show MR %s: %w", mrID, err)
	}
	fields := beads.ParseMRFields(mrBead)
	if fields == nil {
		return "", fmt.Errorf("MR %s has no parsable MR fields", mrID)
	}
	mr := issueToMRInfo(mrBead, fields)

	taskID, err := e.createConflictResolutionTaskForMR(mr, ProcessResult{Conflict: true})
	if err != nil {
		return "", fmt.Errorf("create conflict resolution task for %s: %w", mrID, err)
	}
	if taskID == "" {
		// Merge slot was busy — deferred, not an error. MR stays in queue.
		return "", nil
	}

	if err := e.beads.AddDependency(mr.ID, taskID); err != nil {
		return taskID, fmt.Errorf("block MR %s on task %s: %w", mrID, taskID, err)
	}

	conflictSHA, revErr := e.git.Rev("origin/" + mr.Target)
	if revErr != nil {
		conflictSHA = "unknown-sha"
	}
	if err := e.recordConflictTaskOnMR(mr, taskID, mr.RetryCount+1, conflictSHA); err != nil {
		return taskID, fmt.Errorf("record conflict metadata on %s: %w", mrID, err)
	}

	return taskID, nil
}

// closeSupersededConflictArtifacts closes conflict-resolution tasks made moot
// by a successful land of the source issue (hq-jnap). Two cases:
//  1. The merged MR's own conflict task is still open — the conflict was
//     resolved out-of-band (force-push) without `bd close`, so the task rots.
//  2. Another open MR carries the same source issue (a re-land) — its conflict
//     task is now pointless because the content is on the target branch.
//
// Superseded sibling MRs are closed only when their conflict task verifies it
// belongs to that MR/source issue; this avoids unblocking stale duplicate MRs.
// All operations are best-effort; failures are logged and don't affect the merge.
func (e *Engineer) closeSupersededConflictArtifacts(merged *MRInfo) {
	e.closeConflictTaskIfOpen(conflictTaskIDForMR(merged), merged.ID, merged.ID, merged.SourceIssue)

	if merged.SourceIssue == "" {
		return
	}
	all, err := e.ListAllOpenMRs()
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: conflict-artifact sweep skipped (list MRs): %v\n", err)
		return
	}
	for _, other := range all {
		if other.ID == merged.ID || other.SourceIssue != merged.SourceIssue {
			continue
		}
		if !e.closeConflictTaskIfOpen(conflictTaskIDForMR(other), other.ID, merged.ID, merged.SourceIssue) {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Note: open MR %s shares source issue %s just merged via %s, but had no verified conflict task to close\n",
				other.ID, merged.SourceIssue, merged.ID)
			continue
		}
		reason := fmt.Sprintf("superseded by %s", merged.ID)
		if err := e.closeMRWithReason(other, reason); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to close superseded MR %s: %v\n", other.ID, err)
		} else {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Closed superseded MR %s: %s\n", other.ID, reason)
		}
	}
}

func conflictTaskIDForMR(mr *MRInfo) string {
	if mr == nil {
		return ""
	}
	if mr.ConflictTaskID != "" {
		return mr.ConflictTaskID
	}
	return mr.BlockedBy
}

// closeConflictTaskIfOpen closes a conflict-resolution task if it is still open.
func (e *Engineer) closeConflictTaskIfOpen(taskID, taskMRID, landedMRID, sourceIssue string) bool {
	if taskID == "" {
		return false
	}
	task, err := e.beads.Show(taskID)
	if err != nil || task == nil {
		return false
	}
	if !isConflictTaskForMR(task, taskMRID, sourceIssue) {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: refusing to close unverified conflict task %s for MR %s\n", taskID, taskMRID)
		return false
	}
	if task.Status == string(beads.StatusClosed) {
		return true
	}
	reason := fmt.Sprintf("conflict moot: %s landed (MR %s)", sourceIssue, landedMRID)
	if err := e.beads.CloseWithReason(reason, taskID); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to close moot conflict task %s: %v\n", taskID, err)
		return false
	} else {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Closed moot conflict task: %s (%s)\n", taskID, reason)
	}
	return true
}

func isConflictTaskForMR(task *beads.Issue, mrID, sourceIssue string) bool {
	if task == nil || task.Description == "" || mrID == "" {
		return false
	}
	metadata := conflictTaskMetadata(task.Description)
	if metadata["Original MR"] != mrID {
		return false
	}
	return sourceIssue == "" || metadata["Original issue"] == sourceIssue
}

// ConflictTaskTitlePrefix is the title prefix createConflictResolutionTaskForMR
// gives every conflict-resolution task. Both the prefix and the "- Original MR:"
// metadata line are the conflict-task signature; either alone is too weak to
// identify one (gt-rv8h).
const ConflictTaskTitlePrefix = "Resolve merge conflicts: "

// ConflictTaskOriginalMR reports the merge request a conflict-resolution task
// was created for, or "" when issue is not one of those tasks.
//
// This is the reverse of isConflictTaskForMR: given the task bead a
// conflict-resolution polecat has just finished, it names the MR that task was
// dispatched for. Closing that task is the MR's blocked->ready transition, so
// this is how a completion that created no MR of its own finds the MR it
// released (gt-rv8h) — see internal/cmd/done_conflict_wake.go.
//
// Both the title prefix and the metadata line must agree. conflictTaskMetadata
// records a value for every line it can split on a colon, and conflict-task
// descriptions carry polecat-molecule attachment metadata whose values contain
// prose with colons, so the title gate is what keeps a stray "Original MR:" in
// unrelated prose from being read as a conflict task.
func ConflictTaskOriginalMR(issue *beads.Issue) string {
	if issue == nil || !strings.HasPrefix(issue.Title, ConflictTaskTitlePrefix) {
		return ""
	}
	return strings.TrimSpace(conflictTaskMetadata(issue.Description)["Original MR"])
}

func conflictTaskMetadata(description string) map[string]string {
	metadata := make(map[string]string)
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

// issueToMRInfo converts a beads issue (with parsed MR fields) into an MRInfo.
// Shared by ListReadyMRs, ListBlockedMRs, and ListAllOpenMRs.
func issueToMRInfo(issue *beads.Issue, fields *beads.MRFields) *MRInfo {
	// Parse convoy created_at if present
	var convoyCreatedAt *time.Time
	if fields.ConvoyCreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, fields.ConvoyCreatedAt); err == nil {
			convoyCreatedAt = &t
		}
	}

	// Parse issue timestamps
	var createdAt, updatedAt time.Time
	if issue.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, issue.CreatedAt); err == nil {
			createdAt = t
		}
	}
	if issue.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, issue.UpdatedAt); err == nil {
			updatedAt = t
		}
	}

	// Parse pre-verification timestamp if present
	var preVerifiedAt time.Time
	if fields.PreVerifiedAt != "" {
		if t, err := time.Parse(time.RFC3339, fields.PreVerifiedAt); err == nil {
			preVerifiedAt = t
		}
	}

	return &MRInfo{
		ID:                    issue.ID,
		Branch:                fields.Branch,
		Target:                fields.Target,
		SourceIssue:           fields.SourceIssue,
		Worker:                fields.Worker,
		Rig:                   fields.Rig,
		Title:                 issue.Title,
		Priority:              issue.Priority,
		AgentBead:             fields.AgentBead,
		CommitSHA:             fields.CommitSHA,
		PRURL:                 fields.PRURL,
		PRNumber:              fields.PRNumber,
		RetryCount:            fields.RetryCount,
		ConflictTaskID:        fields.ConflictTaskID,
		ConvoyID:              fields.ConvoyID,
		ConvoyCreatedAt:       convoyCreatedAt,
		PreVerified:           fields.PreVerified,
		PreVerifiedAt:         preVerifiedAt,
		PreVerifiedBase:       fields.PreVerifiedBase,
		PreVerifiedGates:      fields.PreVerifiedGates,
		PreVerifiedExit:       fields.PreVerifiedExit,
		PreVerifiedLog:        fields.PreVerifiedLog,
		EditorialReviewedHead: fields.EditorialReviewedHead,
		CreatedAt:             createdAt,
		UpdatedAt:             updatedAt,
		Assignee:              issue.Assignee,
		Labels:                issue.Labels,
	}
}

// firstOpenBlocker returns the first unresolved blocker ID for an issue.
func (e *Engineer) firstOpenBlocker(issue *beads.Issue) string {
	return beads.FirstUnresolvedBlockerID(issue)
}

// ErrDuplicateBranchMRs marks the duplicate-branch anomaly (gt-k1qf): two or
// more open MRs claiming one branch, the invariant conflict resolution must
// never break. See DuplicateBranchMRs.
var ErrDuplicateBranchMRs = errors.New("two open MRs claim the same branch")

// ListReadyMRs returns unclaimed, unblocked MRs, sorted by priority (highest
// first). An MR sharing its branch with another open MR is excluded rather
// than gated (gt-k1qf) and reported separately by ListQueueAnomalies as a
// duplicate-branch anomaly, so one colliding pair no longer blocks every
// other MR in the rig.
//
// Uses bd list instead of bd ready because MRs are ephemeral beads and
// bd ready filters out ephemeral issues (see gt-t5t6y). This matches the
// pattern used by ListBlockedMRs and ListAllOpenMRs.
func (e *Engineer) ListReadyMRs() ([]*MRInfo, error) {
	// Query beads for all open merge-request issues.
	// Cannot use ReadyWithType here because bd ready excludes ephemeral beads,
	// and MRs are ephemeral by design. Use List + manual blocker check instead.
	issues, err := e.beads.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1, // No priority filter
		Rig:      e.rig.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("querying beads for merge-requests: %w", err)
	}

	// gt-k1qf: never gate an MR that shares its branch with another open MR
	// (see DuplicateBranchMRs for why this check exists).
	duplicateIDs := duplicateBranchMRIDs(issues, e.rig.Name)

	// Convert beads issues to MRInfo
	var mrs []*MRInfo
	for _, issue := range issues {
		// Skip closed MRs (workaround for bd list not respecting --status filter)
		if issue.Status != "open" {
			continue
		}

		if duplicateIDs[issue.ID] {
			continue
		}

		// Skip blocked MRs (replaces bd ready's blocker filtering)
		if beads.HasUnresolvedBlockers(issue) {
			continue
		}

		// Belt-and-suspenders: skip MRs labeled gt:owned-direct.
		// These MRs shouldn't exist (gt done skips MR creation for owned+direct
		// convoys), but if one slips through, the refinery should not process it.
		if beads.HasLabel(issue, "gt:owned-direct") {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Skipping MR %s: owned+direct convoy (belt-and-suspenders)\n", issue.ID)
			continue
		}

		fields := beads.ParseMRFields(issue)
		if fields == nil {
			continue // Skip issues without MR fields
		}

		// Filter by rig — wisps are shared across all rigs (GH#2718).
		if fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name) {
			continue
		}

		// Skip if already assigned, unless claim is stale (allows re-claim after crash).
		// NOTE: Only one refinery runs per rig (enforced by ErrAlreadyRunning in
		// manager.go), so concurrent re-claim race conditions are not a concern.
		if issue.Assignee != "" {
			stale, parseErr := isClaimStale(issue.UpdatedAt, e.config.StaleClaimTimeout)
			if parseErr != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not parse UpdatedAt for %s: %v (treating claim as valid)\n",
					issue.ID, parseErr)
			}
			if !stale {
				continue
			}
			_, _ = fmt.Fprintf(e.output, "[Engineer] Stale claim detected: %s (assignee: %s, updated: %s) — eligible for re-claim\n",
				issue.ID, issue.Assignee, issue.UpdatedAt)
		}

		mrs = append(mrs, issueToMRInfo(issue, fields))
	}

	return mrs, nil
}

// ListBlockedMRs returns MRs that are blocked by open tasks.
// Useful for monitoring/reporting.
//
// This queries beads for blocked merge-request issues.
func (e *Engineer) ListBlockedMRs() ([]*MRInfo, error) {
	// Query all merge-request issues (both ready and blocked)
	issues, err := e.beads.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1, // No priority filter
		Rig:      e.rig.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("querying beads for merge-requests: %w", err)
	}

	// Filter for blocked issues (those with open blockers)
	var mrs []*MRInfo
	for _, issue := range issues {
		if issue.Status != "open" {
			continue
		}

		if !beads.HasUnresolvedBlockers(issue) {
			continue
		}

		blockedBy := e.firstOpenBlocker(issue)

		fields := beads.ParseMRFields(issue)
		if fields == nil {
			continue
		}

		// Filter by rig — wisps are shared across all rigs (GH#2718).
		if fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name) {
			continue
		}

		mr := issueToMRInfo(issue, fields)
		mr.BlockedBy = blockedBy
		mrs = append(mrs, mr)
	}

	return mrs, nil
}

// ListAllOpenMRs returns all open merge requests with full raw data.
// Unlike ListReadyMRs/ListBlockedMRs, this performs no filtering — it returns
// claimed, unclaimed, blocked, and unblocked MRs. It also checks branch existence
// so agents can detect orphaned MRs. Designed for agent-side queue health analysis
// (ZFC: Go transports data, agent decides what's interesting).
func (e *Engineer) ListAllOpenMRs() ([]*MRInfo, error) {
	issues, err := e.beads.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1,
		Rig:      e.rig.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("querying beads for merge-requests: %w", err)
	}

	var mrs []*MRInfo
	for _, issue := range issues {
		if issue.Status != "open" {
			continue
		}

		fields := beads.ParseMRFields(issue)
		if fields == nil {
			continue
		}

		// Filter by rig — wisps are shared across all rigs (GH#2718).
		if fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name) {
			continue
		}

		mr := issueToMRInfo(issue, fields)

		// Check branch existence. Local uses this repo's refs/heads, which only
		// covers branches checked out in a worktree of this same repo clone —
		// agents with their own separate clone (e.g. mayor/rig) won't show up
		// here even though their branch is real. Remote must therefore be a
		// live ls-remote query, not the local refs/remotes/origin/* cache,
		// which goes stale whenever nothing has fetched since the branch was
		// pushed (gt-7v66).
		// Warnings here go to os.Stderr, never e.output: `gt refinery ready
		// --all --json` builds this Engineer on the default e.output
		// (os.Stdout) and encodes its JSON result straight to os.Stdout, so
		// a warning on e.output would land before the JSON and corrupt the
		// stream the witness patrol parses for BranchExistsLocal/Remote
		// (gt-bagu).
		var localWarn, remoteWarn string
		mr.BranchExistsLocal, localWarn = safeBranchExistenceCheck(func() (bool, error) { return e.git.BranchExists(fields.Branch) })
		mr.BranchExistsLocalUnknown = localWarn != ""
		if localWarn != "" {
			_, _ = fmt.Fprintf(os.Stderr, "[Engineer] Warning: could not check local branch existence for %s: %s\n", fields.Branch, localWarn)
		}
		mr.BranchExistsRemote, remoteWarn = safeBranchExistenceCheck(func() (bool, error) { return e.git.RemoteBranchExists("origin", fields.Branch) })
		mr.BranchExistsRemoteUnknown = remoteWarn != ""
		if remoteWarn != "" {
			_, _ = fmt.Fprintf(os.Stderr, "[Engineer] Warning: could not check remote branch existence for %s: %s\n", fields.Branch, remoteWarn)
		}
		mr.BlockedBy = e.firstOpenBlocker(issue)

		mrs = append(mrs, mr)
	}

	return mrs, nil
}

// safeBranchExistenceCheck runs a branch-existence check and treats a query
// error as "could not tell" rather than "false": git.BranchExists and
// RemoteBranchExists already fold a genuinely missing branch into (false,
// nil), so check returning an error means the check itself failed to run
// (network hiccup, timeout, a repo in a bad state), not that the branch is
// gone. Reporting false in that case is indistinguishable from a real
// no-branch and silently mislabels a live MR's branch as missing (gt-bagu)
// — so an error reports true (assume present) instead, paired with a
// message the caller can log.
func safeBranchExistenceCheck(check func() (bool, error)) (exists bool, warn string) {
	exists, err := check()
	if err != nil {
		return true, err.Error()
	}
	return exists, ""
}

// ListQueueAnomalies finds stale claims and orphaned branches in open MRs.
// This gives Witness/Refinery patrols deterministic signals for deadlock risk.
func (e *Engineer) ListQueueAnomalies(now time.Time) ([]*MRAnomaly, error) {
	issues, err := e.beads.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1,
		Rig:      e.rig.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("querying beads for merge-requests: %w", err)
	}

	// Filter by rig — wisps are shared across all rigs (GH#2718).
	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		fields := beads.ParseMRFields(issue)
		if fields != nil && fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name) {
			continue
		}
		filtered = append(filtered, issue)
	}

	return detectQueueAnomalies(filtered, now, e.config.StaleClaimWarningAfter, func(branch string) (bool, bool, error) {
		localExists, err := e.git.BranchExists(branch)
		if err != nil {
			return false, false, err
		}
		// Live ls-remote, not the local refs/remotes/origin/* cache: this feeds
		// orphaned-branch detection, which the witness patrol acts on by closing
		// the MR. A stale cache turns a merge-ready branch into a false orphan
		// and discards the work (gt-7v66).
		remoteExists, err := e.git.RemoteBranchExists("origin", branch)
		if err != nil {
			return false, false, err
		}
		return localExists, remoteExists, nil
	}), nil
}

func detectQueueAnomalies(
	issues []*beads.Issue,
	now time.Time,
	warningAfter time.Duration,
	branchExistsFn func(branch string) (localExists bool, remoteExists bool, err error),
) []*MRAnomaly {
	var anomalies []*MRAnomaly

	for _, issue := range issues {
		if issue == nil || issue.Status != "open" {
			continue
		}
		fields := beads.ParseMRFields(issue)
		if fields == nil || fields.Branch == "" {
			continue
		}

		// 1) Stale claim detection.
		if issue.Assignee != "" {
			updatedAt, err := time.Parse(time.RFC3339, issue.UpdatedAt)
			if err == nil {
				age := now.Sub(updatedAt)
				if age >= warningAfter {
					anomalies = append(anomalies, &MRAnomaly{
						ID:       issue.ID,
						Branch:   fields.Branch,
						Type:     "stale-claim",
						Assignee: issue.Assignee,
						Age:      age,
						Detail:   "MR is claimed but not progressing",
					})
				}
			}
		}

		// 2) Orphaned branch detection.
		// ZFC: report raw anomaly data. Agent decides severity.
		localExists, remoteExists, err := branchExistsFn(fields.Branch)
		if err == nil && !localExists && !remoteExists {
			anomalies = append(anomalies, &MRAnomaly{
				ID:     issue.ID,
				Branch: fields.Branch,
				Type:   "orphaned-branch",
				Detail: "MR branch is missing locally and on origin",
			})
		}
	}

	// 3) Duplicate-branch detection (gt-k1qf): report every MR sharing a
	// branch with another open MR, so a patrol that only ever sees "ready"
	// or "blocked" still learns to refuse and escalate the pair. issues is
	// already rig-scoped by ListQueueAnomalies.
	for _, dup := range duplicateBranchMRsInScope(issues) {
		for _, id := range dup.IDs {
			anomalies = append(anomalies, &MRAnomaly{
				ID:     id,
				Branch: dup.Branch,
				Type:   "duplicate-branch",
				Detail: fmt.Sprintf("branch claimed by multiple open MRs: %s", strings.Join(dup.IDs, ", ")),
			})
		}
	}

	return anomalies
}

// DuplicateBranchMR pairs one branch with every open MR ID that claims it,
// sorted for deterministic output. len(IDs) is always >= 2.
type DuplicateBranchMR struct {
	Branch string
	IDs    []string
}

// DuplicateBranchMRs finds branches claimed by more than one open MR in
// issues, scoped to rigName the same way the rest of the queue is (wisps are
// shared across all rigs — GH#2718). Two live MRs for one branch is exactly
// the gt-k1qf failure: whichever the refinery picks up first, the other is
// left either double-processed or stranded past the normal post-merge path
// once the branch actually lands. supersedeOpenMRsForIssue is supposed to
// prevent this by closing the older MR the instant a replacement is created;
// this is the check that catches it if that ever fails silently. Collisions
// are keyed by branch, not by source_issue: two MRs racing to land the same
// branch are unsafe together regardless of which issue(s) they cite (see
// TestDuplicateBranchMRs_DifferentSourceIssuesSameBranchStillCollide).
//
// Shared by ListReadyMRs (excludes the colliding MRs), ListQueueAnomalies
// (reports them for escalation), and `gt mq list` (marks them "duplicate" so
// the single-MR patrol path agrees with the batch path) so every queue view
// treats a collision the same way.
func DuplicateBranchMRs(issues []*beads.Issue, rigName string) []DuplicateBranchMR {
	scoped := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		fields := beads.ParseMRFields(issue)
		if fields != nil && fields.Rig != "" && !strings.EqualFold(fields.Rig, rigName) {
			continue
		}
		scoped = append(scoped, issue)
	}
	return duplicateBranchMRsInScope(scoped)
}

// duplicateBranchMRsInScope groups already rig-scoped open MRs by branch.
// Callers that have already filtered by rig (ListQueueAnomalies) use this
// directly instead of DuplicateBranchMRs to avoid filtering twice.
func duplicateBranchMRsInScope(issues []*beads.Issue) []DuplicateBranchMR {
	byBranch := map[string][]string{}
	var order []string
	for _, issue := range issues {
		if issue == nil || issue.Status != "open" {
			continue
		}
		fields := beads.ParseMRFields(issue)
		if fields == nil || fields.Branch == "" {
			continue
		}
		if _, seen := byBranch[fields.Branch]; !seen {
			order = append(order, fields.Branch)
		}
		byBranch[fields.Branch] = append(byBranch[fields.Branch], issue.ID)
	}

	var dups []DuplicateBranchMR
	for _, branch := range order {
		ids := byBranch[branch]
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		dups = append(dups, DuplicateBranchMR{Branch: branch, IDs: ids})
	}
	return dups
}

// duplicateBranchMRIDs flattens DuplicateBranchMRs into the set of MR IDs
// that must not be gated.
func duplicateBranchMRIDs(issues []*beads.Issue, rigName string) map[string]bool {
	ids := map[string]bool{}
	for _, d := range DuplicateBranchMRs(issues, rigName) {
		for _, id := range d.IDs {
			ids[id] = true
		}
	}
	return ids
}

// FormatDuplicateBranchMRs renders DuplicateBranchMRs' findings for a
// message: one clause per colliding branch, naming every MR that claims it.
func FormatDuplicateBranchMRs(dups []DuplicateBranchMR) string {
	parts := make([]string, 0, len(dups))
	for _, d := range dups {
		parts = append(parts, fmt.Sprintf("branch %s: %s", d.Branch, strings.Join(d.IDs, ", ")))
	}
	return strings.Join(parts, "; ")
}

// ClaimMR claims an MR for processing by setting the assignee field.
// This replaces mrqueue.Claim() for beads-based MRs.
// The workerID is typically the refinery's identifier (e.g., "gastown/refinery").
func (e *Engineer) ClaimMR(mrID, workerID string) error {
	return e.beads.Update(mrID, beads.UpdateOptions{
		Assignee: &workerID,
	})
}

// ReleaseMR releases a claimed MR back to the queue by clearing the assignee.
// This replaces mrqueue.Release() for beads-based MRs.
func (e *Engineer) ReleaseMR(mrID string) error {
	empty := ""
	return e.beads.Update(mrID, beads.UpdateOptions{
		Assignee: &empty,
	})
}

// postMergeConvoyCheck runs convoy completion checks after a successful merge.
//
// When a source issue is closed by a merge, any convoy tracking that issue may
// now be complete (all tracked issues closed). This method:
//  1. Runs `gt convoy check` to auto-close completed convoys and notify subscribers
//  2. For completed convoys with integration branches (swarms), triggers landing
//  3. Cleans up stale polecat branches from completed work
//
// All operations are best-effort: failures are logged but don't affect merge success.
func (e *Engineer) postMergeConvoyCheck(mr *MRInfo) {
	// Find town root from rig path (rig is at ~/gt/<rigname>, town is ~/gt)
	townRoot := filepath.Dir(e.rig.Path)
	townBeads := filepath.Join(townRoot, ".beads")

	// Quick check: does town-level beads exist?
	if _, err := os.Stat(townBeads); os.IsNotExist(err) {
		return
	}

	// Step 1: Run `gt convoy check` to auto-close completed convoys.
	// This handles cross-rig convoy completion: convoys in town beads (hq-*)
	// tracking issues in rig beads (gt-*) won't auto-close via bd close alone.
	closedConvoys := e.checkAndCloseCompletedConvoys(townRoot, townBeads)

	// Step 2: For each closed convoy, check if it has a swarm with an
	// integration branch that needs landing.
	for _, convoy := range closedConvoys {
		e.landConvoySwarm(townRoot, convoy)
	}

	// Step 3: Notify deacon of convoy-eligible merges for immediate feeding.
	// When the merged MR is part of a convoy, send a structured CONVOY_NEEDS_FEEDING
	// protocol message so the deacon can immediately feed the next ready issue
	// instead of waiting for the next patrol cycle (up to 10 minutes).
	e.notifyDeaconConvoyFeeding(mr)

	// Step 4: Clean up stale branches from completed work.
	// Prune remote tracking refs that no longer exist on origin.
	if e.config.DeleteMergedBranches {
		e.pruneStaleRemoteRefs()
	}
}

// notifyDeaconConvoyFeeding sends a CONVOY_NEEDS_FEEDING protocol message to
// the deacon when the merged MR is part of a convoy. This triggers immediate
// convoy feeding instead of waiting for the next deacon patrol cycle (up to
// 10 minutes). An event is also emitted to wake the deacon from await-signal.
func (e *Engineer) notifyDeaconConvoyFeeding(mr *MRInfo) {
	if mr.ConvoyID == "" {
		return
	}

	// Nudge deacon about convoy feeding instead of sending permanent mail.
	// The deacon discovers convoy state from beads on next patrol cycle;
	// this nudge just accelerates discovery.
	nudgeMsg := fmt.Sprintf("CONVOY_NEEDS_FEEDING: convoy=%s issue=%s", mr.ConvoyID, mr.SourceIssue)
	nudgeCmd := exec.Command("gt", "nudge", "deacon", nudgeMsg)
	util.SetDetachedProcessGroup(nudgeCmd)
	nudgeCmd.Dir = e.workDir
	if err := nudgeCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge deacon about convoy feeding for %s: %v\n", mr.ConvoyID, err)
	} else {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Nudged deacon: CONVOY_NEEDS_FEEDING %s\n", mr.ConvoyID)
	}

	// Emit event to wake deacon from await-signal.
	_ = events.LogFeed(events.TypeMail, e.rig.Name+"/refinery", events.MailPayload("deacon/", "CONVOY_NEEDS_FEEDING "+mr.ConvoyID))
}

// convoyInfo holds minimal info about a closed convoy for post-merge processing.
type convoyInfo struct {
	ID          string
	Title       string
	Description string
}

func refineryHasLabel(labels []string, target string) bool {
	for _, label := range labels {
		if label == target {
			return true
		}
	}
	return false
}

// checkAndCloseCompletedConvoys finds and closes convoys where all tracked issues
// are complete. Returns the list of convoys that were closed.
func (e *Engineer) checkAndCloseCompletedConvoys(townRoot, townBeads string) []convoyInfo {
	townReadEnv := beads.BuildReadOnlyPinnedBDEnv(os.Environ(), townBeads)
	townMutationEnv := beads.BuildMutationPinnedBDEnv(os.Environ(), townBeads)
	routingReadEnv := beads.BuildReadOnlyRoutingBDEnv(os.Environ(), townBeads)

	// List all open issues and filter locally so legacy type=convoy beads remain visible.
	listArgs := beads.InjectFlatForListJSON([]string{"list", "--status=open", "--json", "--limit=0"})
	listArgs = beads.MaybePrependAllowStaleWithEnv(townReadEnv, listArgs)
	listCmd := beads.Command(townBeads, townBeads, beads.ReadOnlyPinned, listArgs...)
	var stdout bytes.Buffer
	listCmd.Stdout = &stdout

	if err := listCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to list convoys: %v\n", err)
		return nil
	}

	var convoys []struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Status      string   `json:"status"`
		Description string   `json:"description"`
		IssueType   string   `json:"issue_type"`
		Labels      []string `json:"labels"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &convoys); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to parse convoy list: %v\n", err)
		return nil
	}

	var closed []convoyInfo

	for _, convoy := range convoys {
		if convoy.IssueType != "convoy" && !refineryHasLabel(convoy.Labels, "gt:convoy") {
			continue
		}
		// Get tracked issues for this convoy via bd dep list
		depArgs := beads.MaybePrependAllowStaleWithEnv(townReadEnv, []string{"dep", "list", convoy.ID, "--direction=down", "--type=tracks", "--json"})
		depCmd := beads.Command(townRoot, townBeads, beads.ReadOnlyPinned, depArgs...)
		var depOut bytes.Buffer
		depCmd.Stdout = &depOut

		if err := depCmd.Run(); err != nil {
			continue
		}

		var deps []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(depOut.Bytes(), &deps); err != nil {
			continue
		}

		// Refresh statuses from home rigs (cross-rig lookup)
		allClosed := true
		for _, dep := range deps {
			// Unwrap external:prefix:id format
			depID := dep.ID
			if strings.HasPrefix(depID, "external:") {
				parts := strings.SplitN(depID, ":", 3)
				if len(parts) == 3 {
					depID = parts[2]
				}
			}

			// Get fresh status from home rig via bd show with routing
			showArgs := beads.MaybePrependAllowStaleWithEnv(routingReadEnv, []string{"show", depID, "--json"})
			showCmd := beads.Command(townRoot, townBeads, beads.ReadOnlyRouting, showArgs...)
			var showOut bytes.Buffer
			showCmd.Stdout = &showOut

			if err := showCmd.Run(); err != nil || showOut.Len() == 0 {
				// Can't verify - treat as open to be safe
				allClosed = false
				break
			}

			var issues []struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(showOut.Bytes(), &issues); err != nil || len(issues) == 0 {
				allClosed = false
				break
			}

			if issues[0].Status != "closed" && issues[0].Status != "tombstone" {
				allClosed = false
				break
			}
		}

		if !allClosed {
			continue
		}

		// All tracked issues are complete - close the convoy
		reason := "All tracked issues completed"
		if len(deps) == 0 {
			reason = "Empty convoy — auto-closed as definitionally complete"
		}

		closeArgs := beads.MaybePrependAllowStaleWithEnv(townMutationEnv, []string{"close", convoy.ID, "-r", reason})
		closeCmd := beads.Command(townBeads, townBeads, beads.MutationPinned, closeArgs...)

		if err := closeCmd.Run(); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to close convoy %s: %v\n", convoy.ID, err)
			continue
		}

		_, _ = fmt.Fprintf(e.output, "[Engineer] Auto-closed convoy %s: %s\n", convoy.ID, convoy.Title)
		closed = append(closed, convoyInfo{
			ID:          convoy.ID,
			Title:       convoy.Title,
			Description: convoy.Description,
		})

		// Send convoy completion notifications (owner + notify addresses)
		e.notifyConvoyCompletion(townRoot, convoy.ID, convoy.Title, convoy.Description)
	}

	return closed
}

// notifyConvoyCompletion sends notifications to convoy owner and notify addresses.
func (e *Engineer) notifyConvoyCompletion(townRoot, convoyID, title, description string) {
	fields, shouldNotify := e.claimConvoyCompletionNotification(townRoot, convoyID, description)
	if !shouldNotify {
		return
	}
	for _, addr := range fields.NotificationAddresses() {
		mailCmd := exec.Command("gt", "mail", "send", addr,
			"-s", fmt.Sprintf("🚚 Convoy landed: %s", title),
			"-m", fmt.Sprintf("Convoy %s has completed.\n\nAll tracked issues are now closed.\n\nClosed by: %s/refinery", convoyID, e.rig.Name),
			"--from", "convoy/"+convoyID,
			"--no-notify")
		util.SetDetachedProcessGroup(mailCmd)
		mailCmd.Dir = townRoot
		if err := mailCmd.Run(); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not notify %s: %v\n", addr, err)
		}
	}
}

func (e *Engineer) claimConvoyCompletionNotification(townRoot, convoyID, fallbackDescription string) (*beads.ConvoyFields, bool) {
	townBeads := filepath.Join(townRoot, ".beads")
	description := fallbackDescription

	readEnv := beads.BuildReadOnlyPinnedBDEnv(os.Environ(), townBeads)
	showArgs := beads.MaybePrependAllowStaleWithEnv(readEnv, []string{"show", convoyID, "--json"})
	showCmd := beads.Command(townBeads, townBeads, beads.ReadOnlyPinned, showArgs...)
	var showOut bytes.Buffer
	showCmd.Stdout = &showOut
	if err := showCmd.Run(); err == nil && showOut.Len() > 0 {
		var convoys []struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(showOut.Bytes(), &convoys); err == nil && len(convoys) > 0 {
			description = convoys[0].Description
		}
	}

	fields := beads.ParseConvoyFields(&beads.Issue{Description: description})
	if fields == nil {
		fields = &beads.ConvoyFields{}
	}
	if fields.CompletionNotifiedAt != "" {
		return fields, false
	}

	fields.CompletionNotifiedAt = time.Now().UTC().Format(time.RFC3339)
	newDesc := beads.SetConvoyFields(&beads.Issue{Description: description}, fields)
	mutationEnv := beads.BuildMutationPinnedBDEnv(os.Environ(), townBeads)
	updateArgs := beads.MaybePrependAllowStaleWithEnv(mutationEnv, []string{"update", convoyID, "--description=" + newDesc})
	updateCmd := beads.Command(townBeads, townBeads, beads.MutationPinned, updateArgs...)
	if err := updateCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not record convoy completion notification state for %s: %v\n", convoyID, err)
		return fields, false
	}

	return fields, true
}

// landConvoySwarm checks if a completed convoy has an associated swarm with an
// integration branch, and triggers landing if so.
func (e *Engineer) landConvoySwarm(townRoot string, convoy convoyInfo) {
	// ZFC: Use typed accessor instead of parsing description text
	fields := beads.ParseConvoyFields(&beads.Issue{Description: convoy.Description})
	var moleculeID string
	if fields != nil {
		moleculeID = fields.Molecule
	}

	if moleculeID == "" {
		return // No swarm/molecule associated with this convoy
	}

	// Check if the molecule has an integration branch (swarm/* pattern)
	integrationBranch := fmt.Sprintf("swarm/%s", moleculeID)
	branchExists, err := e.git.BranchExists(integrationBranch)
	if err != nil || !branchExists {
		// Also check remote
		remoteExists, _ := e.git.RemoteTrackingBranchExists("origin", integrationBranch)
		if !remoteExists {
			return // No integration branch to land
		}
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Landing integration branch %s for convoy %s...\n", integrationBranch, convoy.ID)

	// Use gt swarm land to perform the landing
	landCmd := exec.Command("gt", "swarm", "land", moleculeID)
	util.SetDetachedProcessGroup(landCmd)
	landCmd.Dir = townRoot
	var landOut, landErr bytes.Buffer
	landCmd.Stdout = &landOut
	landCmd.Stderr = &landErr

	if err := landCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to land swarm %s: %v (%s)\n",
			moleculeID, err, strings.TrimSpace(landErr.String()))
		return
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] ✓ Landed integration branch for convoy %s\n", convoy.ID)
}

// pruneStaleRemoteRefs prunes remote tracking refs that no longer exist on origin.
// This cleans up refs from branches that were deleted on the remote after merge.
func (e *Engineer) pruneStaleRemoteRefs() {
	if err := e.git.FetchPrune("origin"); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to prune stale remote refs: %v\n", err)
	}
}
