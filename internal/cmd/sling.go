package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/telemetry"
	"github.com/steveyegge/gastown/internal/witness"
	"github.com/steveyegge/gastown/internal/workspace"
)

var slingCmd = &cobra.Command{
	Use:     "sling <bead-or-formula> [target]",
	GroupID: GroupWork,
	Short:   "Assign work to an agent (THE unified work dispatch command)",
	Long: `Sling work onto an agent's hook and start working immediately.

This is THE command for assigning work in Gas Town. It handles:
  - Existing agents (mayor, crew, witness, refinery)
  - Auto-spawning polecats when target is a rig
  - Dispatching to dogs (Deacon's helper workers)
  - Formula instantiation and wisp creation
  - Auto-convoy creation so the work is tracked

Auto-Convoy:
  When slinging a single issue (not a formula), sling automatically creates
  a convoy to track the work unless --no-convoy is specified. This ensures
  all work appears in 'gt convoy list', even "swarm of one" assignments.

  gt sling gt-abc gastown              # Creates "Work: <issue-title>" convoy
  gt sling gt-abc gastown --no-convoy  # Skip auto-convoy creation

Merge Strategy (--merge):
  Controls how completed work lands. Stored on the auto-convoy.
  gt sling gt-abc gastown --merge=mr      # Merge queue (default)
  gt sling gt-abc gastown --merge=local   # Keep on feature branch

Target Resolution:
  gt sling gt-abc                       # Self (current agent)
  gt sling gt-abc crew                  # Crew worker in current rig
  gt sling gp-abc greenplace               # Auto-spawn polecat in rig
  gt sling gt-abc greenplace/polecats/toast  # Specific polecat: exactly toast, or refused
  gt sling gt-abc greenplace/polecats/toast --create  # ...created as toast if missing
                                        # (new names: lowercase a-z0-9-, >3 chars, not reserved)
  gt sling gt-abc gastown --crew mel    # Crew member mel in gastown
  gt sling gt-abc mayor                 # Mayor
  gt sling gt-abc deacon/dogs           # Auto-dispatch to idle dog
  gt sling gt-abc deacon/dogs/alpha     # Specific dog

Spawning Options (when target is a rig):
  gt sling gp-abc greenplace --create               # Create polecat if missing
  gt sling gp-abc greenplace --force                # Ignore unread mail
  gt sling gp-abc greenplace --account work         # Use specific Claude account

Content Duplicate Guard:
  Before dispatch, the bead's named test functions and file paths are compared
  against the rig's open and recently-closed beads. Two beads describing one
  defect from different vantage points share no keywords, but they do name the
  same tests. A shared test name refuses the sling; a shared file alone warns
  and proceeds. --force overrides.
  gt sling gt-abc greenplace --force                # Sling despite the overlap

Natural Language Args:
  gt sling gt-abc --args "patch release"
  gt sling code-review --args "focus on security"

The --args string is stored in the bead and shown via gt prime. Since the
executor is an LLM, it interprets these instructions naturally.

Stdin Mode (for shell-quoting-safe multi-line content):
  echo "review for security issues" | gt sling gt-abc gastown --stdin
  gt sling gt-abc gastown --stdin <<'EOF'
  Focus on:
  1. SQL injection in query builders
  2. XSS in template rendering
  EOF

  # With --args on CLI, stdin goes to --message:
  echo "Extra context here" | gt sling gt-abc gastown --args "patch release" --stdin

Formula Slinging:
  gt sling mol-release mayor/           # Cook + wisp + attach + nudge
  gt sling code-review --var pr=42

Formula-on-Bead (--on flag):
  gt sling mol-review --on gt-abc       # Apply formula to existing work
  gt sling shiny --on gt-abc crew       # Apply formula, sling to crew

Compare:
  gt hook <bead>      # Just attach (no action)
  gt sling <bead>     # Attach + start now (keep context)
  gt handoff <bead>   # Attach + restart (fresh context)

The propulsion principle: if it's on your hook, YOU RUN IT.

Batch Slinging:
  gt sling gt-abc gt-def gt-ghi gastown   # Sling multiple beads to a rig
  gt sling gt-abc gt-def gastown --max-concurrent 3  # Spawn 3 at a time

  When multiple beads are provided with a rig target, each bead gets its own
  polecat. This parallelizes work dispatch without running gt sling N times.
  Use --max-concurrent to throttle spawn rate and prevent Dolt server overload.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runSling,
}

var (
	slingSubject     string
	slingMessage     string
	slingDryRun      bool
	slingOnTarget    string   // --on flag: target bead when slinging a formula
	slingVars        []string // --var flag: formula variables (key=value)
	slingArgs        string   // --args flag: natural language instructions for executor
	slingStdin       bool     // --stdin: read --message and/or --args from stdin
	slingHookRawBead bool     // --hook-raw-bead: hook raw bead without default formula (expert mode)

	// Flags migrated for polecat spawning (used by sling for work assignment)
	slingCreate        bool   // --create: create polecat if it doesn't exist
	slingForce         bool   // --force: force spawn even if polecat has unread mail
	slingAccount       string // --account: Claude Code account handle to use
	slingAgent         string // --agent: override runtime agent for this sling/spawn
	slingNoConvoy      bool   // --no-convoy: skip auto-convoy creation
	slingOwned         bool   // --owned: mark auto-convoy as caller-managed lifecycle
	slingNoMerge       bool   // --no-merge: skip merge queue on completion (for upstream PRs/human review)
	slingMerge         string // --merge: merge strategy for convoy (mr/local)
	slingNoBoot        bool   // --no-boot: skip wakeRigAgents (avoid witness/refinery boot and lock contention)
	slingMaxConcurrent int    // --max-concurrent: throttle spawn rate in batch mode (spawns N, pauses, spawns N more)
	slingBaseBranch    string // --base-branch: override base branch for polecat worktree
	slingResumeBranch  string // --branch: resume an existing branch instead of creating a fresh one
	slingResumePR      int    // --pr: resume the head branch of an existing PR (resolves via gh)
	slingRalph         bool   // --ralph: enable Ralph Wiggum loop mode for multi-step workflows
	slingFormula       string // --formula: override formula for dispatch (default: mol-polecat-work)
	slingCrew          string // --crew: target a crew member in the specified rig
	slingReviewOnly    bool   // --review-only: mark work as review-only (no merge/commit/push)
	slingActor         string // --actor: override recorded actor (for system/daemon-originated slings)
)

func init() {
	slingCmd.Flags().StringVarP(&slingSubject, "subject", "s", "", "Context subject for the work")
	slingCmd.Flags().StringVarP(&slingMessage, "message", "m", "", "Context message for the work")
	slingCmd.Flags().BoolVarP(&slingDryRun, "dry-run", "n", false, "Show what would be done")
	slingCmd.Flags().StringVar(&slingOnTarget, "on", "", "Apply formula to existing bead (implies wisp scaffolding)")
	slingCmd.Flags().StringArrayVar(&slingVars, "var", nil, "Formula variable (key=value), can be repeated")
	slingCmd.Flags().StringVarP(&slingArgs, "args", "a", "", "Natural language instructions for the executor (e.g., 'patch release')")
	slingCmd.Flags().BoolVar(&slingStdin, "stdin", false, "Read --message and/or --args from stdin (avoids shell quoting issues)")

	// Flags for polecat spawning (when target is a rig)
	slingCmd.Flags().BoolVar(&slingCreate, "create", false, "Create polecat if it doesn't exist")
	slingCmd.Flags().BoolVar(&slingForce, "force", false, "Force spawn even if polecat has unread mail")
	slingCmd.Flags().StringVar(&slingAccount, "account", "", "Claude Code account handle to use")
	slingCmd.Flags().StringVar(&slingAgent, "agent", "", "Override agent/runtime for this sling (e.g., claude, gemini, codex, or custom alias). A polecat_pool seat is honored or the sling is refused; the pool never swaps in the other seat")
	slingCmd.Flags().BoolVar(&slingNoConvoy, "no-convoy", false, "Skip auto-convoy creation for single-issue sling")
	slingCmd.Flags().BoolVar(&slingOwned, "owned", false, "Mark auto-convoy as caller-managed lifecycle (no automatic witness/refinery registration)")
	slingCmd.Flags().BoolVar(&slingHookRawBead, "hook-raw-bead", false, "Hook raw bead without default formula (expert mode)")
	slingCmd.Flags().BoolVar(&slingNoMerge, "no-merge", false, "Skip merge queue on completion (keep work on feature branch for review)")
	slingCmd.Flags().StringVar(&slingMerge, "merge", "", "Merge strategy: mr (merge queue, default), local (keep on branch)")
	slingCmd.Flags().BoolVar(&slingNoBoot, "no-boot", false, "Skip rig boot after polecat spawn (avoids witness/refinery lock contention)")
	slingCmd.Flags().IntVar(&slingMaxConcurrent, "max-concurrent", 0, "Throttle spawn rate: spawn N polecats, pause, then spawn N more (0 = no throttle). Does not limit total concurrent polecats")
	slingCmd.Flags().StringVar(&slingBaseBranch, "base-branch", "", "Override base branch for polecat worktree (e.g., 'develop', 'release/v2')")
	slingCmd.Flags().StringVar(&slingResumeBranch, "branch", "", "Resume work on an existing branch instead of creating a fresh polecat branch (use to fix an existing PR)")
	slingCmd.Flags().IntVar(&slingResumePR, "pr", 0, "Resume work on the head branch of an existing PR (resolved via 'gh pr view'). Mutually exclusive with --branch.")
	slingCmd.Flags().BoolVar(&slingRalph, "ralph", false, "Enable Ralph Wiggum loop mode (fresh context per step, for multi-step workflows)")
	slingCmd.Flags().StringVar(&slingFormula, "formula", "", "Formula to apply (default: mol-polecat-work for polecat targets)")
	slingCmd.Flags().StringVar(&slingCrew, "crew", "", "Target a crew member in the specified rig (e.g., --crew mel with target gastown → gastown/crew/mel)")
	slingCmd.Flags().BoolVar(&slingReviewOnly, "review-only", false, "Mark work as review-only: assignee evaluates and reports back, must NOT merge/commit/push")
	slingCmd.Flags().StringVar(&slingActor, "actor", "", "Override the actor recorded for this sling (e.g. daemon/convoy:<id>). Default: auto-detected from role. For system/daemon-originated dispatch that has no live agent role of its own.")

	slingCmd.AddCommand(slingRespawnResetCmd)
	rootCmd.AddCommand(slingCmd)
}

var slingRespawnResetCmd = &cobra.Command{
	Use:   "respawn-reset <bead-id>",
	Short: "Reset the respawn counter for a bead",
	Long: `Reset the per-bead respawn counter so it can be slung again.

When a bead hits the respawn limit (3 attempts), gt sling blocks further
dispatches to prevent spawn storms. After investigating the root cause,
use this command to allow re-dispatch.`,
	Args: cobra.ExactArgs(1),
	RunE: runSlingRespawnReset,
}

func runSlingRespawnReset(_ *cobra.Command, args []string) error {
	beadID := args[0]
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if err := witness.ResetBeadRespawnCount(townRoot, beadID); err != nil {
		return fmt.Errorf("resetting respawn count for %s: %w", beadID, err)
	}
	fmt.Printf("Reset respawn counter for %s. It can be slung again.\n", beadID)
	return nil
}

func runSling(cmd *cobra.Command, args []string) (retErr error) {
	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}
	// Per-step timing on stderr so a slow dispatch can be attributed (gt-llg8).
	slingSteps = newSlingTimer(os.Stderr)
	defer func() {
		bead, target := "", ""
		if len(args) > 0 {
			bead = args[0]
		}
		if len(args) > 1 {
			target = args[1]
		}
		telemetry.RecordSling(ctx, bead, target, retErr)
	}()
	// The same boundary as executeSling's: a seat the pool claimed for this
	// sling stops standing when the command returns. StartSession drops it on
	// the success path, and the failure paths drop it here — including the ones
	// that return after the spawn without rolling it back, which the rollback
	// paths alone would miss (gt-t8q5).
	defer releasePoolSeatClaim()
	return newSlingRun(slingOptionsFromFlags()).run(ctx, cmd, args)
}

// run is one gt sling: it validates the request, routes it to the batch,
// scheduler, formula, convoy or epic path, or dispatches a single bead
// itself, rolling back a spawned polecat on every exit short of the commit
// point.
func (r *slingRun) run(ctx context.Context, cmd *cobra.Command, args []string) (retErr error) {
	// Polecats cannot sling - check early before writing anything.
	// Check GT_ROLE first: coordinators (mayor, witness, etc.) may have a stale
	// GT_POLECAT in their environment from spawning polecats. Only block if the
	// parsed role is actually polecat (handles compound forms like
	// "gastown/polecats/Toast"). If GT_ROLE is unset, fall back to GT_POLECAT.
	if role := r.getenv("GT_ROLE"); role != "" {
		parsedRole, _, _ := parseRoleString(role)
		if parsedRole == RolePolecat {
			return fmt.Errorf("polecats cannot sling (use gt done for handoff)")
		}
	} else if polecatName := r.getenv("GT_POLECAT"); polecatName != "" {
		return fmt.Errorf("polecats cannot sling (use gt done for handoff)")
	}

	// Validate --merge flag if provided
	if err := validateConvoyMergeFlag(r.opts.merge); err != nil {
		return err
	}

	// Validate --branch / --pr resume flags (gh#3602).
	// These flags reuse an existing branch/PR head instead of creating a fresh
	// polecat branch, letting a polecat continue work on an existing PR.
	if r.opts.resumeBranch != "" && r.opts.resumePR != 0 {
		return fmt.Errorf("--branch and --pr are mutually exclusive")
	}
	if (r.opts.resumeBranch != "" || r.opts.resumePR != 0) && r.opts.baseBranch != "" {
		return fmt.Errorf("--base-branch cannot be combined with --branch or --pr (resume implies starting on the existing branch)")
	}
	if r.opts.resumePR != 0 {
		resolved, err := r.resolvePRBranch(r.opts.resumePR)
		if err != nil {
			return fmt.Errorf("resolving --pr %d: %w", r.opts.resumePR, err)
		}
		r.opts.resumeBranch = resolved
		fmt.Fprintf(r.out, "%s --pr %d resolved to branch %s\n", style.Dim.Render("→"), r.opts.resumePR, resolved)
	}

	// Disable Dolt auto-commit for all bd commands run during sling (gt-u6n6a).
	// Under concurrent load (batch slinging), auto-commits from individual bd writes
	// cause manifest contention and 'database is read only' errors. The Dolt server
	// handles commits — individual auto-commits are unnecessary.
	defer r.autoCommitOff()()

	// Handle --stdin: read message/args from stdin (avoids shell quoting issues)
	if r.opts.stdin {
		if r.opts.message != "" && r.opts.argsText != "" {
			return fmt.Errorf("cannot use --stdin when both --message and --args are already provided")
		}
		data, err := io.ReadAll(r.stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		stdinContent := strings.TrimRight(string(data), "\n")
		if r.opts.argsText == "" {
			// Default: stdin populates --args (the primary instruction channel)
			r.opts.argsText = stdinContent
		} else {
			// --args already set on CLI, stdin goes to --message
			r.opts.message = stdinContent
		}
	}

	// Get town root early - needed for BEADS_DIR when running bd commands
	// This ensures hq-* beads are accessible even when running from polecat worktree
	if r.townErr != nil {
		return fmt.Errorf("finding town root: %w", r.townErr)
	}
	townRoot := r.townRoot
	townBeadsDir := filepath.Join(townRoot, ".beads")

	// Normalize target arguments: trim trailing slashes from target to handle tab-completion
	// artifacts like "gt sling sl-123 slingshot/" → "gt sling sl-123 slingshot"
	// This makes sling more forgiving without breaking existing functionality.
	// Note: Internal agent IDs like "mayor/" are outputs, not user inputs.
	for i := range args {
		args[i] = strings.TrimRight(args[i], "/")
	}

	// --crew flag: expand target from "<rig>" to "<rig>/crew/<name>"
	// e.g., "gt sling gt-abc gastown --crew mel" → target becomes "gastown/crew/mel"
	if r.opts.crew != "" {
		if len(args) < 2 {
			return fmt.Errorf("--crew requires a rig target argument (e.g., gt sling <bead> <rig> --crew %s)", r.opts.crew)
		}
		target := args[len(args)-1]
		args[len(args)-1] = target + "/crew/" + r.opts.crew
	}

	// Validate target format early, before any dispatch path (bead, formula, batch)
	// can trigger resolveTarget side-effects like polecat spawning.
	if len(args) > 1 {
		if err := ValidateTarget(args[len(args)-1]); err != nil {
			return err
		}
	}
	if len(args) == 2 {
		if redirected, err := r.workflowTargetOverride(args); err != nil {
			return err
		} else {
			args = redirected
		}
	}

	// Config-driven dispatch mode: check scheduler.max_polecats
	deferred, deferErr := r.shouldDefer()
	if deferErr != nil {
		return deferErr
	}

	// Batch mode detection: multiple beads with optional rig target
	// Pattern A (explicit rig):  gt sling gt-abc gt-def gt-ghi gastown
	// Pattern B (auto-resolve):  gt sling gt-abc gt-def gt-ghi
	// When len(args) > 2 and last arg is a rig, sling each bead to its own polecat.
	// When all args look like bead IDs, auto-resolve the rig from their prefix.
	if len(args) > 2 {
		lastArg := args[len(args)-1]
		if rigName, isRig := r.isRigName(lastArg); isRig {
			beadIDs := args[:len(args)-1]
			if deferred {
				// Reject epic/convoy IDs in batch — they must be dispatched individually
				for _, id := range beadIDs {
					idType, typeErr := r.idType(id)
					if typeErr == nil && idType != "task" {
						return fmt.Errorf("%s '%s' cannot be batch-scheduled with an explicit rig\nUse: gt sling %s (children auto-resolve rigs)", idType, id, id)
					}
				}
				return r.batchSchedule(beadIDs, rigName, townRoot)
			}
			// Explicit rig: print tip about auto-resolve
			fmt.Fprintf(r.out, "  %s the rig can be auto-resolved from bead prefixes. "+
				"You can omit <%s>.\n",
				style.Dim.Render("Tip:"), rigName)
			return r.batchSling(beadIDs, rigName, townBeadsDir)
		}
		// No explicit rig -- try auto-resolving from bead prefixes
		if allBeadIDs(args) {
			rigName, err := r.rigFromBeadIDs(args, filepath.Dir(townBeadsDir))
			if err != nil {
				return err
			}
			return r.batchSling(args, rigName, townBeadsDir)
		}
	}

	// Deferred routing: formula-on-bead with rig target
	// gt sling mol-review --on gt-abc gastown  (when max_polecats > 0)
	if deferred && r.opts.on != "" && len(args) >= 2 {
		rigName, isRig := r.isRigName(args[len(args)-1])
		if isRig {
			formulaName := args[0]
			if r.opts.hookRawBead {
				formulaName = ""
			}
			beadID := r.opts.on
			return r.scheduleBead(beadID, rigName, ScheduleOptions{
				Formula:      formulaName,
				Args:         r.opts.argsText,
				Vars:         r.opts.vars,
				Merge:        r.opts.merge,
				BaseBranch:   r.opts.baseBranch,
				ResumeBranch: r.opts.resumeBranch,
				NoConvoy:     r.opts.noConvoy,
				Owned:        r.opts.owned,
				DryRun:       r.opts.dryRun,
				Force:        r.opts.force,
				NoMerge:      r.opts.noMerge,
				ReviewOnly:   r.opts.reviewOnly,
				Account:      r.opts.account,
				Agent:        r.opts.agent,
				HookRawBead:  r.opts.hookRawBead,
				Ralph:        r.opts.ralph,
			})
		}
	}

	// Deferred routing: formula-on-bead without explicit rig (auto-resolve from bead prefix)
	// gt sling mol-review --on gt-abc  (when max_polecats > 0, no explicit rig arg)
	if deferred && r.opts.on != "" {
		if len(args) >= 2 {
			// Non-rig last arg with --on in deferred mode — give clear error
			return fmt.Errorf("'%s' is not a known rig\nUse: gt sling %s --on %s <rig>", args[len(args)-1], args[0], r.opts.on)
		}
		// Auto-resolve rig from bead prefix
		townRoot, twErr := r.townOrEnv()
		if twErr != nil {
			return twErr
		}
		rigName := r.rigForBead(townRoot, r.opts.on)
		if rigName == "" {
			return fmt.Errorf("cannot resolve rig for bead %s\nSpecify explicitly: gt sling %s --on %s <rig>", r.opts.on, args[0], r.opts.on)
		}
		formulaName := args[0]
		if r.opts.hookRawBead {
			formulaName = ""
		}
		return r.scheduleBead(r.opts.on, rigName, ScheduleOptions{
			Formula:      formulaName,
			Args:         r.opts.argsText,
			Vars:         r.opts.vars,
			Merge:        r.opts.merge,
			BaseBranch:   r.opts.baseBranch,
			ResumeBranch: r.opts.resumeBranch,
			NoConvoy:     r.opts.noConvoy,
			Owned:        r.opts.owned,
			DryRun:       r.opts.dryRun,
			Force:        r.opts.force,
			NoMerge:      r.opts.noMerge,
			ReviewOnly:   r.opts.reviewOnly,
			Account:      r.opts.account,
			Agent:        r.opts.agent,
			HookRawBead:  r.opts.hookRawBead,
			Ralph:        r.opts.ralph,
		})
	}

	// Single bead + rig (2 args): deferred check before resolveTarget side-effects
	if deferred && len(args) == 2 {
		rigName, isRig := r.isRigName(args[1])
		if isRig {
			// Reject epic/convoy IDs — they must be dispatched without a rig
			// (children auto-resolve their rigs)
			idType, err := r.idType(args[0])
			if err == nil && idType != "task" {
				return fmt.Errorf("%s cannot be scheduled with an explicit rig\nUse: gt sling %s (children auto-resolve rigs)",
					idType, args[0])
			}
			if r.verifyBead(args[0]) != nil {
				formulaWorkDir := townRoot
				if rigBeadsDir, ok := r.rigBeadsDir(townRoot, rigName); ok {
					formulaWorkDir = filepath.Dir(rigBeadsDir)
				}
				if r.verifyFormula(args[0], formulaWorkDir, townRoot) == nil {
					// Standalone formula slinging (cook+wisp+attach) is not bead-based
					// dispatch and does not consume a scheduler slot — fall through to
					// runSlingFormula, which handles polecat spawning via resolveTarget.
					return r.slingFormula(ctx, args)
				}
			}
			beadID := args[0]
			formula := r.resolveFormula(r.opts.formula, r.opts.hookRawBead, townRoot, rigName)
			return r.scheduleBead(beadID, rigName, ScheduleOptions{
				Formula:      formula,
				Args:         r.opts.argsText,
				Vars:         r.opts.vars,
				Merge:        r.opts.merge,
				BaseBranch:   r.opts.baseBranch,
				ResumeBranch: r.opts.resumeBranch,
				NoConvoy:     r.opts.noConvoy,
				Owned:        r.opts.owned,
				DryRun:       r.opts.dryRun,
				Force:        r.opts.force,
				NoMerge:      r.opts.noMerge,
				ReviewOnly:   r.opts.reviewOnly,
				Account:      r.opts.account,
				Agent:        r.opts.agent,
				HookRawBead:  r.opts.hookRawBead,
				Ralph:        r.opts.ralph,
			})
		}
		// Dog targets (deacon/dogs, deacon/dogs/<name>, dog:, dog:<name>) fall through
		// to direct dispatch: dogs are a self-managed pool owned by the Deacon, not rig
		// polecat slots, and therefore don't participate in the capacity scheduler.
		// Without this fallthrough, dispatchFeedDog can't feed stranded convoys when a
		// scheduler is active (bead aa-4yf2).
		if _, isDog := IsDogTarget(args[1]); !isDog {
			// Non-rig, non-dog target in deferred mode — reject to prevent bypassing capacity control
			return fmt.Errorf("deferred dispatch requires a rig target: gt sling %s <rig>\n'%s' is not a known rig", args[0], args[1])
		}
		// else: fall through to direct dispatch path below (resolveTarget handles dogs).
	}

	// Epic/convoy auto-detection (1 arg, no rig): works for both deferred and direct
	if len(args) == 1 {
		idType, err := r.idType(args[0])
		if err == nil && idType != "task" {
			formula := r.resolveFormula(r.opts.formula, r.opts.hookRawBead, townRoot, "")

			switch idType {
			case "convoy":
				if err := validateNoTaskOnlySchedulerFlags(cmd, "convoy"); err != nil {
					return err
				}
				if deferred {
					return r.convoySchedule(args[0], convoyScheduleOpts{
						Formula:         formula,
						FormulaExplicit: r.opts.formula != "",
						HookRawBead:     r.opts.hookRawBead,
						Force:           r.opts.force,
						DryRun:          r.opts.dryRun,
					})
				}
				return r.convoySling(args[0], convoyScheduleOpts{
					Formula:         formula,
					FormulaExplicit: r.opts.formula != "",
					HookRawBead:     r.opts.hookRawBead,
					Force:           r.opts.force,
					DryRun:          r.opts.dryRun,
					NoBoot:          r.opts.noBoot,
				})
			case "epic":
				if err := validateNoTaskOnlySchedulerFlags(cmd, "epic"); err != nil {
					return err
				}
				if deferred {
					return r.epicSchedule(args[0], epicScheduleOpts{
						Formula:     formula,
						HookRawBead: r.opts.hookRawBead,
						Force:       r.opts.force,
						DryRun:      r.opts.dryRun,
					})
				}
				return r.epicSling(args[0], epicScheduleOpts{
					Formula:     formula,
					HookRawBead: r.opts.hookRawBead,
					Force:       r.opts.force,
					DryRun:      r.opts.dryRun,
					NoBoot:      r.opts.noBoot,
				})
			}
		}
		// task bead with deferred + no rig: error — must specify a rig
		if deferred {
			return fmt.Errorf("deferred dispatch requires a rig target: gt sling %s <rig>", args[0])
		}
	}

	// 2-bead auto-resolve: gt sling gt-abc gt-def
	if len(args) == 2 && allBeadIDs(args) {
		if _, isRig := r.isRigName(args[1]); !isRig {
			rigName, err := r.rigFromBeadIDs(args, filepath.Dir(townBeadsDir))
			if err != nil {
				return err
			}
			return r.batchSling(args, rigName, townBeadsDir)
		}
	}

	// Determine mode based on flags and argument types
	var beadID string
	var formulaName string
	attachedMoleculeID := ""

	if r.opts.on != "" {
		// Formula-on-bead mode: gt sling <formula> --on <bead>
		formulaName = args[0]
		beadID = r.opts.on
		// Verify both exist
		if err := r.verifyBead(beadID); err != nil {
			return err
		}
		if err := r.verifyFormula(formulaName, r.hookDir(townRoot, beadID, ""), townRoot); err != nil {
			return err
		}
	} else {
		// Could be bead mode or standalone formula mode
		firstArg := args[0]

		// Try as bead first
		if err := r.verifyBead(firstArg); err == nil {
			// It's a verified bead
			beadID = firstArg
		} else {
			// Not a verified bead - try as standalone formula
			if err := r.verifyFormula(firstArg, townRoot, townRoot); err == nil {
				// Standalone formula mode: gt sling <formula> [target]
				// Deferred dispatch is handled above for the 2-arg rig case (gh#3917).
				return r.slingFormula(ctx, args)
			}
			// Not a formula either - check if it looks like a bead ID (routing issue workaround).
			// Accept it and let the actual bd update fail later if the bead doesn't exist.
			// This fixes: gt sling bd-ka761 beads/crew/dave failing with 'not a valid bead or formula'
			if looksLikeBeadID(firstArg) {
				beadID = firstArg
			} else {
				// Neither bead nor formula
				return fmt.Errorf("'%s' is not a valid bead or formula", firstArg)
			}
		}
	}

	// Serialize assignment writes per bead to prevent concurrent sling races from
	// producing conflicting assignee/metadata updates.
	releaseSlingLock, err := r.lockBead(townRoot, beadID)
	if err != nil {
		return err
	}
	defer releaseSlingLock()

	// Check if bead is already assigned (guard against accidental re-sling).
	// This must happen before r.resolveTarget(), since rig targets can spawn/hook a new polecat as a side-effect.
	info, err := r.beadInfo(beadID)
	if err != nil {
		return fmt.Errorf("checking bead status: %w", err)
	}

	// Guard against slinging beads with flag-like titles (gt-e0kx5).
	// These are garbage beads created by flag-parsing bugs. Slinging them
	// causes dispatch loops where polecats bounce the work.
	if beads.IsFlagLikeTitle(info.Title) {
		return fmt.Errorf("refusing to sling bead %s: title %q looks like a CLI flag (garbage bead from flag-parsing bug)", beadID, info.Title)
	}

	// Guard against dispatching closed/tombstone beads (defense-in-depth).
	// Not bypassed by --force — if you need to re-dispatch, reopen the bead first.
	if info.Status == "closed" || info.Status == "tombstone" {
		return fmt.Errorf("bead %s is %s (work already completed)", beadID, info.Status)
	}

	// Guard against slinging deferred beads (gt-1326mw).
	// Deferred work (e.g., "deferred to post-launch") should not consume polecat slots.
	// Use --force to override when intentionally re-activating deferred work.
	if isDeferredBead(info) && !r.opts.force {
		return fmt.Errorf("refusing to sling deferred bead %s: %q\nDeferred work should not consume polecat slots. Use --force to override", beadID, info.Title)
	}

	// Guard against slinging a bead the human operator owns (gt-21pl0): one
	// labeled `operator`, or assigned to a person rather than to an agent
	// address. Dispatching one spends a polecat seat on work no agent can
	// finish, and silently reverses the operator's own assignment — a convoy
	// feeder re-slung gt-nj23.9 to a fresh polecat minutes after the mayor had
	// un-slung it and assigned it to the operator. The marker makes an
	// automatic dispatcher read this as a deferral rather than a failure.
	if reason := dispatch.OperatorReservation(info.Labels, info.Assignee); reason != "" && !r.opts.force {
		return fmt.Errorf("%s %s is the operator's work (%s)\nAn agent does not take it. Use --force to sling it to one anyway",
			dispatch.SlingRefusalMarker, beadID, reason)
	}

	// Guard against re-slinging work submitted for landing (gt-v4ssj.2). Its
	// session ended on purpose in gt done and its assignee is dead by design,
	// which the auto-force below would read as abandoned work. The landing
	// worker owns it until it lands or is handed back; --force does not
	// override, because the automated redispatch paths pass --force. A human
	// who needs it back removes the label first.
	if slices.Contains(info.Labels, land.LabelReadyToLand) {
		return fmt.Errorf("%s %s is submitted for landing (%s); the landing worker owns it.\nRemove the label first to take it back",
			dispatch.SlingRefusalMarker, beadID, land.LabelReadyToLand)
	}

	originalStatus := info.Status
	originalAssignee := info.Assignee
	force := r.opts.force // local copy to avoid mutating package-level flag
	if (info.Status == "pinned" || info.Status == "hooked" || info.Status == "in_progress") && !force {
		// Auto-force when hooked/in_progress agent's session is confirmed dead (gt-pqf9x, GH#1380).
		// This eliminates the #1 friction in convoy feeding: stale hooks from
		// dead polecats blocking re-sling without --force.
		// IMPORTANT: Stale-hook check must run BEFORE idempotency check so that
		// a dead polecat with a matching target triggers re-sling, not a no-op.
		if (info.Status == "hooked" || info.Status == "in_progress") && info.Assignee != "" && r.agentDead(info.Assignee) {
			// The holder is gone — but that alone does not mean the work is.
			// A polecat killed mid-work (town halt, operator park, crashed
			// session) never runs `gt done`, so its branch stays on origin
			// while the bead still reads as re-slingable. Auto-forcing here
			// would spawn a second polecat from main on work that already
			// exists — the spawn storm behind gt-ibt8 (4 polecats) and
			// gt-da2x (3). Preserved work must be resumed or explicitly
			// discarded, never silently re-created.
			if r.opts.resumeBranch == "" {
				if err := r.survivingWorkGuard(townRoot, beadID, info.Assignee); err != nil {
					return err
				}
			}
			fmt.Fprintf(r.out, "%s Hooked agent %s has no active session, auto-forcing re-sling...\n",
				style.Warning.Render("⚠"), info.Assignee)
			force = true
		} else {
			// Agent is alive (or bead is pinned) — check idempotency before erroring.
			target := ""
			if len(args) > 1 {
				// Batch mode (len(args) > 2) exits earlier at line 231, so
				// args[len(args)-1] is always the target here.
				target = args[len(args)-1]
			}
			// Only resolve self-agent when needed (empty/dot target = self-sling).
			// For explicit targets, idempotency works regardless of cwd/env.
			selfAgent := ""
			skipIdempotency := false
			if target == "" || target == "." {
				sa, _, _, err := r.resolveSelf()
				if err != nil {
					// Can't determine self — skip idempotency for self-target,
					// fall through to the existing error path.
					skipIdempotency = true
				} else {
					selfAgent = sa
				}
			}
			if !skipIdempotency && matchesSlingTarget(target, info.Assignee, selfAgent) {
				if formulaName == "" {
					// Plain sling to same target: no-op.
					fmt.Fprintf(r.out, "%s Bead %s is already %s to %s, no-op\n",
						style.Dim.Render("○"), beadID, info.Status, info.Assignee)
					return nil
				}
				// Formula-on-bead with matching target: fall through so
				// formula instantiation (cook/wisp/bond) runs. The bead
				// stays hooked/pinned to the same agent — only the formula
				// work is new. We don't set force=true to avoid triggering
				// the unhook/reassign path at the force-handler below.
			} else {
				assignee := info.Assignee
				if assignee == "" {
					assignee = "(unknown)"
				}
				return fmt.Errorf("bead %s is already %s to %s\nUse --force to re-sling", beadID, info.Status, assignee)
			}
		}
	}

	// Content duplicate check (gt-mcq): refuse a bead whose named tests and
	// files already appear on open or recently-closed work in this rig. Placed
	// after the already-hooked guard so an idempotent re-sling of the same bead
	// is not reported as a duplicate of itself, and before resolveTarget so a
	// refusal costs no side effects. --force is the documented override, and a
	// dry run reports the overlap without refusing.
	var dupCandidate *duplicateCandidate
	if !force {
		var matches []duplicateMatch
		var checkErr error
		dupCandidate, matches, checkErr = r.checkDuplicates(townRoot, beadID, info)
		if checkErr != nil {
			fmt.Fprintf(r.out, "%s %v\n", style.Dim.Render("Warning:"), checkErr)
		}
		decision := decideSlingDuplicates(beadID, matches)
		switch {
		case decision.Blocked && !r.opts.dryRun:
			return errors.New(decision.Message)
		case decision.Blocked:
			fmt.Fprintf(r.out, "%s Dry run: this sling would be refused.\n", style.Dim.Render("○"))
			_, _ = fmt.Fprint(r.out, decision.Message)
		case decision.Message != "":
			_, _ = fmt.Fprint(r.out, decision.Message)
		}
	}

	// TODO(scheduler-unify): Migrate single-sling rig dispatch to use executeSling().
	// The inline logic below duplicates executeSling's 12-step flow. Batch sling
	// and scheduler dispatch already use the unified path. Single-sling is deferred
	// because it handles non-rig targets (dogs, mayor, crew, self-sling, nudge)
	// that executeSling does not cover. The rig-target case could be factored out
	// to use executeSling, limiting this to non-rig targets only.
	//
	// Resolve target agent using shared dispatch logic.
	// Note: args[1] == args[len(args)-1] here because batch mode (len(args) > 2
	// with rig last arg) exits at line 234. The only remaining case is len(args) <= 2.
	var target string
	if len(args) > 1 {
		target = args[1]
	}
	resolved, err := r.resolveTarget(target, ResolveTargetOptions{
		DryRun:       r.opts.dryRun,
		Force:        force,
		Create:       r.opts.create,
		Account:      r.opts.account,
		Agent:        r.opts.agent,
		NoBoot:       r.opts.noBoot,
		HookBead:     beadID,
		BeadID:       beadID,
		TownRoot:     townRoot,
		BaseBranch:   r.opts.baseBranch,
		ResumeBranch: r.opts.resumeBranch,
	})
	if err != nil {
		return err
	}
	targetAgent := resolved.Agent
	targetPane := resolved.Pane
	hookWorkDir := resolved.WorkDir
	hookSetAtomically := resolved.HookSetAtomically
	delayedDogInfo := resolved.DelayedDogInfo
	newPolecatInfo := resolved.NewPolecatInfo
	isSelfSling := resolved.IsSelfSling
	if newPolecatInfo != nil {
		newPolecatInfo.originalHold = &beadHold{Status: originalStatus, Assignee: originalAssignee}
	}

	// Rollback guard (gt-7evi4). resolveTarget may have spawned or reused a
	// polecat; from here every exit that does not reach the commit point below
	// rolls it back exactly once. Early returns set rollbackReason at most.
	//
	// rollbackBeadID stays "" until this sling first writes to the bead, so a
	// failure before that never burns molecules or releases a hook this sling
	// did not create. With no polecat spawned, the guard has nothing to own once
	// the hook has landed (a delayed dog keeps its own failure handling).
	//
	// The auto-convoy stays open on a rollback so the convoy feeder can
	// re-dispatch the bead with the recorded agent (gt-yg24) — except when raw
	// metadata could not be stored, where the sling never got as far as a
	// dispatchable bead and the convoy is closed with the spawn.
	slingCommitted := false
	hooked := false
	rollbackBeadID := ""
	rollbackReason := ""
	var convoyID string
	rollbackSpawnedPolecat := func(reason string) {
		if newPolecatInfo != nil {
			rollbackConvoyID := ""
			if reason == rawSlingMetadataRollbackReason {
				rollbackConvoyID = convoyID
			}
			fmt.Fprintf(r.out, "%s %s, rolling back spawned polecat %s...\n", style.Warning.Render("⚠"), reason, newPolecatInfo.PolecatName)
			r.rollbackArtifacts(newPolecatInfo, rollbackBeadID, hookWorkDir, rollbackConvoyID)
		}
		if rollbackBeadID == "" {
			return // this sling has not written to the bead: nothing to restore
		}
		r.restoreRawFields(beadID, townRoot, hookWorkDir, info)
		// Under --force, rollback's unhook can clear a pinned bead's original state.
		if force && originalStatus == "pinned" {
			r.restorePinned(townRoot, beadID, originalAssignee)
		}
	}
	defer func() {
		if slingCommitted || r.opts.dryRun || (hooked && newPolecatInfo == nil) {
			return
		}
		reason := rollbackReason
		if reason == "" {
			reason = "Sling did not complete"
			if retErr != nil {
				reason = fmt.Sprintf("Sling failed (%v)", retErr)
			}
		}
		rollbackSpawnedPolecat(reason)
	}()

	var admission *polecatAdmissionHandle
	if !r.opts.dryRun && !hookSetAtomically && strings.Contains(targetAgent, "/polecats/") {
		parts := strings.Split(targetAgent, "/")
		if len(parts) >= 3 {
			var snapshot polecatCapacitySnapshot
			admission, snapshot, err = r.admitPolecat(townRoot, parts[0], beadID, "direct-target")
			if err != nil {
				return err
			}
			defer admission.Release()
			if snapshot.Max > 0 {
				fmt.Fprintf(r.out, "%s Polecat capacity reserved (%d free of %d)\n", style.Dim.Render("○"), snapshot.Free, snapshot.Max)
			}
		}
	}
	// Inject base_branch var for formula instantiation (non-main only; formula default handles main)
	if newPolecatInfo != nil && newPolecatInfo.BaseBranch != "" && newPolecatInfo.BaseBranch != "main" {
		r.opts.vars = append(r.opts.vars, fmt.Sprintf("base_branch=%s", newPolecatInfo.BaseBranch))
	}
	// Inject resume_branch var when the polecat was attached to an existing branch
	// (gh#3602: gt sling --branch / --pr). Lets formulas tell the polecat it is
	// resuming an existing PR instead of creating a fresh branch.
	if r.opts.resumeBranch != "" {
		r.opts.vars = append(r.opts.vars, fmt.Sprintf("resume_branch=%s", r.opts.resumeBranch))
	}

	// Cross-rig guard: prevent slinging beads to polecats in the wrong rig (gt-myecw).
	// Polecats work in their rig's worktree and cannot fix code owned by another rig.
	// Skip for self-sling (user knows what they're doing) and --force overrides.
	if strings.Contains(targetAgent, "/polecats/") && !force && !isSelfSling {
		if err := r.crossRigGuard(beadID, targetAgent, townRoot); err != nil {
			rollbackReason = "Cross-rig guard failed"
			return err
		}
	}

	// Display what we're doing
	if formulaName != "" {
		fmt.Fprintf(r.out, "%s Slinging formula %s on %s to %s...\n", style.Bold.Render("🎯"), formulaName, beadID, targetAgent)
	} else {
		fmt.Fprintf(r.out, "%s Slinging %s to %s...\n", style.Bold.Render("🎯"), beadID, targetAgent)
	}

	// Handle --force when bead is already hooked/in_progress: send shutdown to old polecat and unhook (GH#1380)
	if (info.Status == "hooked" || info.Status == "in_progress") && force && info.Assignee != "" {
		fmt.Fprintf(r.out, "%s Bead already hooked to %s, forcing reassignment...\n", style.Warning.Render("⚠"), info.Assignee)
		if r.opts.dryRun {
			fmt.Fprintf(r.out, "Would send LIFECYCLE:Shutdown to previous assignee %s\n", info.Assignee)
			fmt.Fprintf(r.out, "Would unhook %s from previous assignee\n", beadID)
		} else {

			requester := r.requester()

			// Extract rig name from assignee (e.g., "gastown/polecats/Toast" -> "gastown")
			assigneeParts := strings.Split(info.Assignee, "/")
			if len(assigneeParts) >= 3 && assigneeParts[1] == "polecats" {
				oldRigName := assigneeParts[0]
				oldPolecatName := assigneeParts[2]

				// Send LIFECYCLE:Shutdown to witness - will auto-nuke if clean,
				// otherwise create cleanup wisp for manual intervention
				if townRoot != "" {
					shutdownMsg := &mail.Message{
						From:     "gt-sling",
						To:       fmt.Sprintf("%s/witness", oldRigName),
						Subject:  fmt.Sprintf("LIFECYCLE:Shutdown %s", oldPolecatName),
						Body:     fmt.Sprintf("Reason: work_reassigned\nRequestedBy: %s\nBead: %s\nNewAssignee: %s", requester, beadID, targetAgent),
						Type:     mail.TypeTask,
						Priority: mail.PriorityHigh,
					}
					if err := r.notifyWitness(townRoot, shutdownMsg); err != nil {
						fmt.Fprintf(r.out, "%s Could not send shutdown to witness: %v\n", style.Dim.Render("Warning:"), err)
					} else {
						fmt.Fprintf(r.out, "%s Sent LIFECYCLE:Shutdown to %s/witness for %s\n", style.Bold.Render("→"), oldRigName, oldPolecatName)
					}
				}

				// gt-skwt: clear the outgoing polecat's agent-bead state now,
				// synchronously — don't rely on the shutdown mail alone (see
				// clearReassignedPolecatState).
				r.clearReassigned(townRoot, info.Assignee)
			}

			// Unhook the bead from old owner (set status back to open)
			if err := r.unhook(townRoot, beadID); err != nil {
				fmt.Fprintf(r.out, "%s Could not unhook bead from old owner: %v\n", style.Dim.Render("Warning:"), err)
			}
		}
	}

	// Auto-convoy: check if issue is already tracked by a convoy
	// If not, create one so the work is tracked (unless --no-convoy is set)
	if !r.opts.noConvoy && formulaName == "" {
		if r.opts.dryRun {
			fmt.Fprintf(r.out, "Would create convoy 'Work: %s' if needed\n", info.Title)
			fmt.Fprintf(r.out, "Would add tracking relation to %s if needed\n", beadID)
			if r.opts.merge != "" {
				fmt.Fprintf(r.out, "Would set convoy merge strategy: %s\n", r.opts.merge)
			}
		} else {
			existingConvoy := r.trackedByConvoy(beadID)
			if existingConvoy == "" {
				var err error
				// Record the requested runtime agent and formula on the convoy:
				// if this sling fails after the convoy exists, the convoy
				// feeder re-dispatches the bead and must re-use this agent and
				// formula rather than the rig default (gt-yg24, gt-4lor).
				convoyID, err = r.createConvoy(beadID, info.Title, r.opts.owned, r.opts.merge, r.opts.baseBranch, r.opts.agent, r.opts.formula)
				if err != nil {
					// Log warning but don't fail - convoy is optional
					fmt.Fprintf(r.out, "%s Could not create auto-convoy: %v\n", style.Dim.Render("Warning:"), err)
				} else {
					fmt.Fprintf(r.out, "%s Created convoy 🚚 %s\n", style.Bold.Render("→"), convoyID)
					r.steps.step("convoy")
					fmt.Fprintf(r.out, "  Tracking: %s\n", beadID)
					if r.opts.owned {
						fmt.Fprintf(r.out, "  Lifecycle: caller-managed (owned)\n")
					}
					if r.opts.merge != "" {
						fmt.Fprintf(r.out, "  Merge:    %s\n", r.opts.merge)
					}
				}
			} else {
				fmt.Fprintf(r.out, "%s Already tracked by convoy %s\n", style.Dim.Render("○"), existingConvoy)
			}
		}
	}

	// Issue #288: Auto-apply mol-polecat-work when slinging bare bead to polecat.
	// This ensures polecats get structured work guidance through formula-on-bead.
	// Use --hook-raw-bead to bypass for expert/debugging scenarios.
	if formulaName == "" && !r.opts.hookRawBead && strings.Contains(targetAgent, "/polecats/") {
		targetRig := ""
		if parts := strings.SplitN(targetAgent, "/", 2); len(parts) >= 1 {
			targetRig = parts[0]
		}
		formulaName = r.resolveFormula(r.opts.formula, false, townRoot, targetRig)
		if r.opts.formula != "" {
			fmt.Fprintf(r.out, "  Applying %s for polecat work...\n", formulaName)
		} else {
			fmt.Fprintf(r.out, "  Auto-applying %s for polecat work...\n", formulaName)
		}
	}

	// Guard: ensure only one molecule is attached to a work bead.
	// Checks both dependency bonds (ground truth) and description metadata.
	// When re-slinging with --force, burn ALL existing molecules before creating a new one.
	// Without this, each sling creates a new wisp bonded to the bead, leaving orphaned molecules.
	// NOTE: Uses local `force` (not `r.opts.force`) to respect auto-force paths (dead agent detection).
	if formulaName != "" {
		existingMolecules, err := r.collectMolecules(info, beadID, townRoot)
		if err != nil {
			return fmt.Errorf("checking existing molecule bonds: %w", err)
		}
		if len(existingMolecules) > 0 {
			stale := force || r.orphanMolecule(info)
			if r.opts.dryRun && stale {
				fmt.Fprintf(r.out, "  Would burn %d stale molecule(s): %s\n",
					len(existingMolecules), strings.Join(existingMolecules, ", "))
			} else if stale {
				fmt.Fprintf(r.out, "  %s Burning %d stale molecule(s) from previous assignment: %s\n",
					style.Warning.Render("⚠"), len(existingMolecules), strings.Join(existingMolecules, ", "))
				if err := r.burnMolecules(existingMolecules, beadID, townRoot); err != nil {
					return fmt.Errorf("burning stale molecules: %w", err)
				}
			} else {
				return fmt.Errorf("bead %s already has %d attached molecule(s): %s\nUse --force to replace, or --hook-raw-bead to skip formula",
					beadID, len(existingMolecules), strings.Join(existingMolecules, ", "))
			}
		}
	}

	if r.opts.dryRun {
		if formulaName != "" {
			fmt.Fprintf(r.out, "Would instantiate formula %s:\n", formulaName)
			fmt.Fprintf(r.out, "  1. bd cook %s\n", formulaName)
			fmt.Fprintf(r.out, "  2. bd mol bond %s %s --json --ephemeral --var feature=\"%s\" --var issue=\"%s\"\n", formulaName, beadID, info.Title, beadID)
			fmt.Fprintf(r.out, "  3. bd update %s --status=hooked --assignee=%s\n", beadID, targetAgent)
		} else {
			fmt.Fprintf(r.out, "Would run: bd update %s --status=hooked --assignee=%s\n", beadID, targetAgent)
		}
		if originalAssignee != "" && originalAssignee != targetAgent {
			fmt.Fprintf(r.out, "Would record reassignment on %s: %s -> %s\n", beadID, originalAssignee, targetAgent)
		}
		if r.opts.subject != "" {
			fmt.Fprintf(r.out, "  subject (in nudge): %s\n", r.opts.subject)
		}
		if r.opts.message != "" {
			fmt.Fprintf(r.out, "  context: %s\n", r.opts.message)
		}
		if r.opts.argsText != "" {
			fmt.Fprintf(r.out, "  args (in nudge): %s\n", r.opts.argsText)
		}
		fmt.Fprintf(r.out, "Would inject start prompt to pane: %s\n", targetPane)
		return nil
	}

	// From here on this sling writes to the bead (formula bond, raw metadata,
	// hook), so a rollback must clean the bead as well as the polecat.
	rollbackBeadID = beadID

	// Formula-on-bead mode: instantiate formula and bond to original bead
	formulaVarsForAttachment := strings.Join(r.opts.vars, "\n")
	varsForAttachment := append([]string(nil), r.opts.vars...)
	if formulaName != "" {
		fmt.Fprintf(r.out, "  Instantiating formula %s...\n", formulaName)

		// Auto-inject rig command vars as defaults (user --var flags override)
		if parts := strings.SplitN(targetAgent, "/", 2); len(parts) >= 1 && parts[0] != "" {
			rigCmdVars := r.rigCommandVars(townRoot, parts[0])
			r.opts.vars = append(rigCmdVars, r.opts.vars...)
			varsForAttachment = append([]string(nil), r.opts.vars...)
			formulaVarsForAttachment = strings.Join(r.opts.vars, "\n")
		}

		result, err := r.instantiateFormula(ctx, formulaName, beadID, info.Title, hookWorkDir, townRoot, false, r.opts.vars)
		if err != nil {
			// The guard rolls back the partial artifacts: a wisp creation
			// failure (e.g., missing required vars) must not orphan a polecat.
			rollbackReason = "Formula instantiation failed"
			return fmt.Errorf("instantiating formula %s: %w", formulaName, err)
		}

		fmt.Fprintf(r.out, "%s Formula wisp created: %s\n", style.Bold.Render("✓"), result.WispRootID)
		fmt.Fprintf(r.out, "%s Formula bonded to %s\n", style.Bold.Render("✓"), beadID)

		// Record attached molecule - will be stored in BASE bead (not wisp).
		// The base bead is hooked, and its attached_molecule points to the wisp.
		// This enables:
		// - gt hook/gt prime: read base bead, follow attached_molecule to show wisp steps
		// - gt done: close attached_molecule (wisp) first, then close base bead
		// - Compound resolution: base bead -> attached_molecule -> wisp
		attachedMoleculeID = result.WispRootID
		r.steps.step("formula")
		if len(result.FormulaVars) > 0 {
			varsForAttachment = append([]string(nil), result.FormulaVars...)
			formulaVarsForAttachment = strings.Join(result.FormulaVars, "\n")
		}

		// NOTE: We intentionally keep beadID as the ORIGINAL base bead, not the wisp.
		// The base bead is hooked so that:
		// 1. gt done closes both the base bead AND the attached molecule (wisp)
		// 2. The base bead's attached_molecule field points to the wisp for compound resolution
		// Previously, this line incorrectly set beadID = wispRootID, causing:
		// - Wisp hooked instead of base bead
		// - attached_molecule stored as self-reference in wisp (meaningless)
		// - Base bead left orphaned after gt done
	}

	actor := r.actor()
	mode := ""
	if r.opts.ralph {
		mode = "ralph"
	}
	fieldUpdates := buildSlingFieldUpdates(
		actor,
		r.opts.argsText,
		varsForAttachment,
		attachedMoleculeID,
		formulaName,
		r.opts.noMerge,
		r.opts.reviewOnly,
		mode,
		formulaVarsForAttachment,
		convoyID,
		r.opts.merge,
		r.opts.owned,
	)

	// Hook the bead with retry and verification.
	// See: https://github.com/steveyegge/gastown/issues/148
	//
	// Acquire a per-assignee lock before writing hook_bead to serialize concurrent slings
	// targeting the same polecat. Without this, multiple concurrent slings race on the
	// same assignee's row in Dolt, causing silent rollbacks (issue #3114).
	assigneeUnlock, assigneeLockErr := r.lockAssignee(townRoot, targetAgent)
	if assigneeLockErr != nil {
		return fmt.Errorf("serializing hook write for %s: %w", targetAgent, assigneeLockErr)
	}
	defer assigneeUnlock()
	if attachedMoleculeID == "" && (r.opts.noMerge || r.opts.reviewOnly) {
		if err := r.storeFields(townRoot, beadID, fieldUpdates); err != nil {
			rollbackReason = rawSlingMetadataRollbackReason
			return fmt.Errorf("storing raw sling metadata before hook: %w", err)
		}
	}
	hookDir := r.hookDir(townRoot, beadID, hookWorkDir)
	// The hook write below replaces the assignee. Record the outgoing value
	// first: assignee keeps only the last writer, so afterwards the previous
	// polecat's branch is unreachable from the bead (gt-zd7c).
	r.recordReassignment(townRoot, beadID, originalAssignee, targetAgent, r.requester())
	if err := r.hook(beadID, targetAgent, hookDir, ""); err != nil {
		rollbackReason = "Hook failed"
		return err
	}
	hooked = true
	r.steps.step("hook")
	r.clearOrphanLabels(townRoot, beadID, hookWorkDir)

	// The bead is dispatched now, so later dispatches in this process should
	// see it in the pool even though their snapshot predates this hook.
	r.noteDispatched(townRoot, dupCandidate)

	// Emit a propulsion signal if the target is the mayor.
	// This allows the ACP propeller to react to hook changes event-driven.
	if targetAgent == "mayor/" {
		if townRoot != "" {
			session := "hq-mayor"
			message := fmt.Sprintf("Hook updated: attached bead %s", beadID)
			_ = r.enqueueNudge(townRoot, session, nudge.QueuedNudge{
				Sender:   "sling",
				Message:  message,
				Priority: nudge.PriorityNormal,
			})
		}
	}

	fmt.Fprintf(r.out, "%s Work attached to hook (status=hooked)\n", style.Bold.Render("✓"))

	// Log sling event to activity feed
	_ = r.logFeed(events.TypeSling, actor, events.SlingPayload(beadID, targetAgent))

	// Update agent bead's hook_bead field (ZFC: agents track their current work)
	// Skip if hook was already set atomically during polecat spawn - avoids "agent bead not found"
	// error when polecat redirect setup fails (GH #gt-mzyk5: agent bead created in rig beads
	// but updateAgentHookBead looks in polecat's local beads if redirect is missing).
	if !hookSetAtomically {
		r.updateAgentHook(targetAgent, beadID, hookWorkDir, townBeadsDir)
	}

	// Store all attachment fields in a single read-modify-write cycle.
	// This eliminates the race condition where sequential independent updates
	// (dispatcher, args, no_merge, attached_molecule) could overwrite each other.
	if err := r.storeFields(townRoot, beadID, fieldUpdates); err != nil {
		// Warn but don't fail - polecat will still complete work
		fmt.Fprintf(r.out, "%s Could not store fields in bead: %v\n", style.Dim.Render("Warning:"), err)
	} else {
		if r.opts.argsText != "" {
			fmt.Fprintf(r.out, "%s Args stored in bead (durable)\n", style.Bold.Render("✓"))
		}
		if r.opts.noMerge {
			fmt.Fprintf(r.out, "%s No-merge mode enabled (work stays on feature branch)\n", style.Bold.Render("✓"))
		}
		if r.opts.reviewOnly {
			fmt.Fprintf(r.out, "%s Review-only mode: assignee must evaluate and report back, NOT merge/commit/push\n", style.Bold.Render("⚠"))
		}
	}
	if mode != "" {
		r.updateAgentMode(targetAgent, mode, hookWorkDir, townBeadsDir)
	}

	// Start delayed dog session now that hook is set
	// This ensures dog sees the hook when gt prime runs on session start
	if delayedDogInfo != nil {
		pane, err := r.startDelayedDog(delayedDogInfo)
		if err != nil {
			return fmt.Errorf("starting delayed dog session: %w", err)
		}
		targetPane = pane
	}

	// Start polecat session now that attached_molecule is set.
	// This ensures polecat sees the molecule when gt prime runs on session start.
	freshlySpawned := newPolecatInfo != nil
	if freshlySpawned {
		pane, err := r.startSession(newPolecatInfo)
		if err != nil {
			// The guard rolls back the zombie artifacts (worktree, hooked bead).
			// Without rollback, next sling attempt fails with "bead already hooked" (gt-jn40ft).
			rollbackReason = "Session failed"
			return fmt.Errorf("starting polecat session: %w", err)
		}
		targetPane = pane
		r.steps.step("session")
	}

	// Commit point (gt-7evi4): the work is hooked and any polecat this sling
	// spawned is running it. Nothing after this is rolled back.
	slingCommitted = true

	// Try to inject the "start now" prompt (graceful if no tmux)
	// Skip for freshly spawned polecats - SessionManager.Start() already sent StartupNudge.
	// Skip for self-sling - agent is currently processing the sling command and will see
	// the hooked work on next turn. Nudging would inject text while agent is busy.
	if freshlySpawned {
		// Fresh polecat already got StartupNudge from SessionManager.Start()
	} else if isSelfSling {
		// Self-sling: agent already knows about the work (just slung it)
		fmt.Fprintf(r.out, "%s Self-sling: work hooked, will process on next turn\n", style.Dim.Render("○"))
	} else if targetPane == "" {
		fmt.Fprintf(r.out, "%s No pane to nudge (agent will discover work via gt prime)\n", style.Dim.Render("○"))
	} else {
		// Ensure agent is ready before nudging (prevents race condition where
		// message arrives before Claude has fully started - see issue #115)
		sessionName := r.sessionFromPane(targetPane)
		if sessionName != "" {
			if err := r.ensureAgentReady(sessionName); err != nil {
				// Non-fatal: warn and continue, agent will discover work via gt prime
				fmt.Fprintf(r.out, "%s Could not verify agent ready: %v\n", style.Dim.Render("○"), err)
			}
		}

		if err := r.injectStartPrompt(targetPane, beadID, r.opts.subject, r.opts.argsText); err != nil {
			// Graceful fallback for no-tmux mode
			fmt.Fprintf(r.out, "%s Could not nudge (no tmux?): %v\n", style.Dim.Render("○"), err)
			fmt.Fprintf(r.out, "  Agent will discover work via gt prime / bd show\n")
		} else {
			fmt.Fprintf(r.out, "%s Start prompt sent\n", style.Bold.Render("▶"))
		}
	}

	return nil
}

// checkCrossRigGuard validates that a bead's prefix matches the target rig.
// Polecats work in their rig's worktree and cannot fix code owned by another rig.
// Returns an error if the bead belongs to a different rig than the target polecat.
//
// When the prefix maps to town root, the guard warns rather than errors: this
// ambiguous case arises when a crew member's redirect chain is broken and their
// rig's .beads dir shares the town-level database and prefix (gt-gbu). Blocking
// here would silently swallow all polecat work for the affected rig.
//
// Truly unknown prefixes (not in routes.jsonl at all) are still hard-rejected.
func checkCrossRigGuard(beadID, targetAgent, townRoot string) error {
	beadPrefix := beads.ExtractPrefix(beadID)
	if beadPrefix == "" {
		return nil // Can't determine prefix, skip check
	}

	// Extract target rig from agent path (e.g., "gastown/polecats/Toast" → "gastown")
	targetRig := strings.SplitN(targetAgent, "/", 2)[0]
	if targetRig == "" {
		return nil
	}

	beadRig := beads.GetRigNameForPrefix(townRoot, beadPrefix)

	if beadRig != targetRig {
		if beadRig == "" {
			// GetRigNameForPrefix returns "" for two distinct cases:
			//   (a) prefix is in routes.jsonl with path="." (known town-root prefix)
			//   (b) prefix is not in routes.jsonl at all (unknown prefix)
			// GetRigPathForPrefix distinguishes them: it returns townRoot for (a),
			// empty string for (b).
			if beads.GetRigPathForPrefix(townRoot, beadPrefix) == "" {
				// Unknown prefix — no route exists, can't resolve rig.
				return fmt.Errorf("bead %s (prefix %q) is not in rig %q — prefix not in routes\n"+
					"Create the task from the rig directory: cd %s && bd create --title=...\n"+
					"Use --force to override", beadID, strings.TrimSuffix(beadPrefix, "-"), targetRig, targetRig)
			}
			// Known town-root prefix — warn but allow. A crew member may have a
			// broken redirect chain causing rig beads to land in the town DB with
			// the town prefix. Blocking here silently drops all their polecat work
			// (gt-gbu). The polecat will surface any true mismatch on execution.
			fmt.Printf("  %s Bead %s has prefix %q (town root) but target is rig %q — "+
				"proceeding (broken redirect chain? see gt-gbu)\n",
				style.Warning.Render("⚠"), beadID, strings.TrimSuffix(beadPrefix, "-"), targetRig)
			return nil
		}
		return fmt.Errorf("cross-rig mismatch: bead %s (prefix %q) belongs to rig %q, but target is rig %q\n"+
			"Create the task from the target rig: cd %s && bd create --title=...\n"+
			"Use --force to override", beadID, strings.TrimSuffix(beadPrefix, "-"), beadRig, targetRig, targetRig)
	}

	return nil
}

// rawSlingMetadataRollbackReason marks the one rollback that also closes the
// auto-convoy (see the runSling rollback guard).
const rawSlingMetadataRollbackReason = "Raw sling metadata failed"

// rollbackSlingArtifactsFn is a seam for tests. Production uses rollbackSlingArtifacts.
var rollbackSlingArtifactsFn = rollbackSlingArtifacts

// Rollback seams allow tests to assert molecule-cleanup behavior without
// depending on full beads storage side effects.
var getBeadInfoForRollback = getBeadInfo
var collectExistingMoleculesForRollback = collectExistingMolecules
var burnExistingMoleculesForRollback = burnExistingMolecules

func rawWorkflowFieldValues(info *beadInfo) (noMerge, reviewOnly bool, attachedAt string) {
	if info == nil {
		return false, false, ""
	}
	issue := &beads.Issue{Description: info.Description}
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		return false, false, ""
	}
	return fields.NoMerge, fields.ReviewOnly, fields.AttachedAt
}

func restoreRollbackRawWorkflowFields(beadID, townRoot, hookWorkDir string, info, originalInfo *beadInfo) (bool, error) {
	return restoreRollbackRawWorkflowFieldsVia(nil, beadID, townRoot, hookWorkDir, info, originalInfo)
}

// restoreRollbackRawWorkflowFieldsVia is restoreRollbackRawWorkflowFields
// with bd answered by run (nil: bd on PATH).
func restoreRollbackRawWorkflowFieldsVia(run beads.BDRunner, beadID, townRoot, hookWorkDir string, info, originalInfo *beadInfo) (bool, error) {
	if info == nil {
		return false, nil
	}
	originalNoMerge, originalReviewOnly, originalAttachedAt := rawWorkflowFieldValues(originalInfo)
	issue := &beads.Issue{Description: info.Description}
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		if !originalNoMerge && !originalReviewOnly {
			return false, nil
		}
		fields = &beads.AttachmentFields{}
	}
	if fields.NoMerge == originalNoMerge && fields.ReviewOnly == originalReviewOnly && fields.AttachedAt == originalAttachedAt {
		return false, nil
	}
	fields.NoMerge = originalNoMerge
	fields.ReviewOnly = originalReviewOnly
	fields.AttachedAt = originalAttachedAt
	newDesc := beads.SetAttachmentFields(issue, fields)
	if newDesc == info.Description {
		return false, nil
	}
	updateDir := beads.ResolveHookDir(townRoot, beadID, hookWorkDir)
	if err := BdCmd("update", beadID, "--description="+newDesc).
		Dir(updateDir).
		StripBeadsDir().
		WithAutoCommit().
		Via(run).
		Run(); err != nil {
		return false, err
	}
	return true, nil
}

func clearRollbackRawWorkflowFields(beadID, townRoot, hookWorkDir string, info *beadInfo) (bool, error) {
	return restoreRollbackRawWorkflowFields(beadID, townRoot, hookWorkDir, info, nil)
}

func restoreRollbackRawWorkflowFieldsFromCurrent(beadID, townRoot, hookWorkDir string, originalInfo *beadInfo) {
	if beadID == "" || townRoot == "" {
		return
	}
	info, err := getBeadInfoForRollback(beadID)
	if err != nil {
		fmt.Printf("  %s Could not inspect bead %s for raw workflow metadata rollback: %v\n", style.Dim.Render("Warning:"), beadID, err)
		return
	}
	if restored, restoreErr := restoreRollbackRawWorkflowFields(beadID, townRoot, hookWorkDir, info, originalInfo); restoreErr != nil {
		fmt.Printf("  %s Could not restore raw workflow metadata on %s: %v\n", style.Dim.Render("Warning:"), beadID, restoreErr)
	} else if restored {
		fmt.Printf("  %s Restored raw workflow metadata on %s\n", style.Dim.Render("○"), beadID)
	}
}

func restorePinnedBead(townRoot, beadID, assignee string) {
	if townRoot == "" || beadID == "" {
		return
	}
	dir := beads.ResolveHookDir(townRoot, beadID, "")
	if err := BdCmd("update", beadID, "--status=pinned", "--assignee="+assignee).
		Dir(dir).
		WithAutoCommit().
		Run(); err != nil {
		fmt.Printf("  %s Could not restore pinned state for bead %s: %v\n", style.Dim.Render("Warning:"), beadID, err)
	} else {
		fmt.Printf("  %s Restored pinned state for bead %s\n", style.Dim.Render("○"), beadID)
	}
}

func tryAcquireSlingBeadLock(townRoot, beadID string) (func(), error) {
	lockDir := filepath.Join(townRoot, ".runtime", "locks", "sling")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating sling lock dir: %w", err)
	}

	sweepStaleSlingFlocks(lockDir)

	safeBeadID := strings.NewReplacer("/", "_", ":", "_").Replace(beadID)
	lockPath := filepath.Join(lockDir, safeBeadID+".flock")
	unlock, err := lock.FlockTryAcquireStable(lockPath)
	if errors.Is(err, lock.ErrFlockHeld) {
		return nil, fmt.Errorf("bead %s is already being slung; retry after the current assignment completes", beadID)
	}
	if err != nil {
		return nil, fmt.Errorf("acquiring sling lock for bead %s: %w", beadID, err)
	}

	return unlinkThenUnlock(lockPath, unlock), nil
}

// unlinkThenUnlock wraps a sling lock's unlock so the sentinel file goes with
// the lock. The flock is the lock and the kernel drops it on exit, but nothing
// removed the zero-byte file, so one accumulated per bead and per assignee ever
// slung (gt-10u8). Unlinking while still holding the flock is safe only because
// every acquirer of these paths uses lock.FlockTryAcquireStable: a waiter whose
// open landed before this unlink locks the nameless inode, sees it has no name,
// and reopens rather than share the lock with a fresh file at the same path
// (gt-xtfnq).
func unlinkThenUnlock(path string, unlock func()) func() {
	return func() {
		_ = os.Remove(path)
		unlock()
	}
}

// sweepStaleSlingFlocks removes the sentinels a sling killed before its release
// ran left behind, so a crash does not leak the file the release path cleans
// up. It removes only files it locks itself, with lock.FlockTryAcquireStable,
// so a live holder is skipped and a concurrent acquirer that already opened the
// file reopens instead of locking the inode this removes. Best-effort: an entry
// that cannot be probed is left for the next sling and never blocks this one.
func sweepStaleSlingFlocks(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".flock") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		unlock, err := lock.FlockTryAcquireStable(path)
		if err != nil {
			continue
		}
		unlinkThenUnlock(path, unlock)()
	}
}

// tryAcquireSlingAssigneeLock acquires a per-assignee file lock to serialize concurrent
// hook writes to the same polecat. The per-bead lock (tryAcquireSlingBeadLock) prevents
// double-sling of the same bead, but does not prevent concurrent slings from racing on
// the same assignee's hook_bead field in Dolt. This lock is held only during
// hookBeadWithRetry. Uses non-blocking try-acquire with retry and timeout to avoid
// indefinite blocking if a sling gets stuck.
// See: https://github.com/steveyegge/gastown/issues/3114
func tryAcquireSlingAssigneeLock(townRoot, targetAgent string) (func(), error) {
	lockDir := filepath.Join(townRoot, ".runtime", "locks", "sling")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating sling lock dir: %w", err)
	}

	sweepStaleSlingFlocks(lockDir)

	safeAgent := strings.NewReplacer("/", "_", ":", "_").Replace(targetAgent)
	lockPath := filepath.Join(lockDir, "assignee_"+safeAgent+".flock")

	// Try non-blocking acquire with retry. hookBeadWithRetry itself has 10 retries
	// with up to 30s backoff, so we allow generous total wait time for the lock.
	const maxAttempts = 20
	const retryInterval = 500 // milliseconds
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		unlock, err := lock.FlockTryAcquireStable(lockPath)
		if err == nil {
			return unlinkThenUnlock(lockPath, unlock), nil
		}
		if !errors.Is(err, lock.ErrFlockHeld) {
			return nil, fmt.Errorf("acquiring assignee sling lock for %s: %w", targetAgent, err)
		}
		if attempt < maxAttempts {
			time.Sleep(time.Duration(retryInterval) * time.Millisecond)
		}
	}

	return nil, fmt.Errorf("timed out acquiring assignee sling lock for %s after %ds (another sling may be stuck)", targetAgent, maxAttempts*retryInterval/1000)
}

// resolvePRBranch resolves a GitHub PR number to its head branch name via `gh pr view`.
// Used by `gt sling --pr <number>` to convert the PR number into a branch name that
// the polecat worktree can check out.
func resolvePRBranch(prNumber int) (string, error) {
	cmd := exec.Command("gh", "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "headRefName", "-q", ".headRefName")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("gh pr view: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("PR #%d has no headRefName (does it exist?)", prNumber)
	}
	return branch, nil
}

// rollbackSlingArtifacts cleans up artifacts left by a partial sling.
// This prevents zombie polecats that block subsequent sling attempts with "bead already hooked".
// Cleanup is best-effort: each step logs warnings but continues to clean as much as possible.
// beadID is the bead this sling touched ("" when the failure came before the
// sling wrote to any bead); it is never unhooked from a different assignee.
func rollbackSlingArtifacts(spawnInfo *SpawnedPolecatInfo, beadID, hookWorkDir, convoyID string) {
	realSlingRollback().rollback(spawnInfo, beadID, hookWorkDir, convoyID)
}
