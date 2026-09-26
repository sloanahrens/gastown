package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/lintlock"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/telemetry"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/workspace"
)

var doneCmd = &cobra.Command{
	Use:         "done",
	GroupID:     GroupWork,
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Signal work ready for merge queue",
	Long: `Signal that your work is complete and ready for the merge queue.

This is a convenience command for polecats that:
1. Submits the current branch to the merge queue
2. Auto-detects issue ID from branch name
3. Notifies the Witness with the exit outcome
4. Exits the polecat session after durable handoff
   (Witness/refinery cleanup owns the retired sandbox)

Exit statuses:
  COMPLETED      - Work done, MR submitted (default)
  ESCALATED      - Hit blocker, needs human intervention
  DEFERRED       - Work paused, issue still open

Examples:
  gt done                              # Submit branch, notify COMPLETED, exit session
  gt done --pre-verified               # Submit with pre-verification fast-path
  gt done --target feat/my-branch      # Explicit MR target branch
  gt done --pre-verified --target feat/contract-review  # Pre-verified with explicit target
  gt done --issue gt-abc               # Explicit issue ID
  gt done --skip-verify                # Audit-only escape hatch for non-code closes
  gt done --status ESCALATED           # Signal blocker, skip MR
  gt done --status DEFERRED            # Pause work, skip MR`,
	RunE:         runDone,
	SilenceUsage: true, // Don't print usage on operational errors (confuses agents)
}

var (
	doneIssue         string
	donePriority      int
	doneStatus        string
	doneCleanupStatus string
	doneResume        bool
	donePreVerified   bool
	doneTarget        string
	doneSkipVerify    bool
	doneAllowReverts  bool

	doneAllowThrowawayPaths bool
)

// Valid exit types for gt done
const (
	ExitCompleted = "COMPLETED"
	ExitEscalated = "ESCALATED"
	ExitDeferred  = "DEFERRED"
)

// envDoneFromHandoff marks a `gt done` subprocess as gt handoff's polecat
// redirect (handoff.go), not a directly- or agent-issued final status report.
// Session retirement must not apply to this path (gt-5g3e): polecat-CLAUDE.md
// promises a mid-work handoff continues the work, and only the Witness's
// lifecycle handling owns the polecat from here.
const envDoneFromHandoff = "GT_DONE_FROM_HANDOFF"

func doneContaminationBaseRef(defaultBranch, explicitTarget string) string {
	targetBranch := defaultBranch
	if explicitTarget != "" {
		targetBranch = strings.TrimSpace(explicitTarget)
		if strings.HasPrefix(targetBranch, "origin/") || strings.HasPrefix(targetBranch, "upstream/") {
			return targetBranch
		}
	}

	return "origin/" + targetBranch
}

func shouldUpdateAgentStateOnDone(pushFailed, mrFailed bool) bool {
	return !pushFailed && !mrFailed
}

// isFinalDoneExitType reports whether a gt done exit status ends the polecat's
// turn on its hooked bead: the outcome is signaled and the session has nothing
// left to do. A deferred *issue* is still a finished *turn*, so DEFERRED ends
// the session exactly like COMPLETED and ESCALATED. Only a non-final exit
// (legacy PHASE_COMPLETE, which recycles a polecat still mid-workflow) leaves it
// running.
func isFinalDoneExitType(exitType string) bool {
	switch exitType {
	case ExitCompleted, ExitEscalated, ExitDeferred:
		return true
	default:
		return false
	}
}

func shouldRetirePolecatSessionAfterDone(exitType, mergeStrategy string, pushFailed, mrFailed, fromHandoff bool) bool {
	// A handoff-triggered DEFERRED defers the work mid-task; the polecat (or its
	// successor) is expected to keep going, so it must never be torn down here,
	// regardless of exit type (gt-5g3e).
	if fromHandoff {
		return false
	}
	// A polecat that has signaled a final status and stays alive keeps spending
	// tokens on work it already reported finished (gt-5g3e).
	if !isFinalDoneExitType(exitType) {
		return false
	}
	// A failed push or MR submission leaves work only recoverable from this
	// session, and a local-review merge strategy still expects a human in it.
	if pushFailed || mrFailed {
		return false
	}
	return mergeStrategy != "local"
}

// shouldResolveConvoyForRetirement reports whether gt done still needs to look
// up convoy info to correctly gate session retirement. Every final exit type
// needs this now that DEFERRED and ESCALATED can also retire the session, not
// only COMPLETED (gt-5g3e): a "local" merge strategy must exempt those exits
// from retirement exactly as it does COMPLETED.
func shouldResolveConvoyForRetirement(issueID string, convoyInfo *ConvoyInfo) bool {
	return issueID != "" && convoyInfo == nil
}

type doneSessionKiller interface {
	KillSessionWithProcessesExcluding(name string, excludePIDs []string) error
}

type donePolecatWorktree struct {
	townRoot    string
	cwd         string
	rigName     string
	polecatName string
	actor       string
}

var newDoneSessionKiller = func() doneSessionKiller {
	return tmux.NewTmux()
}

var updateAgentStateOnDoneFn = updateAgentStateOnDone

func updateAgentStateAfterSubmission(cwd, townRoot, exitType, issueID string, pushFailed, mrFailed bool) error {
	if !shouldUpdateAgentStateOnDone(pushFailed, mrFailed) {
		style.PrintWarning("skipping agent cleanup because push or MR submission failed")
		return nil
	}
	return updateAgentStateOnDoneFn(cwd, townRoot, exitType, issueID)
}

// resolvePreVerifiedClaim decides whether to honor a --pre-verified request
// when writing the MR bead's pre_verified stamp. It exists so the stamp can
// never diverge from the same gate-command binding `gt sling` uses to
// populate formula vars: a rig with zero configured gate commands has
// nothing a polecat could have verified, so the claim is downgraded here
// rather than trusted at face value (gt-k4sy).
//
// Returns whether to honor the claim, and a non-empty warning to surface to
// the polecat when the claim is downgraded.
func resolvePreVerifiedClaim(requested bool, townRoot, rigName string) (honor bool, warning string) {
	if !requested {
		return false, ""
	}
	// runPreVerificationGates runs the five *_command gates only, so the stamp
	// cannot cover a named merge_queue.gates entry — the refinery's
	// resolveFastPath refuses such a stamp for the same reason. Refusing at the
	// producer keeps the two sides agreeing on what a stamp means instead of
	// queueing a claim nothing will honor (gt-ypkc).
	if gates := rig.LoadNamedGateCommands(townRoot, rigName); len(gates) > 0 {
		return false, fmt.Sprintf("ignoring --pre-verified: rig defines %d named merge_queue.gates entry(ies), which the pre-verification run does not cover — the refinery will run them", len(gates))
	}
	mq := rig.ResolveMergeQueueConfig(townRoot, rigName)
	if !mq.HasAnyGateCommand() {
		return false, "ignoring --pre-verified: rig has no configured gate commands (setup/typecheck/lint/test/build) — there is nothing to have verified"
	}
	return true, ""
}

// preVerificationStamp holds the pre_verified_* fields to append to the MR
// bead description once a --pre-verified gate run is trusted.
type preVerificationStamp struct {
	verifiedBase string
	gateSetSHA   string
	logSHA256    string
}

// resolvePreVerification runs (and gates trust in) the --pre-verified check
// that gt done stamps into the MR bead. It refuses to stamp — returning
// ok=false with a warning instead — when the branch's HEAD does not
// actually contain the resolved target base as an ancestor.
//
// --pre-verified disables autoRebaseOnTarget (done_rebase.go), so a branch
// left behind the target runs gates at its own (stale) HEAD; without this
// check the stamp would record pre_verified_base=<target HEAD the branch
// never rebased onto>, and the refinery's fast-path would then skip gates
// on a merged tree nothing ever verified (om-gate T8).
func resolvePreVerification(g *git.Git, worktree, defaultBranch, target string, mq *config.MergeQueueConfig, gateSetSHA string, gateSlot preVerifySlot) (preVerificationStamp, bool, string) {
	verifiedBaseRef := g.CleanBaseRef("origin", defaultBranch, target)
	verifiedBase, baseErr := g.Rev(verifiedBaseRef)
	if baseErr != nil {
		return preVerificationStamp{}, false, fmt.Sprintf("could not resolve %s for pre-verified base: %v (skipping pre-verification)", verifiedBaseRef, baseErr)
	}

	ancestor, ancErr := g.IsAncestor(verifiedBase, "HEAD")
	if ancErr != nil {
		return preVerificationStamp{}, false, fmt.Sprintf("could not verify %s is an ancestor of HEAD: %v (skipping pre-verification)", shortSHA(verifiedBase), ancErr)
	}
	if !ancestor {
		return preVerificationStamp{}, false, fmt.Sprintf("--pre-verified: HEAD does not contain %s (branch is behind %s and --pre-verified skips auto-rebase) — refusing to stamp a base the branch was never verified against; rebase onto %s and retry", shortSHA(verifiedBase), target, target)
	}

	result, runErr := runPreVerificationGates(worktree, mq, gateSlot)
	if runErr != nil {
		return preVerificationStamp{}, false, fmt.Sprintf("--pre-verified: could not run gates: %v (MR will not carry the pre-verified stamp)", runErr)
	}
	if !result.success {
		return preVerificationStamp{}, false, fmt.Sprintf("--pre-verified: gate %q failed (exit %d) — see %s; MR will not carry the pre-verified stamp", result.failedGate, result.exitCode, result.logPath)
	}

	return preVerificationStamp{
		verifiedBase: verifiedBase,
		gateSetSHA:   gateSetSHA,
		logSHA256:    result.logSHA256,
	}, true, ""
}

// preVerificationResult is the outcome of runPreVerificationGates.
type preVerificationResult struct {
	success    bool
	failedGate string // name of the gate that failed; empty on success or no-op
	exitCode   int    // failing gate's exit code, or 0 on success/no-op
	logPath    string // path to the captured combined stdout/stderr log
	logSHA256  string // sha256 hex digest of the log; empty unless success
}

// preVerificationGateTimeout bounds each individual pre-verification gate
// command. Without it a hung gate (e.g. a test waiting on a port) blocks gt
// done indefinitely at a point where the branch may already be pushed but no
// MR bead exists yet. The refinery's own GateConfig.Timeout is per-gate and
// operator-configured; gt done's gate commands (the polecat-side *_command
// set) carry no such per-gate config, so this is a single generous fixed
// bound instead.
//
// A var rather than a const so a test can drive the timeout branch without
// sleeping out ten minutes (gt-ypkc).
var preVerificationGateTimeout = 10 * time.Minute

// preVerificationGateOutcome is one pre-verification gate command's result:
// the run error (nil on success) and whether it was the gate's timeout that
// ended it, which the caller reports distinctly from a failing exit code.
type preVerificationGateOutcome struct {
	err      error
	timedOut bool
}

// runPreVerificationGate runs one gate command in worktree, streaming combined
// output to logFile. Split out of runPreVerificationGates' loop so the lint
// gate can be run more than once when golangci-lint's lock is held (gt-xsty),
// which is also why the caller owns ctx: every attempt of one gate draws on
// that gate's single preVerificationGateTimeout budget, rather than a retry
// getting a fresh one.
func runPreVerificationGate(ctx context.Context, worktree, script string, logFile *os.File) preVerificationGateOutcome {
	// Trust boundary: gate commands come from rig config.json (operator-
	// controlled infrastructure config), not from PR branches or user
	// input — same trust boundary as the refinery's own gate runner.
	cmd := exec.CommandContext(ctx, "sh", "-c", script) //nolint:gosec // G204: command is from trusted rig config
	cmd.Dir = worktree
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// SetProcessGroup, not SetDetachedProcessGroup: the group's Cancel hook is
	// what makes the gate's deadline reach the script's own children — a hung
	// `make test` otherwise leaves the test binary running after gt done has
	// gone (gt-ypkc).
	util.SetProcessGroup(cmd)
	err := cmd.Run()
	return preVerificationGateOutcome{err: err, timedOut: ctx.Err() == context.DeadlineExceeded}
}

// runPreVerificationGateHeld runs one pre-verification gate under its own
// preVerificationGateTimeout budget, holding the container-gate slot for the
// test gate when gateSlot.hold says it needs one (gt-l6by). The slot is taken
// before the budget starts, so its wait is never charged to the gate, and it
// is released by defer however the gate ends. A non-nil error means the slot
// could not be taken and the gate did not run.
func runPreVerificationGateHeld(name, script, worktree, logPath string, logFile *os.File, mq *config.MergeQueueConfig, gateSlot preVerifySlot) (preVerificationGateOutcome, error) {
	if name == "test" {
		release, err := gateSlot.hold(worktree, script, mq, logFile)
		if err != nil {
			return preVerificationGateOutcome{}, err
		}
		defer release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), preVerificationGateTimeout)
	defer cancel()
	var got preVerificationGateOutcome
	runGate := func() lintlock.Attempt {
		from := fileSize(logFile)
		got = runPreVerificationGate(ctx, worktree, script, logFile)
		return lintlock.Attempt{Err: got.err, Output: readLogFrom(logPath, from)}
	}
	if name == "lint" {
		// gt-xsty: lint is the only gate with a cross-process lock
		// (golangci-lint's), so a concurrent lint is waited out rather
		// than costing the submission its pre-verified stamp. This gate's
		// own 10m bound is the retry budget.
		_ = lintlock.Retry(ctx, runGate, func(attempt, attempts int, wait time.Duration) {
			fmt.Fprintf(logFile, "=== gate lint: another golangci-lint holds the lock (attempt %d/%d); retrying in %s ===\n", attempt, attempts, wait.Round(time.Second))
		})
	} else {
		runGate()
	}
	return got, nil
}

// runPreVerificationGates performs the verification `gt done --pre-verified`
// stamps: it runs each of the rig's configured gate commands, in
// setup/typecheck/lint/build/test order, in worktree, streaming combined
// output to <worktree>/.runtime/gt-preverify.log. It stops at the first
// failing gate. There is no path that reports success without this function
// having actually executed every configured command (om-gate T8).
//
// The test gate runs inside a container-gate slot whenever it may start a
// container-backed suite (gt-l6by, see resolvePreVerifyTestSlot): a stamped MR
// skips the refinery's gate, so this run is the one that exercises the Docker
// suite and it must hold a slot like every other suite that does. The slot
// wait is not counted against the gate's preVerificationGateTimeout budget.
func runPreVerificationGates(worktree string, mq *config.MergeQueueConfig, gateSlot preVerifySlot) (preVerificationResult, error) {
	type namedGate struct {
		name string
		cmd  string
	}
	var gates []namedGate
	if mq != nil {
		for _, ng := range []namedGate{
			{"setup", mq.SetupCommand},
			{"typecheck", mq.TypecheckCommand},
			{"lint", mq.LintCommand},
			{"build", mq.BuildCommand},
			{"test", mq.TestCommand},
		} {
			if ng.cmd != "" {
				gates = append(gates, ng)
			}
		}
	}

	// Written under .runtime/ (constants.DirRuntime) so it is a recognized
	// runtime artifact (git.go's runtimeArtifactRoot): untracked at the
	// worktree root it made CleanExcludingRuntime() false, tripping gt done's
	// uncommitted-work checks on re-runs (om-gate T8).
	logDir := filepath.Join(worktree, constants.DirRuntime)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return preVerificationResult{}, fmt.Errorf("creating pre-verification log dir %s: %w", logDir, err)
	}
	logPath := filepath.Join(logDir, "gt-preverify.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return preVerificationResult{}, fmt.Errorf("creating pre-verification log %s: %w", logPath, err)
	}
	defer logFile.Close()

	for _, ng := range gates {
		fmt.Fprintf(logFile, "=== gate %s: %s ===\n", ng.name, ng.cmd)
		got, slotErr := runPreVerificationGateHeld(ng.name, ng.cmd, worktree, logPath, logFile, mq, gateSlot)
		if slotErr != nil {
			return preVerificationResult{}, slotErr
		}
		if got.timedOut {
			// A hung gate (e.g. a test waiting on a port) must not wedge gt
			// done indefinitely at a point where the branch is already
			// pushed but no MR bead exists yet — degrade to "no stamp"
			// instead (om-gate T8).
			fmt.Fprintf(logFile, "=== gate %s timed out after %s ===\n", ng.name, preVerificationGateTimeout)
			return preVerificationResult{success: false, failedGate: ng.name, exitCode: -1, logPath: logPath}, nil
		}
		if got.err != nil {
			exitCode := -1
			var exitErr *exec.ExitError
			if errors.As(got.err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
			return preVerificationResult{success: false, failedGate: ng.name, exitCode: exitCode, logPath: logPath}, nil
		}
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		return preVerificationResult{}, fmt.Errorf("reading pre-verification log %s: %w", logPath, err)
	}
	sum := sha256.Sum256(logBytes)
	return preVerificationResult{success: true, logPath: logPath, logSHA256: hex.EncodeToString(sum[:])}, nil
}

func resolveDonePolecatWorktree() (donePolecatWorktree, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return donePolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable: %w", err)
	}
	return resolveDonePolecatWorktreeAt(cwd)
}

func resolveDonePolecatWorktreeAt(cwd string) (donePolecatWorktree, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return donePolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable")
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return donePolecatWorktree{}, fmt.Errorf("resolving current directory: %w", err)
	}
	if info, err := os.Stat(absCwd); err != nil {
		return donePolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable: %w", err)
	} else if !info.IsDir() {
		return donePolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current path is not a directory: %s", absCwd)
	}

	townRoot, err := workspace.FindOrError(absCwd)
	if err != nil {
		return donePolecatWorktree{}, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if err := doneValidateSessionTownRoot(townRoot); err != nil {
		return donePolecatWorktree{}, err
	}

	actorRig, actorName, err := donePolecatActorIdentity(os.Getenv("BD_ACTOR"))
	if err != nil {
		return donePolecatWorktree{}, err
	}
	roleRig, roleName, err := donePolecatEnvIdentity(os.Getenv("GT_ROLE"), os.Getenv("GT_RIG"), os.Getenv("GT_POLECAT"))
	if err != nil {
		return donePolecatWorktree{}, err
	}
	if actorRig != roleRig || actorName != roleName {
		return donePolecatWorktree{}, fmt.Errorf("gt done identity mismatch: BD_ACTOR=%s/polecats/%s but GT_ROLE/GT_RIG/GT_POLECAT resolve to %s/polecats/%s", actorRig, actorName, roleRig, roleName)
	}
	if err := doneRejectGitEnvOverrides(); err != nil {
		return donePolecatWorktree{}, err
	}

	gitRoot, err := doneGitTopLevel(absCwd)
	if err != nil {
		return donePolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat git worktree: %w", err)
	}
	gitRoot = doneCanonicalPath(gitRoot)
	canonicalCwd := doneCanonicalPath(absCwd)
	if !donePathWithin(gitRoot, canonicalCwd) {
		return donePolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory %s is outside git root %s", canonicalCwd, gitRoot)
	}

	candidates, err := donePolecatWorktreeCandidates(townRoot, actorRig, actorName)
	if err != nil {
		return donePolecatWorktree{}, err
	}
	for _, candidate := range candidates {
		if gitRoot == doneCanonicalPath(candidate) {
			return donePolecatWorktree{
				townRoot:    townRoot,
				cwd:         gitRoot,
				rigName:     actorRig,
				polecatName: actorName,
				actor:       fmt.Sprintf("%s/polecats/%s", actorRig, actorName),
			}, nil
		}
	}

	return donePolecatWorktree{}, fmt.Errorf("gt done must be run from assigned polecat worktree %s; current git root is %s", strings.Join(candidates, " or "), gitRoot)
}

func donePolecatWorktreeCandidates(townRoot, rigName, polecatName string) ([]string, error) {
	nested := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	info, err := os.Stat(nested)
	if err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("assigned polecat worktree path is not a directory: %s", nested)
		}
		return []string{nested}, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking assigned polecat worktree %s: %w", nested, err)
	}

	return []string{filepath.Join(townRoot, rigName, "polecats", polecatName)}, nil
}

func donePolecatActorIdentity(actor string) (string, string, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", "", fmt.Errorf("gt done requires BD_ACTOR to identify the assigned polecat")
	}
	parts := strings.Split(actor, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] != "polecats" || parts[2] == "" {
		return "", "", fmt.Errorf("gt done is for polecats only (BD_ACTOR=%s)", actor)
	}
	if err := doneValidateIdentitySegment("BD_ACTOR rig", parts[0]); err != nil {
		return "", "", err
	}
	if err := doneValidateIdentitySegment("BD_ACTOR polecat", parts[2]); err != nil {
		return "", "", err
	}
	return parts[0], parts[2], nil
}

func donePolecatEnvIdentity(gtRole, gtRig, gtPolecat string) (string, string, error) {
	gtRole = strings.TrimSpace(gtRole)
	gtRig = strings.TrimSpace(gtRig)
	gtPolecat = strings.TrimSpace(gtPolecat)
	if gtRole == "" {
		return "", "", fmt.Errorf("gt done requires GT_ROLE to identify the assigned polecat")
	}
	if gtRig == "" {
		return "", "", fmt.Errorf("gt done requires GT_RIG to identify the assigned polecat")
	}
	if gtPolecat == "" {
		return "", "", fmt.Errorf("gt done requires GT_POLECAT to identify the assigned polecat")
	}
	if err := doneValidateIdentitySegment("GT_RIG", gtRig); err != nil {
		return "", "", err
	}
	if err := doneValidateIdentitySegment("GT_POLECAT", gtPolecat); err != nil {
		return "", "", err
	}

	roleRig, rolePolecat, err := donePolecatRoleIdentity(gtRole)
	if err != nil {
		return "", "", err
	}
	if roleRig != "" && roleRig != gtRig {
		return "", "", fmt.Errorf("gt done identity mismatch: GT_ROLE rig %s != GT_RIG %s", roleRig, gtRig)
	}
	if rolePolecat != "" && rolePolecat != gtPolecat {
		return "", "", fmt.Errorf("gt done identity mismatch: GT_ROLE polecat %s != GT_POLECAT %s", rolePolecat, gtPolecat)
	}

	return gtRig, gtPolecat, nil
}

func donePolecatRoleIdentity(gtRole string) (string, string, error) {
	if gtRole == string(RolePolecat) {
		return "", "", nil
	}
	parts := strings.Split(gtRole, "/")
	switch len(parts) {
	case 2:
		role, roleRig, rolePolecat := parseRoleString(gtRole)
		if role != RolePolecat || roleRig == "" || rolePolecat == "" {
			return "", "", fmt.Errorf("gt done is for polecats only (GT_ROLE=%s)", gtRole)
		}
		if err := doneValidateIdentitySegment("GT_ROLE rig", roleRig); err != nil {
			return "", "", err
		}
		if err := doneValidateIdentitySegment("GT_ROLE polecat", rolePolecat); err != nil {
			return "", "", err
		}
		return roleRig, rolePolecat, nil
	case 3:
		if parts[1] != "polecats" || parts[0] == "" || parts[2] == "" {
			return "", "", fmt.Errorf("gt done is for polecats only (GT_ROLE=%s)", gtRole)
		}
		if err := doneValidateIdentitySegment("GT_ROLE rig", parts[0]); err != nil {
			return "", "", err
		}
		if err := doneValidateIdentitySegment("GT_ROLE polecat", parts[2]); err != nil {
			return "", "", err
		}
		return parts[0], parts[2], nil
	default:
		return "", "", fmt.Errorf("gt done is for polecats only (GT_ROLE=%s)", gtRole)
	}
}

func doneValidateIdentitySegment(name, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("gt done invalid %s: %q is not a single path segment", name, value)
	}
	return nil
}

func doneValidateSessionTownRoot(townRoot string) error {
	current := doneCanonicalPath(townRoot)
	for _, envName := range []string{"GT_TOWN_ROOT", "GT_ROOT"} {
		envRoot := strings.TrimSpace(os.Getenv(envName))
		if envRoot == "" {
			continue
		}
		if doneCanonicalPath(envRoot) != current {
			return fmt.Errorf("gt done town root mismatch: %s=%s but current workspace is %s", envName, doneCanonicalPath(envRoot), current)
		}
	}
	return nil
}

func donePathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func doneRejectGitEnvOverrides() error {
	for _, envName := range []string{
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE",
	} {
		if strings.TrimSpace(os.Getenv(envName)) != "" {
			return fmt.Errorf("gt done requires an unambiguous git worktree; unset %s", envName)
		}
	}
	return nil
}

func doneGitTopLevel(cwd string) (string, error) {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolving git root for %s: %s", cwd, strings.TrimSpace(string(output)))
	}
	gitRoot := strings.TrimSpace(string(output))
	if gitRoot == "" {
		return "", fmt.Errorf("git root for %s is empty", cwd)
	}
	return gitRoot, nil
}

func doneCanonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

func polecatSessionRetirementTarget(rigName, polecatName string, pid int) (string, []string, bool) {
	if rigName == "" || polecatName == "" || pid <= 0 {
		return "", nil, false
	}
	return session.PolecatSessionName(session.PrefixFor(rigName), polecatName), []string{fmt.Sprintf("%d", pid)}, true
}

func retirePolecatSessionAfterDone(rigName, polecatName string, pid int) error {
	sessionName, excludePIDs, ok := polecatSessionRetirementTarget(rigName, polecatName, pid)
	if !ok {
		return nil
	}
	return newDoneSessionKiller().KillSessionWithProcessesExcluding(sessionName, excludePIDs)
}

// retirePolecatSessionAfterFinalExit decides whether this exit retires the live
// polecat session and, when it does, tears the session down. The decision and
// the kill share one function so a final status cannot report itself retired
// and then skip the kill; tests drive this path directly (gt-5g3e).
//
// Call it as gt done's last action. retirePolecatSessionAfterDone excludes the
// caller's own PID, so the durable handoff writes above it still finish.
func retirePolecatSessionAfterFinalExit(exitType, mergeStrategy string, pushFailed, mrFailed, fromHandoff bool, rigName, polecatName string, pid int) bool {
	if !shouldRetirePolecatSessionAfterDone(exitType, mergeStrategy, pushFailed, mrFailed, fromHandoff) {
		fmt.Printf("%s Session preserved for recovery, local review, or handoff continuation\n", style.Bold.Render("→"))
		return false
	}
	fmt.Printf("%s Polecat session retiring after durable handoff\n", style.Bold.Render("✓"))
	fmt.Printf("%s Terminating polecat session\n", style.Bold.Render("→"))
	if err := retirePolecatSessionAfterDone(rigName, polecatName, pid); err != nil {
		style.PrintWarning("could not terminate polecat session: %v", err)
	}
	return true
}

func cleanupStatusAfterSuccessfulPush(status string) string {
	if status == "unpushed" || status == "has_unpushed" {
		return "clean"
	}
	return status
}

// observeCleanupStatus derives the polecat's self-reported cleanup status from
// the live worktree: uncommitted files, stashes, and whether the branch is
// pushed to origin. It returns "" when git cannot be read at all.
//
// CheckUncommittedWork.UnpushedCommits doesn't work for branches without
// upstream tracking (common for polecats), so the pushed check goes through the
// more robust BranchPushedToRemote, which compares against origin/main.
func observeCleanupStatus(g *git.Git, branch string) string {
	workStatus, err := g.CheckUncommittedWork()
	if err != nil {
		style.PrintWarning("could not auto-detect cleanup status: %v", err)
		return ""
	}
	pushed, unpushedCount, pushErr := g.BranchPushedToRemote(branch, "origin")
	if pushErr != nil {
		style.PrintWarning("could not check if branch is pushed: %v", pushErr)
	}
	return cleanupStatusFromWorkState(workStatus, pushed, unpushedCount, pushErr)
}

// resolveDoneAgentIdentity returns the role context gt done writes its
// agent-bead lifecycle metadata through, plus the actor string it logs under.
//
// The polecat identity is already proven by resolveDonePolecatWorktree: gt done
// refuses to run unless BD_ACTOR and GT_ROLE/GT_RIG/GT_POLECAT agree on one
// polecat and cwd is that polecat's worktree. Seeding the context from those
// validated identifiers, and letting env/cwd detection merely refine it, means
// the agent-bead ID no longer depends on that detection succeeding at all.
//
// That dependency was the silent hole: getAgentBeadID returns "" for a
// RoleUnknown/rig-less context, and every agent-bead write in gt done is
// guarded by `agentBeadID != ""`. When role detection degraded, gt done skipped
// the done-intent label, the resume checkpoints, active_mr, the completion
// metadata, agent_state AND the cleanup_status self-report, then exited 0 and
// logged "[done]" — leaving a slot that reads cleanup_status=<missing> with no
// later writer to repair it (see selfReportCleanupStatus and reclaim.go).
func resolveDoneAgentIdentity(cwd, townRoot, rigName, polecatName string) (RoleContext, string) {
	ctx := RoleContext{
		Role:     RolePolecat,
		Rig:      rigName,
		Polecat:  polecatName,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return ctx, ""
	}
	if roleInfo.Role != RoleUnknown {
		ctx.Role = roleInfo.Role
	}
	if roleInfo.Rig != "" {
		ctx.Rig = roleInfo.Rig
	}
	if roleInfo.Polecat != "" {
		ctx.Polecat = roleInfo.Polecat
	}
	// Only a named detection contributes a log actor. ActorString degrades to
	// the literal "unknown" for a RoleUnknown context, which would otherwise
	// replace the already-validated BD_ACTOR sender on the "[done]" townlog line
	// and the feed event.
	if roleInfo.Role == RoleUnknown {
		return ctx, ""
	}
	return ctx, roleInfo.ActorString()
}

// resolveCleanupStatusForSelfReport picks the value gt done records on its
// agent bead: the status the run already computed, else a freshly observed one,
// else an explicit CleanupUnknown. It is TOTAL — it never returns an empty
// value, which is the whole point (see selfReportCleanupStatus).
func resolveCleanupStatusForSelfReport(doneCleanupStatus, observedStatus string) polecat.CleanupStatus {
	if status := parseCleanupStatus(doneCleanupStatus); status != polecat.CleanupUnknown {
		return status
	}
	if status := parseCleanupStatus(observedStatus); status != polecat.CleanupUnknown {
		return status
	}
	return polecat.CleanupUnknown
}

// selfReportCleanupStatus writes the polecat's cleanup_status to its agent bead.
// The write is TOTAL by construction: every completed gt done leaves a value.
//
// cleanup_status is the durable evidence that decides whether a slot may be
// reclaimed (nuke, check-recovery, broken-idle reclaim), and nothing else ever
// fills it in — reclaim.go documents that a missing/unknown status can never
// become anything else. A path that skips this write therefore strands the slot
// permanently and drives re-dispatch storms (hq-vx224: flint, granite, shale,
// agate, basalt ... blocked cleanup rig-wide).
//
// Two shapes of skip are closed here:
//
//   - A push or MR failure returns early from updateAgentStateAfterSubmission,
//     which used to take the self-report with it. A failed push is exactly the
//     case where the witness needs to see "has_unpushed".
//   - A status that could not be observed stays "" or parses to CleanupUnknown.
//     Re-observing the live worktree at completion time is the last chance to
//     record a real value; if even that fails, "unknown" is recorded rather
//     than nothing. "unknown" is not a clearance — CleanupStatus.IsSafe() is
//     false for it and workstate.go's gates treat it exactly like an empty
//     value, so recording it is fail-closed. It only makes "gt done ran and
//     could not prove the tree was safe" distinguishable from "gt done never
//     ran", which is what the blocked slots were indistinguishable from.
func selfReportCleanupStatus(g *git.Git, branch string, updater cleanupStatusUpdater, agentBeadID, doneCleanupStatus string) {
	if agentBeadID == "" {
		style.PrintWarning("no agent bead ID for this polecat; cleanup_status not recorded — the slot will read as cleanup_status=<missing> and cannot be reclaimed")
		return
	}
	observed := ""
	if parseCleanupStatus(doneCleanupStatus) == polecat.CleanupUnknown {
		observed = observeCleanupStatus(g, branch)
	}
	status := resolveCleanupStatusForSelfReport(doneCleanupStatus, observed)
	if err := updater.UpdateAgentCleanupStatus(agentBeadID, string(status)); err != nil {
		// Non-fatal: don't return — done-intent labels still need clearing (za-o9e)
		fmt.Fprintf(os.Stderr, "Warning: couldn't update agent %s cleanup status: %v\n", agentBeadID, err)
	}
}

func cleanupStatusFromWorkState(workStatus *git.UncommittedWorkStatus, branchPushed bool, unpushedCount int, branchPushedErr error) string {
	if workStatus == nil {
		return "unknown"
	}
	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
		return "uncommitted"
	}
	if workStatus.StashCount > 0 {
		return "stash"
	}
	if branchPushedErr != nil || !branchPushed || unpushedCount > 0 {
		return "unpushed"
	}
	return "clean"
}

var reviewEvidencePrefixes = []string{
	"report:",
	"findings:",
	"review:",
	"evidence:",
	"verdict:",
	"decision:",
	"pr-sheriff-evidence",
	"pr sheriff evidence",
}

var generatedCommentPrefixes = []string{
	"verified_push_",
	"mr created:",
}

func doneSourceCloseSkipReason(bd *beads.Beads, issueID string, issue *beads.Issue) (string, bool) {
	currentHead, _ := currentReviewEvidenceHead()
	return doneSourceCloseSkipReasonForHead(bd, issueID, issue, currentHead)
}

func doneDirectMergeSkipReason(bd *beads.Beads, issueID string, issue *beads.Issue, targetBranch string) string {
	if strings.TrimSpace(issueID) == "" {
		return "source issue is required for direct merge"
	}
	issue, skipReason, _ := loadDoneSourceIssue(bd, issueID, issue)
	if skipReason != "" {
		return skipReason
	}
	if err := validateConcreteSourceIssue(issueID, issue); err != nil {
		return err.Error()
	}
	if attachment := beads.ParseAttachmentFields(issue); attachment != nil {
		switch {
		case attachment.NoMerge:
			return fmt.Sprintf("source_issue %s has no_merge=true", issueID)
		case attachment.ReviewOnly:
			return fmt.Sprintf("review-only issue %s cannot be direct-merged to %s", issueID, targetBranch)
		case strings.EqualFold(strings.TrimSpace(attachment.MergeStrategy), "local"):
			return fmt.Sprintf("source_issue %s has merge_strategy=local", issueID)
		}
	}
	if unchecked := beads.HasUncheckedCriteria(issue); unchecked > 0 {
		return fmt.Sprintf("issue %s has %d unchecked acceptance criteria — skipping direct merge", issueID, unchecked)
	}
	return ""
}

func doneSourceCloseSkipReasonForHead(bd *beads.Beads, issueID string, issue *beads.Issue, currentHead string) (string, bool) {
	issue, skipReason, fatal := loadDoneSourceIssue(bd, issueID, issue)
	if skipReason != "" {
		return skipReason, fatal
	}
	if err := validateConcreteSourceIssue(issueID, issue); err != nil {
		return err.Error(), true
	}
	if attachment := beads.ParseAttachmentFields(issue); attachment != nil && strings.EqualFold(strings.TrimSpace(attachment.MergeStrategy), "local") {
		return fmt.Sprintf("issue %s has merge_strategy=local — skipping close", issueID), false
	}
	if skipReason, fatal := doneReviewOnlyCloseSkipReasonForHead(bd, issueID, issue, currentHead); skipReason != "" {
		return skipReason, fatal
	}
	if unchecked := beads.HasUncheckedCriteria(issue); unchecked > 0 {
		return fmt.Sprintf("issue %s has %d unchecked acceptance criteria — skipping close", issueID, unchecked), false
	}
	return "", false
}

func doneReviewOnlyCloseSkipReason(bd *beads.Beads, issueID string, issue *beads.Issue) (string, bool) {
	issue, skipReason, fatal := loadDoneSourceIssue(bd, issueID, issue)
	if skipReason != "" {
		return skipReason, fatal
	}
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil || !attachment.ReviewOnly {
		return "", false
	}
	currentHead, err := currentReviewEvidenceHead()
	if err != nil {
		return fmt.Sprintf("could not verify review evidence for %s: %v", issueID, err), true
	}
	return doneReviewOnlyCloseSkipReasonForHead(bd, issueID, issue, currentHead)
}

func doneReviewOnlyCloseSkipReasonForHead(bd *beads.Beads, issueID string, issue *beads.Issue, currentHead string) (string, bool) {
	issue, skipReason, fatal := loadDoneSourceIssue(bd, issueID, issue)
	if skipReason != "" {
		return skipReason, fatal
	}
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil || !attachment.ReviewOnly {
		return "", false
	}
	assignmentAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(attachment.AttachedAt))
	if err != nil {
		return fmt.Sprintf("review-only issue %s has no fresh assignment timestamp — re-sling it or add evidence after a fresh assignment", issueID), true
	}
	if strings.TrimSpace(issue.Assignee) == "" {
		return fmt.Sprintf("review-only issue %s has no assignee for evidence author validation", issueID), true
	}
	currentHead = strings.TrimSpace(currentHead)
	if currentHead == "" {
		return fmt.Sprintf("review-only issue %s has no current HEAD for evidence validation", issueID), true
	}
	hasEvidence, err := hasFreshReviewReportEvidence(bd, issueID, issue, assignmentAt, issue.Assignee, currentHead)
	if err != nil {
		return fmt.Sprintf("could not verify review evidence for %s: %v", issueID, err), true
	}
	if !hasEvidence {
		return fmt.Sprintf("review-only issue %s has no fresh review evidence comment for assignee %s and head %s", issueID, strings.TrimSpace(issue.Assignee), currentHead), true
	}
	return "", false
}

func loadDoneSourceIssue(bd *beads.Beads, issueID string, issue *beads.Issue) (*beads.Issue, string, bool) {
	if issueID == "" {
		return nil, "", false
	}
	if issue != nil {
		return issue, "", false
	}
	if bd == nil {
		return nil, fmt.Sprintf("could not inspect issue %s close eligibility", issueID), true
	}
	loaded, err := bd.Show(issueID)
	if err != nil {
		return nil, fmt.Sprintf("could not inspect issue %s close eligibility: %v", issueID, err), true
	}
	return loaded, "", false
}

func currentReviewEvidenceHead() (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving current HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func hasFreshReviewReportEvidence(bd *beads.Beads, issueID string, issue *beads.Issue, assignmentAt time.Time, assignee, currentHead string) (bool, error) {
	if issue != nil {
		if hasFreshReviewEvidenceComment(issue.Comments, assignmentAt, assignee, currentHead) {
			return true, nil
		}
	}
	if bd == nil || issueID == "" {
		return false, nil
	}
	comments, err := bd.Comments(issueID)
	if err != nil {
		return false, err
	}
	if hasFreshReviewEvidenceComment(comments, assignmentAt, assignee, currentHead) {
		return true, nil
	}
	return false, nil
}

func hasFreshReviewEvidenceComment(comments []beads.Comment, assignmentAt time.Time, assignee, currentHead string) bool {
	assignee = strings.TrimSpace(assignee)
	currentHead = strings.TrimSpace(currentHead)
	if assignee == "" || currentHead == "" {
		return false
	}
	for _, comment := range comments {
		createdAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(comment.CreatedAt))
		if err != nil || !createdAt.After(assignmentAt) {
			continue
		}
		if strings.TrimSpace(comment.Author) != assignee {
			continue
		}
		if isGeneratedReviewComment(comment.Text) || !isReviewEvidenceText(comment.Text) {
			continue
		}
		if reviewEvidenceHeadSHA(comment.Text) != currentHead {
			continue
		}
		return true
	}
	return false
}

func isGeneratedReviewComment(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(strings.TrimSpace(line))
		if lower == "" {
			continue
		}
		for _, prefix := range generatedCommentPrefixes {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}

func reviewEvidenceHeadSHA(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		for _, key := range []string{"head_sha", "target_head_sha", "head"} {
			for _, sep := range []string{":", "="} {
				prefix := key + sep
				if strings.HasPrefix(lower, prefix) {
					return strings.TrimSpace(trimmed[len(prefix):])
				}
			}
		}
	}
	return ""
}

func isReviewEvidenceText(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		generated := false
		for _, prefix := range generatedCommentPrefixes {
			if strings.HasPrefix(lower, prefix) {
				generated = true
				break
			}
		}
		if generated {
			continue
		}
		for _, prefix := range reviewEvidencePrefixes {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}

// autoSaveSquashTitle builds the commit subject used when every commit on a
// polecat branch is machine-generated (gt-3wf): a conventional-commit prefix
// from the issue type, the issue title, and the issue id. Returns "" when
// neither the issue nor its id is known, letting the squash fall back to a
// generic subject.
func autoSaveSquashTitle(issue *beads.Issue, issueID string) string {
	title := ""
	issueType := ""
	if issue != nil {
		title = strings.TrimSpace(issue.Title)
		issueType = strings.ToLower(strings.TrimSpace(issue.Type))
	}
	if title == "" {
		if issueID == "" {
			return ""
		}
		return fmt.Sprintf("fix: implementation work for %s", issueID)
	}
	prefix := "feat"
	switch issueType {
	case "bug":
		prefix = "fix"
	case "chore":
		prefix = "chore"
	}
	if issueID != "" {
		return fmt.Sprintf("%s: %s (%s)", prefix, title, issueID)
	}
	return fmt.Sprintf("%s: %s", prefix, title)
}

func init() {
	doneCmd.Flags().StringVar(&doneIssue, "issue", "", "Source issue ID (default: parse from branch name)")
	doneCmd.Flags().IntVarP(&donePriority, "priority", "p", -1, "Override priority (0-4, default: inherit from issue)")
	doneCmd.Flags().StringVar(&doneStatus, "status", ExitCompleted, "Exit status: COMPLETED, ESCALATED, or DEFERRED")
	doneCmd.Flags().StringVar(&doneCleanupStatus, "cleanup-status", "", "Git cleanup status: clean, uncommitted, unpushed, stash, unknown (ZFC: agent-observed)")
	doneCmd.Flags().BoolVar(&doneResume, "resume", false, "Resume from last checkpoint (auto-detected, for Witness recovery)")
	doneCmd.Flags().BoolVar(&donePreVerified, "pre-verified", false, "Mark MR as pre-verified (polecat ran gates after rebasing onto target)")
	doneCmd.Flags().StringVar(&doneTarget, "target", "", "Explicit MR target branch (overrides formula_vars and auto-detection)")
	doneCmd.Flags().BoolVar(&doneSkipVerify, "skip-verify", false, "Skip verified-push checks for audit/test-only completion (recorded on bead)")
	doneCmd.Flags().BoolVar(&doneAllowReverts, "allow-reverts", false, "Submit a branch that undoes content already merged to the target (refused by default)")
	doneCmd.Flags().BoolVar(&doneAllowThrowawayPaths, "allow-throwaway-paths", false, "Submit a branch that adds scratch, backup or /tmp files to the target (refused by default)")

	rootCmd.AddCommand(doneCmd)
}

func runDone(cmd *cobra.Command, args []string) (retErr error) {
	defer func() { telemetry.RecordDone(context.Background(), strings.ToUpper(doneStatus), retErr) }()
	// Guard: Only polecats should call gt done
	// Crew, deacons, witnesses etc. don't use gt done - they persist across tasks.
	// Polecat sessions end with gt done — the session is cleaned up, but the
	// polecat's persistent identity (agent bead, CV chain) survives across assignments.
	actor := os.Getenv("BD_ACTOR")
	if actor != "" && !isPolecatActor(actor) {
		return fmt.Errorf("gt done is for polecats only (you are %s)\nPolecat sessions end with gt done — the session is cleaned up, but identity persists.\nOther roles persist across tasks and don't use gt done.", actor)
	}

	// Validate exit status
	exitType := strings.ToUpper(doneStatus)
	if exitType != ExitCompleted && exitType != ExitEscalated && exitType != ExitDeferred {
		return fmt.Errorf("invalid exit status '%s': must be COMPLETED, ESCALATED, or DEFERRED", doneStatus)
	}

	// Every final exit status retires the live polecat session after durable
	// handoff; failed submissions and local-review paths preserve it.

	worktree, err := resolveDonePolecatWorktree()
	if err != nil {
		return err
	}
	townRoot := worktree.townRoot
	cwd := worktree.cwd
	rigName := worktree.rigName
	polecatName := worktree.polecatName
	sender := worktree.actor

	g := git.NewGit(cwd)

	branch, err := g.CurrentBranch()
	if err != nil {
		return fmt.Errorf("getting current branch: %w", err)
	}
	if err := requireRealCurrentBranch(branch, "gt done"); err != nil {
		return err
	}

	// Auto-detect cleanup status if not explicitly provided
	// This prevents premature polecat cleanup by ensuring witness knows git state
	if doneCleanupStatus == "" {
		doneCleanupStatus = observeCleanupStatus(g, branch)
	}

	// SAFETY NET (gt-pvx, stash recovery): If we detected stashes belonging to
	// this branch, auto-pop them so the existing uncommitted-work auto-commit
	// path (below) catches the contents and saves them as a normal commit.
	//
	// Background: agents have been observed running `git stash` to clear the
	// working tree before rebase/checkout, then dying before `git stash pop`.
	// The stash entries become orphaned in .git/refs/stash, surviving for
	// indefinite periods and silently leaking work. By popping them on the way
	// out of `gt done`, the recovery flow turns "lost" stashes into a
	// committed safety-net snapshot.
	//
	// Pop happens oldest-first so the most recent state ends up on top of the
	// working tree (matches what a user would do manually). If any pop has
	// conflicts, we stop and let the agent/user resolve — surfacing the
	// conflict is better than silently dropping the stash.
	if doneCleanupStatus == "stash" {
		entries, err := g.StashListForBranch()
		if err != nil {
			style.PrintWarning("auto-pop: could not list stashes: %v — orphaned stashes may remain", err)
		} else if len(entries) > 0 {
			fmt.Printf("\n%s %d stash(es) detected on this branch — auto-popping (gt-pvx safety net)\n",
				style.Bold.Render("⚠"), len(entries))
			// Pop oldest first: iterate in reverse so newest lands on top.
			popFailed := false
			for i := len(entries) - 1; i >= 0; i-- {
				e := entries[i]
				fmt.Printf("  popping %s — %s\n", e.Ref, e.Message)
				if popErr := g.StashPop(e.Ref); popErr != nil {
					style.PrintWarning("auto-pop %s failed (likely conflict): %v", e.Ref, popErr)
					style.PrintWarning("stopping pop chain — resolve conflict manually then re-run gt done")
					popFailed = true
					break
				}
				// After each pop, stash refs shift; re-fetch the list before next pop.
				entries, err = g.StashListForBranch()
				if err != nil || len(entries) == 0 {
					break
				}
			}
			if !popFailed {
				// Re-evaluate cleanup status: pops likely produced uncommitted changes
				// that the next block will auto-commit. Worst case, status was already
				// uncommitted and the next block runs anyway.
				if workStatus, wsErr := g.CheckUncommittedWork(); wsErr == nil && workStatus.HasUncommittedChanges {
					doneCleanupStatus = "uncommitted"
					fmt.Printf("%s Stash content moved to working tree — will auto-commit below.\n",
						style.Bold.Render("✓"))
				} else {
					// Pops succeeded but produced nothing dirty (e.g. stashes were
					// already merged). Recompute status normally.
					doneCleanupStatus = ""
				}
			}
		}
	}

	// SAFETY NET: Auto-commit uncommitted work before ANY exit path (gt-pvx).
	// Polecats have been observed running gt done without committing their
	// implementation work (1000s of lines lost). This happened because:
	// 1. The agent skips the "commit changes" formula step
	// 2. The COMPLETED check blocks, but the agent retries with --status DEFERRED
	//    which skips all checks
	// 3. The agent's session dies after the error, before it can commit
	//
	// Auto-commit ensures work is NEVER lost regardless of exit type or agent behavior.
	// The commit message is clearly marked as an auto-save so reviewers know.
	if doneCleanupStatus == "uncommitted" {
		// Re-check to get file details (cleanup detection already confirmed uncommitted changes)
		workStatus, err := g.CheckUncommittedWork()
		if err == nil && workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
			if len(workStatus.UnmergedFiles) > 0 {
				return fmt.Errorf("cannot auto-save unmerged conflicts: %s\nResolve conflicts first, or use --status DEFERRED to exit without completing", strings.Join(workStatus.UnmergedFiles, ", "))
			}

			fmt.Printf("\n%s Uncommitted changes detected — auto-saving to prevent work loss\n", style.Bold.Render("⚠"))
			fmt.Printf("  Files: %s\n\n", workStatus.String())

			// Stage all changes (git add -A), then unstage overlay/runtime files (gt-p35)
			// and any deletions of tracked files (gt-pvx safety: never commit deletions).
			if addErr := g.Add("-A"); addErr != nil {
				style.PrintWarning("auto-commit: git add failed: %v — uncommitted work may be at risk", addErr)
			} else {
				// Unstage Gas Town overlay files that git add -A picked up.
				// These are runtime artifacts that must not be committed to repos.
				_ = g.ResetFiles("CLAUDE.local.md")
				// Only unstage CLAUDE.md if it contains the overlay marker
				if claudeData, readErr := os.ReadFile(filepath.Join(cwd, "CLAUDE.md")); readErr == nil {
					if strings.Contains(string(claudeData), templates.PolecatLifecycleMarker) {
						_ = g.ResetFiles("CLAUDE.md")
					}
				}
				// Unstage runtime/ephemeral artifacts using the centralized git policy.
				for _, path := range workStatus.RuntimeArtifactPaths() {
					_ = g.ResetFiles(path)
				}
				// Unstage throwaway files (gt-ozo4). This runs `git add -A` for the
				// same reason the checkpoint dog does, and would sweep up the same
				// scratch, /tmp, editor-backup and patch-leftover files. They stay
				// in the worktree, untracked, and are named so the polecat sees
				// which of them it is not getting a safety-net commit for.
				if throwaway := checkpoint.ThrowawayPaths(workStatus.UntrackedFiles); len(throwaway) > 0 {
					_ = g.ResetFiles(throwaway...)
					style.PrintWarning("auto-commit: left %d throwaway file(s) uncommitted: %s",
						len(throwaway), strings.Join(throwaway, ", "))
				}
				// Unstage deletions of tracked files. A safety-net auto-commit should
				// preserve work (additions + modifications), never destroy it (deletions).
				// This prevents the bug where a polecat's working tree has a missing
				// tracked file (e.g. .beads/metadata.json) and the auto-save commits
				// the deletion, breaking infrastructure for subsequent sessions.
				if stagedDeletions, delErr := g.StagedDeletions(); delErr == nil && len(stagedDeletions) > 0 {
					_ = g.ResetFiles(stagedDeletions...)
				}
				// Build a descriptive commit message
				autoMsg := "fix: auto-save uncommitted implementation work (gt-pvx safety net)"
				if issueFromBranch := parseBranchName(branch).Issue; issueFromBranch != "" {
					autoMsg = fmt.Sprintf("fix: auto-save uncommitted implementation work (%s, gt-pvx safety net)", issueFromBranch)
				}
				if commitErr := g.Commit(autoMsg); commitErr != nil {
					style.PrintWarning("auto-commit: git commit failed: %v — uncommitted work may be at risk", commitErr)
				} else {
					fmt.Printf("%s Auto-committed uncommitted work (safety net)\n", style.Bold.Render("✓"))
					fmt.Printf("  The agent should have committed before running gt done.\n")
					fmt.Printf("  This auto-save prevents work loss.\n\n")
					doneCleanupStatus = "unpushed" // Update status — changes are now committed but not pushed
				}
			}
		}
	}

	// Parse branch info
	info := parseBranchName(branch)

	// Override with explicit flags
	issueID := doneIssue
	if issueID == "" {
		issueID = info.Issue
	}

	// The MR's worker must be whoever is actually running `gt done` right
	// now (polecatName, validated above against BD_ACTOR/GT_POLECAT and the
	// worktree path), never the name parsed out of the branch. A --branch
	// rework reuses the ORIGINAL polecat's branch name under a different
	// worker, so trusting info.Worker here misattributes the MR and later
	// misroutes FIX_NEEDED to a polecat that no longer holds the issue
	// (gt-fl0n).
	worker := polecatName

	// Get agent bead ID for cross-referencing.
	ctx, actor := resolveDoneAgentIdentity(cwd, townRoot, rigName, polecatName)
	if actor != "" {
		sender = actor
	}
	agentBeadID := getAgentBeadID(ctx)

	// Recreate the agent bead if it's missing (hq-xu4p). Done-intent
	// labels, checkpoints, and active_mr all write to it; when it's gone
	// every write fails 'issue not found' and witness zombie detection +
	// done-resume silently degrade. Best-effort: a failed recreate just
	// leaves the existing warnings.
	//
	// Completion now exits the live polecat session after durable handoff.
	// The agent bead keeps lifecycle metadata for witness/refinery cleanup.
	ensureAgentBeadExists(beads.New(cwd).ForAgentBead(), agentBeadID, ctx)
	var assignedIssueIDs []string
	loadAssignedIssueIDs := func() []string {
		if assignedIssueIDs == nil && sender != "" {
			assignedIssueIDs = findAssignedBeadsForAgent(cwd, sender)
		}
		return assignedIssueIDs
	}

	// If issue ID not set by flag or branch name, query for hooked beads
	// assigned to this agent. This replaces reading agent_bead.hook_bead
	// (hq-l6mm5: direct bead tracking instead of agent bead slot).
	if issueID == "" && sender != "" {
		if hookIssue, ambiguous := selectAssignedIssue("", loadAssignedIssueIDs()); hookIssue != "" {
			issueID = hookIssue
		} else if ambiguous {
			return fmt.Errorf("multiple active assignments found for %s; cannot infer issue from hook. Use --issue to disambiguate", sender)
		}
	}

	// Stale-branch guard (hq-l0fj): a redispatched polecat that reuses its
	// previous work branch carries the OLD bead-id in the branch name, which
	// would mis-attribute this MR (close credit goes to a closed bead; the
	// real issue stays open and hooked). When the branch-derived id differs
	// from the hooked bead, trust the hook. An explicit --issue flag still
	// wins, and subtask branches of the hooked bead (e.g. gt-abc.1 under
	// hooked gt-abc) are left alone.
	if doneIssue == "" && info.Issue != "" && sender != "" {
		if hookIssue, ambiguous := selectAssignedIssue(info.Issue, loadAssignedIssueIDs()); isStaleBranchIssue(info.Issue, hookIssue) {
			style.PrintWarning("branch %q embeds issue %s but your hooked bead is %s — submitting for %s (stale branch reuse?)", branch, info.Issue, hookIssue, hookIssue)
			fmt.Printf("  Fresh branches must be named polecat/<name>/<bead-id>+<suffix> for the bead you are working.\n")
			fmt.Printf("  Use --issue to override if the branch-derived id is actually correct.\n\n")
			issueID = hookIssue
		} else if ambiguous {
			return fmt.Errorf("branch %q embeds issue %s but %s has multiple active assignments; use --issue to disambiguate", branch, info.Issue, sender)
		}
	}

	// Write done-intent label EARLY, before push/MR operations.
	// If gt done crashes after this point, the Witness can detect the intent
	// and auto-nuke the zombie polecat.
	//
	// Also read existing checkpoints for resume capability (gt-aufru).
	// If gt done was interrupted (SIGTERM, context exhaustion, SIGKILL),
	// checkpoints indicate which stages completed. On re-invocation, we
	// skip those stages to avoid repeating work or hitting errors.
	checkpoints := map[DoneCheckpoint]string{}
	if agentBeadID != "" {
		// ForAgentBead: dual-scope agent-bead resolution (rig-local first,
		// legacy town fallback — gt-8we).
		bd := beads.New(cwd).ForAgentBead()
		setDoneIntentLabel(bd, agentBeadID, exitType)
		checkpoints = readDoneCheckpoints(bd, agentBeadID)
		if len(checkpoints) > 0 {
			fmt.Printf("%s Resuming gt done from checkpoint (previous run was interrupted)\n", style.Bold.Render("→"))
		}
	}

	// Write heartbeat state="exiting" (gt-3vr5: heartbeat v2).
	// Tells the witness we're in the gt done flow — trust the agent until
	// heartbeat goes stale. No timer-based inference needed.
	// Parallel to done-intent label for backwards compat during migration.
	heartbeatSession := os.Getenv("GT_SESSION")
	if heartbeatSession != "" && townRoot != "" {
		polecat.TouchSessionHeartbeatWithState(townRoot, heartbeatSession, polecat.HeartbeatExiting, "gt done", issueID)
	}

	// Get configured default branch for this rig
	defaultBranch := "main" // fallback
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	baseRef := g.CleanBaseRef("origin", defaultBranch, doneTarget)

	// For COMPLETED, we need an issue ID and branch must not be the default branch
	var mrID string
	var pushFailed bool
	var mrFailed bool
	var doneErrors []string
	// pushVerifyErr records a failed "origin is at the commit this MR would
	// declare" assertion (gt-2wqt). It is returned as gt done's exit status so
	// the caller cannot mistake a dropped submission for a successful one.
	var pushVerifyErr error
	var convoyInfo *ConvoyInfo // Populated if issue is tracked by a convoy
	var sourceIssueForNoMerge *beads.Issue
	var sourceBD *beads.Beads
	if exitType == ExitCompleted {
		if branch == defaultBranch || branch == "master" {
			// A conflict-resolution pass ends on the base branch by design: it
			// submits no branch of its own, because its work is a rewritten head
			// already pushed to the branch of an existing MR
			// (mol-polecat-conflict-resolve, cleanup-and-exit). Rejecting it here
			// returns before notifyWitness, so the wake for the MR the pass just
			// released never fires (gt-rv8h, gt-tne1). DEFERRED is not this exit:
			// it files a finished pass as "stuck".
			if task := conflictResolutionCompletionTask(cwd, agentBeadID, issueID); task != nil {
				if !beads.IssueStatus(task.Status).IsTerminal() {
					// Completing now would walk away from a task the refinery is
					// still blocked on. Say so, rather than reporting a merge-queue
					// rejection the polecat cannot act on.
					return fmt.Errorf("cannot complete %s: conflict-resolution task %s is still open, and its MR stays blocked until it closes\nClose it first: bd close %s --reason=\"resolved conflicts\"",
						defaultBranch, task.ID, task.ID)
				}
				fmt.Printf("%s Conflict-resolution completion on %s — no branch of its own to submit\n", style.Bold.Render("→"), defaultBranch)
				fmt.Printf("  %s is closed; the refinery wake below names the MR it released.\n", task.ID)
				goto notifyWitness
			}
			return fmt.Errorf("cannot submit %s/master branch to merge queue", defaultBranch)
		}

		// CRITICAL: Verify work exists before completing (hq-xthqf)
		// Polecats calling gt done without commits results in lost work.
		// We MUST check for:
		// 1. Working directory availability (can't verify git state without it)
		// 2. Uncommitted changes (work that would be lost)
		// 3. Unique commits compared to origin (ensures branch was pushed with actual work)

		// Block if there are uncommitted changes (would be lost on completion).
		// Runtime artifacts (.claude/, .opencode/, .beads/, .runtime/, __pycache__/) are
		// excluded — these are toolchain-managed and normally gitignored.
		// Without this filter, gt done fails on virtually every polecat because
		// Cursor creates .claude/ at runtime in every workspace.
		workStatus, err := g.CheckUncommittedWork()
		if err != nil {
			return fmt.Errorf("checking git status: %w", err)
		}
		if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
			return fmt.Errorf("cannot complete: uncommitted changes would be lost\nCommit your changes first, or use --status DEFERRED to exit without completing\nUncommitted: %s", workStatus.String())
		}

		// Check if branch has commits ahead of the clean target base. In fork-backed
		// rigs this is upstream/main, not the fork's origin/main.
		aheadCount, err := g.CommitsAhead(baseRef, "HEAD")
		if err != nil {
			// Fallback to local branch comparison if origin not available
			aheadCount, err = g.CommitsAhead(defaultBranch, branch)
			if err != nil {
				// Can't determine - assume work exists and continue
				style.PrintWarning("could not check commits ahead of %s: %v", defaultBranch, err)
				aheadCount = 1
			}
		}

		// Check no_merge or review_only flags on the hooked bead. When set,
		// this is a non-code task (email, research, analysis, PRD review)
		// where zero commits is expected.
		// Must be checked before the zero-commit guard below (GH#2496, gt-kvf).
		isNoMergeTask := false
		reviewOnlySource := false
		if issueID != "" {
			sourceInfo, sourceErr := resolveSubmitSourceIssue(cwd, issueID)
			if sourceErr != nil {
				return fmt.Errorf("source issue validation failed: %w", sourceErr)
			}
			sourceIssueForNoMerge = sourceInfo.Issue
			sourceBD = sourceInfo.BD
			if af := beads.ParseAttachmentFields(sourceIssueForNoMerge); af != nil {
				if af.NoMerge || af.ReviewOnly {
					isNoMergeTask = true
				}
				reviewOnlySource = af.ReviewOnly
			}
		}

		// If no commits ahead, work was likely already merged or is a legitimate
		// report-only completion. Fork-backed rigs must not infer success from fork main.
		// For polecats, zero commits usually means the polecat sleepwalked through
		// implementation without writing code (gastown#1484, beads#emma).
		// The --cleanup-status=clean escape is preserved for legitimate report-only
		// tasks (audits, reviews) that the formula explicitly directs to use it.
		// no_merge/review_only tasks (GH#2496, gt-kvf) also bypass: non-code work has no commits by design.
		// IMPORTANT: The error message must NOT mention --cleanup-status=clean.
		// LLM agents read error messages and self-bypass (the original bug).
		if aheadCount == 0 {
			if os.Getenv("GT_POLECAT") != "" && doneCleanupStatus != "clean" && !isNoMergeTask {
				// Before failing, check whether commits exist on the remote feature branch.
				// After a polecat pushes to origin/<feature-branch> and submits an MR,
				// if master advances (e.g., other MRs land), the feature branch is no
				// longer ahead of origin/master — but the work WAS committed and pushed.
				// In that case, treat as "MR already submitted" and fall through. (GH#wd7)
				branchPushedWithWork := false
				if branch != defaultBranch {
					pushed, unpushed, pushErr := g.BranchPushedToRemote(branch, "origin")
					branchPushedWithWork = pushErr == nil && pushed && unpushed == 0
				}
				if !branchPushedWithWork {
					return fmt.Errorf("cannot complete: no commits on branch ahead of %s\n"+
						"Polecats must have at least 1 commit to submit.\n"+
						"If the bug was already fixed upstream: gt done --status DEFERRED\n"+
						"If you're blocked: gt done --status ESCALATED",
						baseRef)
				}
			}

			// Non-polecat (crew/mayor), polecat with --cleanup-status=clean
			// (report-only tasks like audits/reviews), or no_merge polecat
			// (non-code tasks like email/research per GH#2496):
			// zero commits is valid.
			fmt.Printf("%s Branch has no commits ahead of %s\n", style.Bold.Render("→"), baseRef)
			fmt.Printf("  Work was likely already merged or report-only.\n")
			fmt.Printf("  Skipping MR creation - completing without merge request.\n\n")

			// G15 fix: Close the base issue when completing with no MR.
			// Without this, no-op polecats (bug already fixed) leave issues stuck
			// in HOOKED state with assignee pointing to the nuked polecat.
			// Normally the Refinery closes after merge, but with no MR, nothing
			// would ever close the issue.
			if issueID != "" {
				bd := sourceBD
				if bd == nil {
					bd = beads.New(cwd)
				}

				skipClose := false
				if skipReason, fatal := doneSourceCloseSkipReason(bd, issueID, sourceIssueForNoMerge); skipReason != "" {
					style.PrintWarning("%s", skipReason)
					fmt.Printf("  The bead will remain open for witness/mayor review.\n")
					notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
					if fatal {
						return fmt.Errorf("cannot complete review-only/no-MR work: %s", skipReason)
					}
					skipClose = true
				}

				if !skipClose {
					closeReason := "Completed with no code changes (already fixed or already merged)"
					noMRCommitSHA, _ := g.Rev("HEAD")
					if doneSkipVerify {
						noteVerifiedPushSkipped(bd, cwd, issueID, defaultBranch, noMRCommitSHA, "--skip-verify on no-MR close")
						if noMRCommitSHA != "" {
							closeReason = fmt.Sprintf("%s\nskip_verify: true\ntarget_branch: %s\ncommit_sha: %s", closeReason, defaultBranch, noMRCommitSHA)
						}
					} else if !isNoMergeTask {
						if g.ForkBackedRemote("origin") {
							return fmt.Errorf("cannot close no-MR code bead in fork/upstream mode: %s has no commits ahead of %s; use the fork PR flow instead", branch, baseRef)
						}
						if verifyErr := g.VerifyPushedCommitReachableFromPushTarget("origin", defaultBranch, noMRCommitSHA); verifyErr != nil {
							noteVerifiedPushFailure(bd, cwd, issueID, defaultBranch, noMRCommitSHA, verifyErr)
							return fmt.Errorf("cannot close no-MR code bead: %w", verifyErr)
						}
						if noMRCommitSHA != "" {
							closeReason = fmt.Sprintf("%s\ntarget_branch: %s\ncommit_sha: %s", closeReason, defaultBranch, noMRCommitSHA)
						}
					}
					// G15 fix: Force-close bypasses molecule dependency checks.
					// The polecat is about to be nuked — open wisps should not block closure.
					// Retry with backoff handles transient dolt lock contention (A2).
					var closeErr error
					for attempt := 1; attempt <= 3; attempt++ {
						closeErr = bd.ForceCloseWithReason(closeReason, issueID)
						if closeErr == nil {
							fmt.Printf("%s Issue %s closed (no MR needed)\n", style.Bold.Render("✓"), issueID)
							break
						}
						if attempt < 3 {
							style.PrintWarning("close attempt %d/3 failed: %v (retrying in %ds)", attempt, closeErr, attempt*2)
							time.Sleep(time.Duration(attempt*2) * time.Second)
						}
					}
					if closeErr != nil {
						style.PrintWarning("could not close issue %s after 3 attempts: %v (issue may be left HOOKED)", issueID, closeErr)
					}
				}
			}

			// Skip straight to witness notification (no MR needed)
			goto notifyWitness
		}

		if reviewOnlySource {
			return fmt.Errorf("cannot complete review-only issue %s with commits ahead of %s; add a fresh review evidence comment and complete without code changes", issueID, baseRef)
		}

		// Branch contamination preflight: check if branch is significantly behind
		// the effective target branch, which indicates the branch may contain stale merge-base
		// artifacts that will pollute the PR diff. (GH#2220)
		//
		// gh#3400: Refresh remote tracking refs first so contamination check (and
		// the auto-rebase below) sees the current clean base. In fork-backed rigs,
		// that base is upstream/main, not the fork's origin/main.
		contaminationBase := baseRef
		if doneTarget != "" && doneTarget != defaultBranch {
			contaminationBase = doneContaminationBaseRef(defaultBranch, doneTarget)
		}
		fetchRemote := git.RemoteForRef(contaminationBase)
		if fetchRemote == "" {
			fetchRemote = "origin"
		}
		if fetchErr := g.Fetch(fetchRemote); fetchErr != nil {
			style.PrintWarning("could not fetch %s before contamination check: %v (proceeding with local refs)", fetchRemote, fetchErr)
		}

		// Does this branch already exist where this run will push it? Every
		// branch push on this path targets origin (the refspec built further
		// down is branch:branch against it), so ask about origin/<branch> —
		// not whichever remote the fetch above followed. In a fork-backed rig
		// that fetch is upstream's, which leaves origin/<branch> stale enough
		// to miss a push from an earlier dispatch and mistake a reused branch
		// for a new one (gt-i0z3). Drives both the auto-rebase gate below and
		// the commit-message squash gate further down, which must agree.
		_, pushedReason := branchAlreadyOnRemote(g, "origin", branch, fetchRemote != "origin")
		if pushedCheckpointBranch(checkpoints[CheckpointPushed]) == branch {
			pushedReason = "prior push checkpoint exists"
		}

		contam, err := g.CheckBranchContamination(contaminationBase)
		if err == nil && contam.Behind > 0 {
			const warnThreshold = 50
			const blockThreshold = 200
			if contam.Behind >= blockThreshold {
				return fmt.Errorf("branch contamination: %d commits behind %s (threshold: %d)\n"+
					"The branch is severely stale and will include unrelated changes in the PR.\n"+
					"Fix: git fetch %s && git rebase %s",
					contam.Behind, contaminationBase, blockThreshold, fetchRemote, contaminationBase)
			} else if contam.Behind >= warnThreshold {
				style.PrintWarning("branch is %d commits behind %s — consider rebasing to avoid PR contamination", contam.Behind, contaminationBase)
			}

			// gh#3400: Auto-rebase the polecat branch onto the latest target before
			// push, so the resulting MR/PR has a current base.
			//
			// gt-bf5x: checkpoints only remember THIS session's own push. A
			// branch reused across a redispatch (formula's rejected-MR rework
			// exception) can already be on origin from a *previous* session,
			// which the checkpoint has no record of — rebasing it here would
			// diverge local history from origin for no reason (the formula
			// already rebases such branches itself in step 2). Treat an
			// existing origin/<branch> the same way the commit-message squash
			// step below already does: as proof this branch was pushed before.
			// pushedReason names which of the two proved it (gt-i0z3).
			rebased, skipReason, rebaseErr := autoRebaseOnTarget(g, contaminationBase, contam.Behind, donePreVerified, pushedReason)
			if rebaseErr != nil {
				return rebaseErr
			}
			if rebased {
				fmt.Printf("%s Branch rebased onto %s\n", style.Bold.Render("✓"), contaminationBase)
				// Recompute commits ahead since rebase rewrote history.
				aheadCount, _ = g.CommitsAhead(baseRef, "HEAD")
			} else if skipReason != "" {
				style.PrintWarning("branch is %d commits behind %s but %s; skipping auto-rebase", contam.Behind, contaminationBase, skipReason)
			}
		}

		// Refuse to submit a branch that reverts work already merged to the
		// target (gt-63sz). Anything else in this path — the contamination
		// check above, the MR gate, the refinery — reads the commit graph, and
		// a `git reset --soft origin/main` over a stale checkout produces a
		// branch that is one commit ahead of a fresh base while its content
		// undoes every commit merged in between. Only the branch's file content
		// shows that, so this is the one place that looks at it.
		//
		// Runs after the auto-rebase above: a rebase replays the same diff, so
		// it neither causes nor cures this and the check must see the branch in
		// the state that would actually be pushed.
		if doneAllowReverts {
			style.PrintWarning("skipping merged-work revert check (--allow-reverts): the branch may undo work merged to %s", contaminationBase)
		} else if err := reportRevertedMerges(g, contaminationBase); err != nil {
			return err
		}

		// Refuse a branch that would add throwaway files to the target
		// (gt-ozo4). Checked here, before the commit-message squash below
		// rewrites history, so a refusal leaves the branch exactly as the
		// polecat left it.
		if doneAllowThrowawayPaths {
			style.PrintWarning("skipping throwaway-file check (--allow-throwaway-paths): the branch may add scratch files to %s", contaminationBase)
		} else if err := reportThrowawayPaths(g, contaminationBase); err != nil {
			return err
		}

		// Refuse a rework whose content is byte-identical to an attempt the
		// refinery already rejected (gt-0jzd5). Runs after the rebase above,
		// for the same reason the revert check does: patch-id is
		// base-invariant, so what matters is the content about to be pushed.
		var sourceNotes string
		if sourceIssueForNoMerge != nil {
			sourceNotes = sourceIssueForNoMerge.Notes
		}
		rejectedTip := func(mrID string) (string, bool) { return rejectedTipFromMR(sourceBD, mrID) }
		if err := reportUnchangedSinceRejection(g, sourceNotes, issueID, contaminationBase, rejectedTip); err != nil {
			return err
		}

		// Rewrite machine-generated commit messages before submission (gt-3wf).
		// The gt-pvx safety net and checkpoint dog commit real work under
		// generic subjects ("fix: auto-save uncommitted implementation work",
		// "WIP: checkpoint (auto)"). Polecats are told to amend before gt done
		// but observably never do, so main's merge history fills with
		// meaningless messages. Squash the branch into one commit instead:
		// non-generated subjects are preserved (first as title, rest in the
		// body); when every commit is machine-generated, the source issue
		// title becomes the subject. Skipped when the branch was already
		// pushed (resume checkpoint, or the agent pushed manually — polecat
		// branch names are session-unique, so an existing origin/<branch>
		// means this session pushed it, and gt-i0z3 made the ref read behind
		// pushedReason refresh first) — rewriting history then would break the
		// later non-force push.
		//
		// The skip is safe only while the tip is real work: a machine-generated
		// tip passes through it unseen and is landed as-is, which is how a
		// checkpoint commit became main's tip (gt-iki6, f0a00f6). That shape
		// cannot be squashed here — origin already has the commit — so refuse
		// it and hand the polecat the rewrite instead.
		tip, tipErr := checkpoint.InspectAutoSaveTip(cwd, baseRef, "HEAD")
		if tipErr != nil {
			// An unreadable tip must not fail the gate open on a branch origin
			// already has: that is the shape being refused, unverified.
			if pushedReason != "" {
				return autoSaveTipUninspectableError(branch, pushedReason, tipErr)
			}
			style.PrintWarning("could not inspect the branch tip for auto-save commits: %v", tipErr)
		} else if err := autoSaveTipGate(tip, branch, pushedReason); err != nil {
			return err
		}
		if pushedReason == "" {
			headBefore, headBeforeErr := g.Rev("HEAD")
			if squashed, squashErr := checkpoint.SquashAutoSaveCommits(cwd, baseRef, autoSaveSquashTitle(sourceIssueForNoMerge, issueID)); squashErr != nil {
				// A squash that failed after its soft reset leaves the branch
				// holding no commits at all; refuse before anything pushes it.
				if headAfter, revErr := g.Rev("HEAD"); headBeforeErr == nil && revErr == nil && headAfter != headBefore {
					return autoSaveSquashResetError(branch, baseRef, squashErr)
				}
				if tip.AutoSave {
					return autoSaveTipRefusalError(tip, branch,
						fmt.Sprintf("the squash failed: %v.", squashErr))
				}
				style.PrintWarning("could not rewrite auto-save commit messages: %v (submitting as-is)", squashErr)
			} else if squashed > 0 {
				fmt.Printf("%s Squashed %d auto-save/WIP commit(s) into a single descriptive commit\n", style.Bold.Render("✓"), squashed)
				aheadCount, _ = g.CommitsAhead(baseRef, "HEAD")
			}
		}

		// Strip Gas Town overlay from CLAUDE.md / CLAUDE.local.md (gt-p35).
		// Polecats commit the overlay (polecat lifecycle boilerplate) into repos,
		// overwriting project-specific CLAUDE.md content. Detect and revert before push.
		if stripped := stripOverlayCLAUDEmd(g, defaultBranch, baseRef); stripped {
			// Recalculate commits ahead since we added a cleanup commit
			aheadCount, _ = g.CommitsAhead(baseRef, "HEAD")
		}

		// Determine merge strategy from convoy (gt-myofa.3)
		// Convoys can override the default MR-based workflow:
		//   direct: push commits straight to target branch, bypass refinery
		//   mr:     default — create merge-request bead, refinery merges
		//   local:  keep on feature branch, no push, no MR (for human review/upstream PRs)
		//
		// Primary: read convoy info from the issue's attachment fields (gt-7b6wf fix).
		// gt sling stores convoy_id and merge_strategy on the issue when dispatching,
		// which avoids unreliable cross-rig dep resolution at gt done time.
		// Fallback: dep-based lookup via getConvoyInfoForIssue (for issues dispatched
		// before this fix, or where attachment fields weren't set).
		convoyInfo = getConvoyInfoFromSourceIssue(sourceIssueForNoMerge)
		if convoyInfo == nil {
			convoyInfo = getConvoyInfoForIssue(issueID)
		}

		// Handle "local" strategy: skip push and MR entirely
		if convoyInfo != nil && convoyInfo.MergeStrategy == "local" {
			fmt.Printf("%s Local merge strategy: skipping push and merge queue\n", style.Bold.Render("→"))
			fmt.Printf("  Branch: %s\n", branch)
			if issueID != "" {
				fmt.Printf("  Issue: %s\n", issueID)
			}
			fmt.Println()
			fmt.Printf("%s\n", style.Dim.Render("Work stays on local feature branch."))
			goto notifyWitness
		}

		// Handle "direct" strategy: push to target branch, skip MR
		if convoyInfo != nil && convoyInfo.MergeStrategy == "direct" {
			fmt.Printf("%s Direct merge strategy: pushing to %s\n", style.Bold.Render("→"), defaultBranch)
			directBd := sourceBD
			if directBd == nil {
				directBd = beads.New(cwd)
			}
			if skipReason := doneDirectMergeSkipReason(directBd, issueID, sourceIssueForNoMerge, defaultBranch); skipReason != "" {
				style.PrintWarning("%s", skipReason)
				notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
				return fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
			}
			// Push submodule changes before direct push (gt-dzs)
			pushSubmoduleChanges(g, baseRef)
			directRefspec := branch + ":" + defaultBranch
			// A direct-merge convoy is the one sanctioned way a polecat's
			// session puts work on the default branch, so name it for the
			// pre-push hook (gt-ibt8) — the strategy and the source issue were
			// already vetted by doneDirectMergeSkipReason above.
			directPushErr := g.PushWithEnv("origin", directRefspec, false, []string{git.EnvDoneDirectMerge})
			if directPushErr != nil {
				pushFailed = true
				errMsg := fmt.Sprintf("direct push to %s failed: %v", defaultBranch, directPushErr)
				doneErrors = append(doneErrors, errMsg)
				style.PrintWarning("%s", errMsg)
				goto notifyWitness
			}
			directCommitSHA, _ := g.Rev("HEAD")
			if doneSkipVerify {
				noteVerifiedPushSkipped(directBd, cwd, issueID, defaultBranch, directCommitSHA, "--skip-verify on direct merge")
			} else if verifyErr := g.VerifyPushedCommitReachableFromPushTarget("origin", defaultBranch, directCommitSHA); verifyErr != nil {
				pushFailed = true
				errMsg := verifyErr.Error()
				doneErrors = append(doneErrors, errMsg)
				noteVerifiedPushFailure(directBd, cwd, issueID, defaultBranch, directCommitSHA, verifyErr)
				style.PrintWarning("%s\nDirect merge pushed but remote verification failed. Source bead will remain in progress.", errMsg)
				goto notifyWitness
			}
			fmt.Printf("%s Branch pushed directly to %s\n", style.Bold.Render("✓"), defaultBranch)
			doneCleanupStatus = cleanupStatusAfterSuccessfulPush(doneCleanupStatus)

			// Close the base issue — no MR/refinery will close it
			if issueID != "" {
				if skipReason, fatal := doneSourceCloseSkipReason(directBd, issueID, sourceIssueForNoMerge); skipReason != "" {
					style.PrintWarning("%s", skipReason)
					notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
					if fatal {
						return fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
					}
				} else {
					closeReason := fmt.Sprintf("Direct merge to %s (convoy strategy)", defaultBranch)
					var closeErr error
					for attempt := 1; attempt <= 3; attempt++ {
						closeErr = directBd.ForceCloseWithReason(closeReason, issueID)
						if closeErr == nil {
							fmt.Printf("%s Issue %s closed (direct merge)\n", style.Bold.Render("✓"), issueID)
							break
						}
						if attempt < 3 {
							style.PrintWarning("close attempt %d/3 failed: %v (retrying in %ds)", attempt, closeErr, attempt*2)
							time.Sleep(time.Duration(attempt*2) * time.Second)
						}
					}
					if closeErr != nil {
						style.PrintWarning("could not close issue %s after 3 attempts: %v", issueID, closeErr)
					}
				}
			}

			goto notifyWitness
		}

		// Default: "mr" strategy (or no convoy) — push branch, create MR bead

		if issueID == "" {
			return fmt.Errorf("cannot determine source issue from branch '%s'; use --issue to specify", branch)
		}

		// Initialize beads and validate the source before any remote mutation.
		// Without a redirect, MR beads are invisible to the Refinery.
		resolvedBeads := beads.ResolveBeadsDir(cwd)
		if beads.IsLocalBeadsDir(cwd, resolvedBeads) {
			fmt.Fprintf(os.Stderr, "WARNING: beads resolved to local dir %s (no shared-beads redirect)\n", resolvedBeads)
			fmt.Fprintf(os.Stderr, "  MR beads written here will be invisible to the Refinery — run 'gt polecat repair' to fix\n")
		}
		bd := beads.NewWithBeadsDir(cwd, resolvedBeads)
		if attachmentFields := beads.ParseAttachmentFields(sourceIssueForNoMerge); attachmentFields != nil && strings.EqualFold(strings.TrimSpace(attachmentFields.MergeStrategy), "local") {
			fmt.Printf("%s Local merge strategy: skipping push and merge queue\n", style.Bold.Render("→"))
			fmt.Printf("  Branch: %s\n", branch)
			fmt.Printf("  Issue: %s\n", issueID)
			fmt.Println()
			fmt.Printf("%s\n", style.Dim.Render("Work stays on local feature branch."))
			goto notifyWitness
		}

		// Fallback: check if issue belongs to a direct-merge convoy that the
		// primary check missed — e.g., issues dispatched before the attachment-field
		// fix, or where dep-based lookup failed at that point. This must happen
		// before the generic branch/submodule push because direct mode has no MR or
		// refinery recheck.
		convoyInfo = getConvoyInfoFromSourceIssue(sourceIssueForNoMerge)
		if convoyInfo == nil {
			convoyInfo = getConvoyInfoForIssue(issueID)
		}
		if convoyInfo != nil && convoyInfo.MergeStrategy == "direct" {
			fmt.Printf("%s Late-detected direct merge strategy: pushing to %s\n", style.Bold.Render("→"), defaultBranch)
			fmt.Printf("  Convoy: %s\n", convoyInfo.ID)
			directBd := sourceBD
			if directBd == nil {
				directBd = bd
			}
			if skipReason := doneDirectMergeSkipReason(directBd, issueID, sourceIssueForNoMerge, defaultBranch); skipReason != "" {
				style.PrintWarning("%s", skipReason)
				notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
				return fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
			}

			pushSubmoduleChanges(g, baseRef)
			directRefspec := branch + ":" + defaultBranch
			// Late-detected direct merge: same sanctioned landing as the
			// primary check above, so it names the same signal (gt-ibt8).
			directPushErr := g.PushWithEnv("origin", directRefspec, false, []string{git.EnvDoneDirectMerge})
			if directPushErr != nil {
				pushFailed = true
				errMsg := fmt.Sprintf("direct push to %s failed: %v", defaultBranch, directPushErr)
				doneErrors = append(doneErrors, errMsg)
				style.PrintWarning("%s", errMsg)
				goto notifyWitness
			}
			directCommitSHA, _ := g.Rev("HEAD")
			if doneSkipVerify {
				noteVerifiedPushSkipped(directBd, cwd, issueID, defaultBranch, directCommitSHA, "--skip-verify on late direct merge")
			} else if verifyErr := g.VerifyPushedCommitReachableFromPushTarget("origin", defaultBranch, directCommitSHA); verifyErr != nil {
				pushFailed = true
				errMsg := verifyErr.Error()
				doneErrors = append(doneErrors, errMsg)
				noteVerifiedPushFailure(directBd, cwd, issueID, defaultBranch, directCommitSHA, verifyErr)
				style.PrintWarning("%s\nLate direct merge pushed but remote verification failed. Source bead will remain in progress.", errMsg)
				goto notifyWitness
			}
			fmt.Printf("%s Branch pushed directly to %s\n", style.Bold.Render("✓"), defaultBranch)
			doneCleanupStatus = cleanupStatusAfterSuccessfulPush(doneCleanupStatus)

			if skipReason, fatal := doneSourceCloseSkipReason(directBd, issueID, sourceIssueForNoMerge); skipReason != "" {
				style.PrintWarning("%s", skipReason)
				notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
				if fatal {
					return fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
				}
			} else {
				var closeErr error
				for attempt := 1; attempt <= 3; attempt++ {
					closeErr = directBd.ForceCloseWithReason(
						fmt.Sprintf("Direct merge to %s (convoy strategy, late detection)", defaultBranch), issueID)
					if closeErr == nil {
						fmt.Printf("%s Issue %s closed (direct merge)\n", style.Bold.Render("✓"), issueID)
						break
					}
					if attempt < 3 {
						style.PrintWarning("close attempt %d/3 failed: %v (retrying in %ds)", attempt, closeErr, attempt*2)
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if closeErr != nil {
					style.PrintWarning("could not close issue %s after 3 attempts: %v", issueID, closeErr)
				}
			}

			goto notifyWitness
		}

		// Pre-declare push variables for checkpoint goto (gt-aufru)
		var refspec string
		var pushErr error
		var pushedCommitSHA string
		// Declared here rather than at its assignment below because the
		// checkpoint resume jumps over that point into afterPush, and a goto may
		// not carry a variable into scope (gt-0opm).
		var pushFailureDetail string

		// Use explicit refspec (branch:branch) to create the remote branch.
		// Without refspec, git push follows the tracking config — polecat branches
		// track origin/main, so a bare push sends commits to main directly,
		// bypassing the MR/refinery flow (G20 root cause).
		//
		// Built before the checkpoint resume because the landing retry at
		// afterPush re-sends this refspec, and the MR declares this commit, so
		// the retry must not be a different push (gt-0opm).
		refspec = branch + ":" + branch

		// Resume: skip push if already completed in a previous run (gt-aufru).
		// Validate checkpoint branch matches current branch (ge-sbo: stale checkpoint
		// on polecat reassignment causes new work to skip push for old branch)
		// AND that the branch has not moved since that push (gt-2wqt: a
		// branch-only key let a retry after new commits skip the push, so the
		// MR declared a commit origin never received — the
		// "gate fails → fix commit → re-run gt done" cycle reproduced it every
		// time). A matching checkpoint records a push of this exact commit that
		// gt-2wqt's assertion already proved landed; classifyResumedPush reads
		// what that means for this run.
		checkpointHead, checkpointHeadErr := g.Rev("HEAD")
		if checkpointHeadErr != nil {
			return fmt.Errorf("resolving HEAD for push checkpoint: %w", checkpointHeadErr)
		}
		if checkpoints[CheckpointPushed] != "" {
			// Only the --target override is resolved this early; a run whose MR
			// targets another source of it (a formula_vars base_branch) misses
			// the landed classification, which costs it only that outcome.
			landedTarget := defaultBranch
			if doneTarget != "" {
				landedTarget = doneTarget
			}
			switch classifyResumedPush(g, "origin", landedTarget, checkpoints[CheckpointPushed], branch, checkpointHead) {
			case resumedWorkLanded:
				fmt.Printf("%s Branch %s already landed on origin/%s — nothing left to submit\n",
					style.Bold.Render("✓"), branch, landedTarget)
				goto notifyWitness
			case resumedWorkPushed:
				fmt.Printf("%s Branch already pushed (resumed from checkpoint)\n", style.Bold.Render("✓"))
				goto afterPush
			default:
				// Stale checkpoint — either a previous assignment's branch (ge-sbo) or
				// this branch at an earlier commit (gt-2wqt) — discard and push normally.
				fmt.Printf("→ Discarding stale push checkpoint (%s, now on %s@%s)\n",
					checkpoints[CheckpointPushed], branch, shortSHA(checkpointHead))
			}
		}

		// CRITICAL: Push branch BEFORE creating MR bead (hq-6dk53, hq-a4ksk)
		// The MR bead triggers Refinery to process this branch. If the branch
		// isn't pushed yet, Refinery finds nothing to merge. The worktree gets
		// nuked at the end of gt done, so the commits are lost forever.
		//
		// Auto-push submodule changes BEFORE parent push (gt-dzs).
		// If the parent repo's submodule pointer references commits that don't
		// exist on the submodule's remote, the Refinery MR will be broken.
		// Detect modified submodules and push each one first.
		pushSubmoduleChanges(g, baseRef)

		fmt.Printf("Pushing branch to remote...\n")
		pushedCommitSHA, _ = g.Rev("HEAD")
		pushErr = pushBranchToOrigin(g, townRoot, rigName, refspec)
		// pushFailureDetail carries the divergence diagnosis (and any recovery
		// error) into the terminal message at afterPush, where the landing retry
		// decides whether the failure is terminal at all.
		if pushErr != nil {
			// Both push attempts failed non-fast-forward. Before alarming as
			// possible work loss, check whether origin already has this branch
			// from an earlier dispatch/rebase carrying the same work (gt-bf5x)
			// — if so, it's safe to leased-force-push rather than raise a false
			// alarm.
			recovered, diagnosis, recoverErr := recoverDivergedPush(g, "origin", refspec, branch, baseRef)
			if recovered {
				fmt.Printf("%s Recovered non-fast-forward push: %s\n", style.Bold.Render("✓"), diagnosis)
				pushErr = nil
			} else {
				// gt-0opm: a failed push command is not yet an unlanded push, so
				// fall through to afterPush, which re-asserts origin and retries
				// before this becomes terminal.
				pushFailureDetail = diagnosis
				// Report the recovery failure independently of the diagnosis:
				// the two say different things (why recovery was refused vs.
				// why the attempt itself broke), and the fetch/comparison
				// errors that produce a diagnosis-less failure are exactly the
				// ones a reader needs to see (gt-i0z3).
				if recoverErr != nil {
					pushFailureDetail = fmt.Sprintf("%s [recovery attempt: %v]", pushFailureDetail, recoverErr)
				}
				style.PrintWarning("push failed for branch '%s': %v — re-checking origin before treating the work as unlanded", branch, pushErr)
			}
		}

	afterPush:

		// Verify the remote branch tip is the exact commit this run will declare
		// in the MR before anything downstream trusts the push (gt-2wqt).
		//
		// This runs on EVERY path into afterPush — including the checkpoint
		// resume above — because a recorded push is a fact about the past and
		// origin can move off it afterwards. The failure it closes: origin
		// holding an older tip than the commit the MR declares, so the refinery
		// gates and merges the old tree while the MR looks ready
		// (gt-wisp-i2vk amber/gt-uoqg, gt-wisp-qjy slate, gt-wisp-yle marble).
		// A branch-exists check is not enough, and neither is the push's exit
		// code: "Branch pushed" below is printed from this assertion alone.
		{
			if pushedCommitSHA == "" {
				pushedCommitSHA, _ = g.Rev("HEAD")
			}
			if doneSkipVerify {
				noteVerifiedPushSkipped(sourceBD, cwd, issueID, branch, pushedCommitSHA, "--skip-verify on branch push")
				fmt.Printf("%s Branch pushed to origin (verification skipped: --skip-verify)\n", style.Bold.Render("✓"))
			} else if recovered, verifyErr := landBranchPushBeforeMR(
				func() error { return pushBranchToOrigin(g, townRoot, rigName, refspec) },
				func() error { return verifyPushLandedBeforeMR(g, townRoot, rigName, branch, pushedCommitSHA) },
				time.Sleep,
			); verifyErr != nil {
				// gt-0opm: only reached once origin was re-asserted and the push
				// re-sent, so a first failing attempt never strands the work.
				pushFailed = true
				pushVerifyErr = verifyErr
				errMsg := unlandedPushMessage(branch, pushErr, pushFailureDetail, verifyErr)
				doneErrors = append(doneErrors, errMsg)
				noteVerifiedPushFailure(sourceBD, cwd, issueID, branch, pushedCommitSHA, verifyErr)
				if pushErr != nil {
					style.PrintWarning("%s\nCommits exist locally but failed to push. Witness will be notified.", errMsg)
				} else {
					style.PrintWarning("%s\nNo merge request created: it would declare a commit origin does not have. Witness will be notified.", errMsg)
				}
				goto notifyWitness
			} else {
				// The gt-0opm recovery: the push command failed and origin has
				// the commit anyway. Nothing below marks this run failed, so the
				// MR is created normally.
				if recovered {
					fmt.Printf("%s Branch pushed to origin (recovered: the first attempt reported an error, origin has commit %s)\n",
						style.Bold.Render("✓"), shortSHA(pushedCommitSHA))
				} else {
					fmt.Printf("%s Branch pushed to origin\n", style.Bold.Render("✓"))
				}
			}

			// Fix cleanup_status after successful push (gt-wcr).
			// Status was detected before push, so "unpushed" is now stale.
			doneCleanupStatus = cleanupStatusAfterSuccessfulPush(doneCleanupStatus)

			// Write push checkpoint for resume (gt-aufru), keyed on branch AND
			// commit (gt-2wqt) and written only once the remote tip is verified,
			// so a failed verification can never let a retry skip the push.
			if agentBeadID != "" && pushedCommitSHA != "" {
				// ForAgentBead: dual-scope agent-bead resolution (rig-local first,
				// legacy town fallback — gt-8we).
				cpBd := beads.New(cwd).ForAgentBead()
				writeDoneCheckpoint(cpBd, agentBeadID, CheckpointPushed, pushedCheckpointValue(branch, pushedCommitSHA))
			}
		}

		// Check for no_merge flag - if set, skip merge queue and notify for review
		{
			attachmentFields := beads.ParseAttachmentFields(sourceIssueForNoMerge)
			if attachmentFields != nil && attachmentFields.NoMerge {
				fmt.Printf("%s No-merge mode: skipping merge queue\n", style.Bold.Render("→"))
				fmt.Printf("  Branch: %s\n", branch)
				fmt.Printf("  Issue: %s\n", issueID)
				fmt.Println()

				// When merge_strategy=pr, create a GitHub PR for human review
				// instead of just leaving the branch on origin (gas-rfi).
				var prURL string
				if noMergeMQ := rig.ResolveMergeQueueConfig(townRoot, rigName); noMergeMQ != nil && noMergeMQ.MergeStrategy == "pr" {
					issueTitle := sourceIssueForNoMerge.Title
					prTitle := fmt.Sprintf("%s (%s)", issueTitle, issueID)
					if issueTitle == "" {
						prTitle = issueID
					}
					// Build PR body from bead description + diff stat
					var prBodyBuilder strings.Builder
					prBodyBuilder.WriteString("## Summary\n\n")
					if sourceIssueForNoMerge.Description != "" {
						// Strip attachment metadata lines from description
						descLines := strings.Split(sourceIssueForNoMerge.Description, "\n")
						var cleanDesc []string
						for _, line := range descLines {
							trimmed := strings.TrimSpace(line)
							if strings.HasPrefix(trimmed, "attached_") || strings.HasPrefix(trimmed, "dispatched_by:") || strings.HasPrefix(trimmed, "formula_vars:") {
								continue
							}
							cleanDesc = append(cleanDesc, line)
						}
						desc := strings.TrimSpace(strings.Join(cleanDesc, "\n"))
						if desc != "" {
							prBodyBuilder.WriteString(desc)
							prBodyBuilder.WriteString("\n\n")
						}
					}
					// Add diff stat for quick review context
					if diffStat, diffErr := g.DiffStat(baseRef + "..." + branch); diffErr == nil && diffStat != "" {
						prBodyBuilder.WriteString("## Changes\n\n```\n")
						prBodyBuilder.WriteString(diffStat)
						prBodyBuilder.WriteString("```\n\n")
					}
					prBodyBuilder.WriteString("---\n")
					prBodyBuilder.WriteString(fmt.Sprintf("*Polecat: %s | Issue: %s*\n", worker, issueID))
					prBody := prBodyBuilder.String()
					ghCmd := exec.CommandContext(context.Background(), "gh", "pr", "create",
						"--base", defaultBranch,
						"--head", branch,
						"--title", prTitle,
						"--body", prBody,
					)
					ghCmd.Dir = cwd
					prOutput, prErr := ghCmd.Output()
					if prErr != nil {
						style.PrintWarning("could not create GitHub PR: %v", prErr)
					} else {
						prURL = strings.TrimSpace(string(prOutput))
						fmt.Printf("%s GitHub PR created: %s\n", style.Bold.Render("✓"), prURL)
					}
				} else {
					fmt.Printf("%s\n", style.Dim.Render("Work stays on feature branch for human review."))
				}

				// Mail dispatcher with READY_FOR_REVIEW
				if dispatcher := attachmentFields.DispatchedBy; dispatcher != "" {
					townRouter := mail.NewRouter(townRoot)
					defer townRouter.WaitPendingNotifications()
					reviewBody := fmt.Sprintf("Branch: %s\nIssue: %s\nReady for review.", branch, issueID)
					if prURL != "" {
						reviewBody = fmt.Sprintf("Branch: %s\nIssue: %s\nPR: %s\nReady for review.", branch, issueID, prURL)
					}
					reviewMsg := &mail.Message{
						To:      dispatcher,
						From:    detectSender(),
						Subject: fmt.Sprintf("READY_FOR_REVIEW: %s", issueID),
						Body:    reviewBody,
					}
					if err := townRouter.Send(reviewMsg); err != nil {
						style.PrintWarning("could not notify dispatcher: %v", err)
					} else {
						fmt.Printf("%s Dispatcher notified: READY_FOR_REVIEW\n", style.Bold.Render("✓"))
					}
				}

				// No-merge work never goes through the refinery, so close the source bead
				// here after notifying the dispatcher. Otherwise hooked work remains open.
				if issueID != "" {
					noMergeBd := sourceBD
					if noMergeBd == nil {
						noMergeBd = bd
					}
					canCloseIssue := true
					if skipReason, fatal := doneSourceCloseSkipReason(noMergeBd, issueID, sourceIssueForNoMerge); skipReason != "" {
						style.PrintWarning("%s", skipReason)
						notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
						if fatal {
							return fmt.Errorf("cannot complete review-only/no-merge work: %s", skipReason)
						}
						canCloseIssue = false
					}
					if canCloseIssue && attachmentFields.AttachedMolecule != "" {
						if n := closeDescendants(noMergeBd, attachmentFields.AttachedMolecule); n > 0 {
							fmt.Fprintf(os.Stderr, "Closed %d molecule step(s) for %s\n", n, attachmentFields.AttachedMolecule)
						}
						if closeErr := forceCloseIssueWithRetry(
							noMergeBd.ForceCloseWithReason,
							attachmentFields.AttachedMolecule,
							"done",
							"Attached molecule %s closed",
						); closeErr != nil && !errors.Is(closeErr, beads.ErrNotFound) {
							style.PrintWarning("could not close attached molecule %s after 3 attempts: %v", attachmentFields.AttachedMolecule, closeErr)
							canCloseIssue = false
						}
					}

					closeReason := "No-merge work completed; merge queue skipped"
					if prURL != "" {
						closeReason = fmt.Sprintf("%s\npr_url: %s", closeReason, prURL)
					}
					if canCloseIssue {
						if closeErr := forceCloseIssueWithRetry(
							noMergeBd.ForceCloseWithReason,
							issueID,
							closeReason,
							"Issue %s closed (no-merge)",
						); closeErr != nil {
							style.PrintWarning("could not close issue %s after 3 attempts: %v (issue may be left HOOKED)", issueID, closeErr)
						}
					}
				}

				// Skip MR creation, go to witness notification
				goto notifyWitness
			}
		}

		// Determine target branch for the MR.
		// Priority: explicit --target flag > formula_vars base_branch > integration branch auto-detect > rig default.
		target := defaultBranch
		explicitTarget := false

		// 1. Explicit --target flag (highest priority — polecat knows its base branch).
		// This is the most reliable path: the formula passes {{base_branch}} directly,
		// avoiding any dependency on bd.Show() or Dolt availability.
		if doneTarget != "" {
			target = doneTarget
			explicitTarget = true
			fmt.Printf("  Target branch: %s (from --target flag)\n", target)
		}

		// 2. Check for --base-branch override in formula vars (stored on bead at sling time).
		// Fallback for polecats dispatched before --target flag existed, or when
		// the formula doesn't pass --target explicitly.
		if !explicitTarget && target == defaultBranch && sourceIssueForNoMerge != nil {
			if af := beads.ParseAttachmentFields(sourceIssueForNoMerge); af != nil {
				if bb := extractFormulaVar(af.FormulaVars, "base_branch"); bb != "" && bb != defaultBranch {
					target = bb
					fmt.Printf("  Target branch override: %s (from formula_vars)\n", target)
				}
			}
		}

		// 3. Auto-detect integration branch from epic hierarchy (if enabled).
		// Only overrides if no explicit target was set above.
		if !explicitTarget && target == defaultBranch {
			if refineryIntegrationEnabled(townRoot, rigName) {
				autoTarget, err := beads.DetectIntegrationBranch(sourceBD, g, issueID)
				if err == nil && autoTarget != "" {
					target = autoTarget
				}
			}
		}

		// gt-a8i3: refuse a self-targeted MR no matter which of the sources
		// above produced it (known cause: a resume dispatch's base_branch
		// formula var leaking the resume branch) — guarded unconditionally
		// since a self-target is never valid regardless of cause. Also
		// refuses an unexplained polecat/* target (gt-w2jc): explicitTarget
		// is true only for the --target flag path above, never for the
		// formula_vars/auto-detect paths that leaked the self-target once
		// already.
		var targetErr error
		target, targetErr = resolveMRTarget(target, branch, defaultBranch, explicitTarget)
		if targetErr != nil {
			return targetErr
		}

		// Get source issue for priority inheritance
		var priority int
		carriedFrom := ""
		if donePriority >= 0 {
			// An explicit --priority is the submitter's own intent; nothing
			// carries over it.
			priority = donePriority
		} else {
			// A superseded MR for this issue may hold a manual bump the source
			// issue never saw (gt-m7fm; see carriedMRPriority).
			priority, carriedFrom = carriedMRPriority(bd, issueID, sourceIssueForNoMerge.Priority)
		}

		// Pre-declare for checkpoint goto (gt-aufru)
		var existingMR *beads.Issue
		var commitSHA string

		// GH#3032: Resolve HEAD commit SHA for MR dedup.
		// Branch name alone is not a valid dedup key — a polecat may push new
		// commits to the same branch after a gate failure. The commit SHA
		// distinguishes genuinely new submissions from idempotent retries.
		commitSHA, _ = g.Rev("HEAD")

		// Resume: skip MR creation if already completed in a previous run (gt-aufru).
		// Mirrors the push checkpoint pattern above. Without this, every retry
		// re-attempts bd.Create which hits unique constraints or creates duplicates.
		// Validate that the checkpoint MR still describes this run before resuming
		// onto it (ge-sbo); mrCheckpointStaleReason states what that requires.
		if checkpoints[CheckpointMRCreated] != "" {
			cpMRID := checkpoints[CheckpointMRCreated]
			if cpMR, cpErr := bd.Show(cpMRID); cpErr == nil && cpMR != nil {
				if reason := mrCheckpointStaleReason(cpMR, branch, commitSHA); reason != "" {
					fmt.Printf("→ Discarding stale MR checkpoint %s (%s)\n", cpMRID, reason)
				} else {
					if err := validateMergeRequestSource(cpMR, issueID, sourceIssueForNoMerge); err != nil {
						mrFailed = true
						errMsg := fmt.Sprintf("checkpoint MR validation failed: %v", err)
						doneErrors = append(doneErrors, errMsg)
						style.PrintWarning("%s\nBranch is pushed but MR bead not trusted. Witness will be notified.", errMsg)
						goto notifyWitness
					}
					mrID = cpMRID
					fmt.Printf("%s MR already created (resumed from checkpoint: %s)\n", style.Bold.Render("✓"), mrID)
					goto afterMR
				}
			}
			// If MR lookup fails, fall through to create/find MR normally.
		}

		// Check if MR bead already exists for this branch+SHA (idempotency)
		if commitSHA != "" {
			existingMR, err = bd.FindMRForBranchAndSHA(branch, commitSHA)
		} else {
			existingMR, err = bd.FindMRForBranch(branch)
		}
		if err != nil {
			style.PrintWarning("could not check for existing MR: %v", err)
			// Continue with creation attempt - Create will fail if duplicate
		}

		if existingMR != nil {
			// MR already exists with same branch AND commit — true idempotent retry
			if err := validateMergeRequestSource(existingMR, issueID, sourceIssueForNoMerge); err != nil {
				mrFailed = true
				errMsg := fmt.Sprintf("existing MR validation failed: %v", err)
				doneErrors = append(doneErrors, errMsg)
				style.PrintWarning("%s\nBranch is pushed but existing MR bead not trusted. Witness will be notified.", errMsg)
				goto notifyWitness
			}
			mrID = existingMR.ID
			fmt.Printf("%s MR already exists (idempotent)\n", style.Bold.Render("✓"))
			fmt.Printf("  MR ID: %s\n", style.Bold.Render(mrID))
		} else {
			// Build MR bead title and description
			title := fmt.Sprintf("Merge: %s", issueID)
			description := fmt.Sprintf("branch: %s\ntarget: %s\nsource_issue: %s\nrig: %s",
				branch, target, issueID, rigName)
			if commitSHA != "" {
				description += fmt.Sprintf("\ncommit_sha: %s", commitSHA)
			}
			if doneSkipVerify {
				description += "\nskip_verify: true"
			}
			if worker != "" {
				description += fmt.Sprintf("\nworker: %s", worker)
			}
			if agentBeadID != "" {
				description += fmt.Sprintf("\nagent_bead: %s", agentBeadID)
			}

			// Add conflict resolution tracking fields (initialized, updated by Refinery)
			description += "\nretry_count: 0"
			description += "\nlast_conflict_sha: null"
			description += "\nconflict_task_id: null"

			// Phase 3: Add pre-verification metadata if polecat ran gates after rebasing.
			// The refinery uses these fields to fast-path merge without re-running gates.
			// honorPreVerified is gated on the same gate-command binding gt sling reads,
			// so the stamp can't say "verified" when there was nothing to verify (gt-k4sy).
			honorPreVerified, preVerifiedWarning := resolvePreVerifiedClaim(donePreVerified, townRoot, rigName)
			if preVerifiedWarning != "" {
				style.PrintWarning("%s", preVerifiedWarning)
			}
			fullGatesVerified := false
			if honorPreVerified {
				mq := rig.ResolveMergeQueueConfig(townRoot, rigName)
				// config.CombineGateSetSHA is the same function the refinery's
				// currentGateSetSHAFn calls (internal/refinery/engineer.go); the
				// two values must agree exactly or the fast-path never fires
				// (om-gate T8).
				gateSetSHA := config.CombineGateSetSHA(mq, rig.LoadNamedGateCommands(townRoot, rigName))
				// gt-azmw: renew the exiting heartbeat across this bounded gate
				// run (up to five 10m gates), exactly as the default test-verify
				// below does — see StartExitingHeartbeatKeepAlive.
				stopHeartbeat := polecat.StartExitingHeartbeatKeepAlive(townRoot, heartbeatSession, "gt done", issueID)
				// gt-l6by: the pre-verified test gate may start the rig's
				// container-backed suite, so it takes a container-gate slot
				// under the same polecat role the default gate uses.
				gateSlot := preVerifySlot{townRoot: townRoot, role: fmt.Sprintf("%s/%s", rigName, polecatName)}
				stamp, ok, warning := resolvePreVerification(g, cwd, defaultBranch, target, mq, gateSetSHA, gateSlot)
				stopHeartbeat()
				if warning != "" {
					style.PrintWarning("%s", warning)
				}
				if ok {
					fullGatesVerified = true
					description += "\npre_verified: true"
					description += fmt.Sprintf("\npre_verified_at: %s", time.Now().UTC().Format(time.RFC3339))
					description += fmt.Sprintf("\npre_verified_base: %s", stamp.verifiedBase)
					description += fmt.Sprintf("\npre_verified_gates: %s", stamp.gateSetSHA)
					description += "\npre_verified_exit: 0"
					description += fmt.Sprintf("\npre_verified_log: %s", stamp.logSHA256)
				}
			}

			// gt-h9kf: gt done must itself test at least the branch's changed
			// packages before an MR can be created — polecats were submitting
			// with their own new tests never run (2 of 4 gate rejections in a
			// 90-minute window, each costing a full refinery gate cycle plus a
			// redispatch). Runs unconditionally unless the polecat already ran
			// the full gate set via --pre-verified (fullGatesVerified, which
			// already covers this and more) or explicitly opted out with
			// --skip-verify. A failure here returns an error and no MR bead is
			// created — that refusal is the fix.
			if !doneSkipVerify && !fullGatesVerified {
				verifyMQ := rig.ResolveMergeQueueConfig(townRoot, rigName)
				verifyRole := fmt.Sprintf("%s/%s", rigName, polecatName)
				// gt-azmw: this gate can hold the container slot for 60m
				// (defaultTestVerifySlotTimeout) and then run for a scaled run
				// budget (30m minimum, more with more changed packages), all of
				// it silent, while the heartbeat written at gt done's start ages
				// past every consumer's stale threshold. Renew it for exactly as
				// long as this bounded stage can run. The gate itself now logs
				// progress lines to the pane and the verify log (gt-pnkd).
				stopHeartbeat := polecat.StartExitingHeartbeatKeepAlive(townRoot, heartbeatSession, "gt done", issueID)
				verify, verifyErr := runDefaultTestVerification(g, cwd, defaultBranch, target, verifyMQ, townRoot, verifyRole)
				stopHeartbeat()
				if verifyErr != nil {
					return verifyErr
				}
				if verify.lintRan {
					fmt.Printf("%s Default lint-verify passed (%s, %s)\n", style.Bold.Render("✓"), verify.lintCommand, verify.lintElapsed.Round(time.Second))
					description += "\nlint_verified: true"
					description += fmt.Sprintf("\nlint_verified_command: %s", verify.lintCommand)
				}
				if verify.skipReason != "" {
					style.PrintWarning("gt done: skipping default test-verify: %s", verify.skipReason)
				} else if verify.ran {
					fmt.Printf("%s Default test-verify passed (full test_command)\n", style.Bold.Render("✓"))
					description += "\ntest_verified: true"
					description += fmt.Sprintf("\ntest_verified_at: %s", time.Now().UTC().Format(time.RFC3339))
					description += fmt.Sprintf("\ntest_verified_sha: %s", commitSHA)
					description += fmt.Sprintf("\ntest_verified_scope: %s", verify.scope)
					if len(verify.packages) > 0 {
						description += fmt.Sprintf("\ntest_verified_packages: %s", strings.Join(verify.packages, ","))
					}
					description += fmt.Sprintf("\ntest_verified_slot_used: %t", verify.slotUsed)
					description += "\ntest_verified_exit: 0"
					description += fmt.Sprintf("\ntest_verified_log: %s", verify.logSHA256)
					// gt-pnkd: record the budgets the gate actually resolved and
					// what it actually cost, so a refinery gate flap can be told
					// apart from a budget that was too tight without re-reading
					// the polecat's (now nuked) worktree .runtime log.
					description += fmt.Sprintf("\ntest_verified_run_budget: %s", humanDuration(verify.runBudget))
					description += fmt.Sprintf("\ntest_verified_slot_cap: %s", humanDuration(verify.slotTimeout))
					description += fmt.Sprintf("\ntest_verified_slot_wait: %s", verify.slotWait.Round(time.Second))
					description += fmt.Sprintf("\ntest_verified_elapsed: %s", verify.runElapsed.Round(time.Second))
				}
			}

			mrIssue, err := bd.Create(beads.CreateOptions{
				Title:       title,
				Labels:      []string{"gt:merge-request"},
				Priority:    priority,
				Description: description,
				Ephemeral:   true,
				Rig:         rigName, // Ensure MR bead is created in the rig's database (gt-7y7)
			})
			if err != nil {
				// Non-fatal: record the error and skip to notifyWitness.
				// Push succeeded so branch is on remote, but MR bead failed.
				// Set mrFailed so the witness knows not to send MERGE_READY.
				mrFailed = true
				errMsg := fmt.Sprintf("MR bead creation failed: %v", err)
				doneErrors = append(doneErrors, errMsg)
				style.PrintWarning("%s\nBranch is pushed but MR bead not created. Witness will be notified.", errMsg)
				goto notifyWitness
			}
			mrID = mrIssue.ID

			// Guard against empty ID from bd create (observed in ephemeral/wisp mode).
			// Fail fast with a clear message rather than passing "" to bd.Show.
			if mrID == "" {
				mrFailed = true
				errMsg := "MR bead creation returned empty ID"
				doneErrors = append(doneErrors, errMsg)
				style.PrintWarning("%s\nBranch is pushed but MR bead has no ID. Witness will be notified.", errMsg)
				goto notifyWitness
			}

			// GH#1945: Verify MR bead is readable before considering it confirmed.
			// bd.Create() succeeds when the bead is written locally, but if the write
			// didn't persist (Dolt failure, corrupt state), we'd nuke the worktree
			// with no MR in the queue — losing the polecat's work permanently.
			if verifiedMR, verifyErr := bd.Show(mrID); verifyErr != nil || verifiedMR == nil {
				mrFailed = true
				errMsg := fmt.Sprintf("MR bead created but verification read-back failed (id=%s): %v", mrID, verifyErr)
				doneErrors = append(doneErrors, errMsg)
				style.PrintWarning("%s\nBranch is pushed but MR bead not confirmed. Preserving worktree.", errMsg)
				goto notifyWitness
			}

			// gt-gpy: Validate that the MR bead landed in the rig's database.
			// If the source bead has a cross-rig prefix (e.g., hq-), the routing
			// could still resolve to the wrong database despite Rig: rigName.
			// This is a warning-only guard — mrFailed is NOT set on mismatch.
			if prefixErr := beads.ValidateRigPrefix(townRoot, rigName, mrID); prefixErr != nil {
				style.PrintWarning("MR bead prefix mismatch: %v\nThe refinery may not find this MR — check 'gt mq list %s'", prefixErr, rigName)
			}

			// GH#3032: Supersede older open MRs for the same source issue.
			// When a polecat re-submits after fixing a gate failure, the old MR
			// (same branch, different SHA) is stale. Close it so the refinery
			// doesn't process the old submission.
			//
			// gt-c5uv: the old MR was usually submitted by a *different* polecat
			// (deacon redispatch with resume_branch after a rejection), and its
			// agent bead's active_mr still names it. Clearing that pointer here is
			// what keeps the superseded worker out of a permanent
			// idle-pr-open/reusable=false state — nothing downstream does it. See
			// supersedeOpenMRsForIssue.
			for _, sup := range supersedeOpenMRsForIssue(bd, bd.ForAgentBead(), issueID, mrIssue, townRoot, rigName) {
				fmt.Printf("  %s Superseded old MR: %s\n", style.Dim.Render("○"), sup.ID)
				if sup.AgentCleared {
					fmt.Printf("  %s Cleared active_mr on %s\n", style.Dim.Render("○"), sup.AgentBead)
				}
			}
			if carriedFrom != "" {
				fmt.Printf("  %s Inherited priority P%d from %s\n", style.Dim.Render("○"), priority, carriedFrom)
			}

			// Update agent bead with active_mr reference (for traceability).
			// ForAgentBead resolves the agent bead's database (rig-local first,
			// legacy town fallback — gt-8we) to avoid the "issue not found"
			// warning that leaves active_mr null after every gt done (hq-e73z).
			if agentBeadID != "" {
				if err := bd.ForAgentBead().UpdateAgentActiveMR(agentBeadID, mrID); err != nil {
					style.PrintWarning("could not update agent bead with active_mr: %v", err)
				}
			}

			// GH#2599: Back-link source issue to MR bead for discoverability.
			if issueID != "" {
				comment := fmt.Sprintf("MR created: %s", mrID)
				if err := sourceBD.AddComment(issueID, comment); err != nil {
					style.PrintWarning("could not back-link source issue %s to MR %s: %v", issueID, mrID, err)
				}
			}

			// Success output
			fmt.Printf("%s Work submitted to merge queue (verified)\n", style.Bold.Render("✓"))
			fmt.Printf("  MR ID: %s\n", style.Bold.Render(mrID))

			// NOTE: Refinery nudge is deferred to AFTER the Dolt branch merge
			// (see post-merge nudge below). Nudging here would race with the
			// merge — refinery wakes up and queries main before the polecat's
			// Dolt branch (containing the MR bead) is merged.
		}

		// Write MR checkpoint for resume (gt-aufru)
		if mrID != "" && agentBeadID != "" {
			// ForAgentBead: dual-scope agent-bead resolution (rig-local first,
			// legacy town fallback — gt-8we).
			cpBd := beads.New(cwd).ForAgentBead()
			writeDoneCheckpoint(cpBd, agentBeadID, CheckpointMRCreated, mrID)
		}

	afterMR:
		fmt.Printf("  Source: %s\n", branch)
		fmt.Printf("  Target: %s\n", target)
		fmt.Printf("  Issue: %s\n", issueID)
		if worker != "" {
			fmt.Printf("  Worker: %s\n", worker)
		}
		fmt.Printf("  Priority: P%d\n", priority)
		fmt.Println()
		fmt.Printf("%s\n", style.Dim.Render("The Refinery will process your merge request."))
	} else {
		// For ESCALATED or DEFERRED, just print status
		fmt.Printf("%s Signaling %s\n", style.Bold.Render("→"), exitType)
		if issueID != "" {
			fmt.Printf("  Issue: %s\n", issueID)
		}
		fmt.Printf("  Branch: %s\n", branch)
	}

notifyWitness:
	// Nudge refinery — MR bead is already on main (transaction-based shared main).
	var wakeConflictCandidates []string
	var wakeConflictBD *beads.Beads
	if shouldNudgeRefinery(exitType, mrID) {
		nudgeRefinery(rigName, "MERGE_READY received - check inbox for pending work")
	} else if !pushFailed {
		// A conflict-resolution completion releases an MR that already exists,
		// so shouldNudgeRefinery's COMPLETED+new-MR gate above can never fire for
		// it and nothing else emits a wake for the blocked->ready transition —
		// see wakeRefineryForReadyConflict (gt-rv8h).
		//
		// Skipped when this run's own push failed: the resolution's new head
		// would then not be on origin, and waking the refinery would only invite
		// it to merge a stale head. The MR is left blocked and the existing
		// push-failure recovery path owns it.
		wakeConflictBD = sourceBD
		if wakeConflictBD == nil {
			wakeConflictBD = beads.New(cwd)
		}
		// issueID is branch-derived unless --issue was passed, and a conflict
		// polecat may still be on the resolved branch (whose name carries the
		// source issue, not the task). The agent bead's hook_bead is the other
		// candidate for "the conflict task this completion just finished".
		// Captured here, before updateAgentStateAfterSubmission below clears
		// the agent bead's hook_bead field, but the actual readiness check is
		// deferred until after that call closes the hooked conflict task
		// (gt-ue2h): checking readiness here always found the task still open,
		// since gt done itself hadn't closed it yet.
		wakeConflictCandidates = conflictResolutionCandidates(cwd, agentBeadID, issueID)
	}

	// Write completion metadata to agent bead for audit trail.
	// Self-managed completion (gt-1qlg): metadata is retained for anomaly
	// detection and crash recovery by witness patrol, but the witness no
	// longer processes routine completions from these fields.
	fmt.Printf("\nNotifying Witness...\n")
	if agentBeadID != "" {
		// ForAgentBead: dual-scope agent-bead resolution (rig-local first,
		// legacy town fallback — gt-8we).
		completionBd := beads.New(cwd).ForAgentBead()
		meta := &beads.CompletionMetadata{
			ExitType:       exitType,
			MRID:           mrID,
			Branch:         branch,
			HookBead:       issueID,
			MRFailed:       mrFailed,
			PushFailed:     pushFailed,
			CompletionTime: time.Now().UTC().Format(time.RFC3339),
		}
		if err := completionBd.UpdateAgentCompletion(agentBeadID, meta); err != nil {
			style.PrintWarning("could not write completion metadata to agent bead: %v", err)
		}
	}

	// Write witness notification checkpoint for resume (gt-aufru)
	if agentBeadID != "" {
		// ForAgentBead: dual-scope agent-bead resolution (rig-local first,
		// legacy town fallback — gt-8we).
		cpBd := beads.New(cwd).ForAgentBead()
		writeDoneCheckpoint(cpBd, agentBeadID, CheckpointWitnessNotified, "ok")
	}

	// Self-report cleanup_status (ZFC #10). Deliberately NOT part of
	// updateAgentStateAfterSubmission below: that call is skipped whenever
	// push or MR submission failed, which is precisely the case where the
	// witness needs to see "has_unpushed" on the slot. See
	// selfReportCleanupStatus for why this write must never be skipped.
	//
	// Addressed through the rig directory rather than cwd, matching
	// updateAgentStateOnDone below: both hit the same database, and the rig path
	// still resolves if the worktree is already gone.
	selfReportCleanupStatus(g, branch, beads.New(filepath.Join(townRoot, rigName)).ForAgentBead(), agentBeadID, doneCleanupStatus)

	// Log done event (townlog and activity feed)
	if err := LogDone(townRoot, sender, issueID); err != nil {
		style.PrintWarning("could not log done event: %v", err)
	}
	if err := events.LogFeed(events.TypeDone, sender, events.DonePayload(issueID, branch)); err != nil {
		style.PrintWarning("could not log feed event: %v", err)
	}

	// Update agent bead state (ZFC: self-report completion). If push/MR failed,
	// keep the hook intact so Witness can recover the still-open work.
	if err := updateAgentStateAfterSubmission(cwd, townRoot, exitType, issueID, pushFailed, mrFailed); err != nil {
		return err
	}

	// Check the conflict-resolution wake now that the hooked conflict task
	// bead above has actually been closed (updateAgentStateOnDone, reached via
	// updateAgentStateAfterSubmission). wakeConflictCandidates is nil whenever
	// shouldNudgeRefinery already fired or the push failed (gt-ue2h).
	if wakeConflictCandidates != nil {
		wakeRefineryForReadyConflict(wakeConflictBD.Show, rigName, wakeConflictCandidates...)
	}

	// Nudge witness only after hook/cleanup state is updated. Otherwise witness can
	// evaluate slot availability against stale hook_bead or cleanup_status and emit
	// false SLOT_BLOCKED/SLOT_OPEN signals.
	nudgeWitness(rigName, fmt.Sprintf("POLECAT_DONE %s exit=%s", polecatName, exitType))
	fmt.Printf("%s Witness notified of %s (via nudge)\n", style.Bold.Render("✓"), exitType)

	// Every final exit status (COMPLETED, ESCALATED, DEFERRED) retires the
	// polecat session after durable handoff, unless it's a handoff-triggered
	// DEFERRED (gt-5g3e). Preserve the feature branch and metadata;
	// Witness/refinery cleanup owns the sandbox.
	isPolecat := false
	mergeStrategy := ""
	fromHandoff := os.Getenv(envDoneFromHandoff) == "1"
	if roleInfo, err := GetRoleWithContext(cwd, townRoot); err == nil && roleInfo.Role == RolePolecat {
		isPolecat = true

		if pushFailed || mrFailed {
			fmt.Printf("%s Work needs recovery (push or MR failed) — session preserved\n", style.Bold.Render("⚠"))
		}
		// Resolve convoy info for every final exit, not only COMPLETED: DEFERRED
		// and ESCALATED can also retire the session now, so a local-review convoy
		// on either of those exits must still be exempted (gt-5g3e).
		if shouldResolveConvoyForRetirement(issueID, convoyInfo) {
			convoyInfo = getConvoyInfoFromSourceIssue(sourceIssueForNoMerge)
			if convoyInfo == nil {
				convoyInfo = getConvoyInfoForIssue(issueID)
			}
		}
		if convoyInfo != nil {
			mergeStrategy = convoyInfo.MergeStrategy
		}
	}

	fmt.Println()
	if !isPolecat {
		fmt.Printf("%s Session exiting\n", style.Bold.Render("→"))
		fmt.Printf("  Witness will handle cleanup.\n")
	}

	// Retire the live session as the final action. The PID exclusion prevents
	// killing gt done before all metadata and notifications above are written.
	if isPolecat {
		retirePolecatSessionAfterFinalExit(exitType, mergeStrategy, pushFailed, mrFailed, fromHandoff, rigName, polecatName, os.Getpid())
	}

	// Fail closed on a push that could not be verified against origin (gt-2wqt):
	// without a non-zero status the caller cannot tell a dropped submission from
	// a landed one. Everything above — witness notification, completion
	// metadata, session preservation — has already run.
	if pushVerifyErr != nil {
		return pushVerifyErr
	}

	return nil
}

// pushSubmoduleChanges detects submodules modified between baseRef
// and HEAD, and pushes each submodule's new commit to its remote before the
// parent repo push. This prevents the parent's submodule pointer from
// referencing commits that don't exist on the submodule's remote (gt-dzs).
func pushSubmoduleChanges(g *git.Git, baseRef string) {
	subChanges, err := g.SubmoduleChanges(baseRef, "HEAD")
	if err != nil {
		// Non-fatal: repos without submodules return nil, nil.
		// Only warn if the error is real (not just "no submodules").
		style.PrintWarning("could not detect submodule changes: %v", err)
		return
	}
	for _, sc := range subChanges {
		if sc.NewSHA == "" {
			continue // Submodule removed, nothing to push
		}
		shortSHA := sc.NewSHA
		if len(shortSHA) > 8 {
			shortSHA = shortSHA[:8]
		}
		fmt.Printf("Pushing submodule %s (%s)...\n", sc.Path, shortSHA)
		if subPushErr := g.PushSubmoduleCommit(sc.Path, sc.NewSHA, "origin"); subPushErr != nil {
			style.PrintWarning("submodule push failed for %s: %v (parent push may fail)", sc.Path, subPushErr)
		} else {
			fmt.Printf("%s Submodule %s pushed\n", style.Bold.Render("✓"), sc.Path)
		}
	}
}

func forceCloseIssueWithRetry(closeFn func(string, ...string) error, issueID, reason, successFormat string) error {
	return forceCloseIssueWithRetrySleep(closeFn, issueID, reason, successFormat, time.Sleep)
}

func forceCloseIssueWithRetrySleep(closeFn func(string, ...string) error, issueID, reason, successFormat string, sleep func(time.Duration)) error {
	var closeErr error
	for attempt := 1; attempt <= 3; attempt++ {
		closeErr = closeFn(reason, issueID)
		if closeErr == nil {
			fmt.Printf("%s "+successFormat+"\n", style.Bold.Render("✓"), issueID)
			return nil
		}
		if attempt < 3 {
			style.PrintWarning("close attempt %d/3 failed: %v (retrying in %ds)", attempt, closeErr, attempt*2)
			sleep(time.Duration(attempt*2) * time.Second)
		}
	}
	return closeErr
}

func notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, reason string) {
	if townRoot == "" || rigName == "" || issueID == "" {
		return
	}
	if sender == "" {
		sender = fmt.Sprintf("%s/polecat", rigName)
	}

	router := mail.NewRouter(townRoot)
	defer router.WaitPendingNotifications()
	msg := &mail.Message{
		To:      fmt.Sprintf("%s/witness", rigName),
		From:    sender,
		Subject: fmt.Sprintf("DONE_CLOSE_SKIPPED: %s", issueID),
		Body: fmt.Sprintf("gt done skipped closing %s.\n\nReason: %s\n\nThe bead remains open for witness/mayor review.",
			issueID, reason),
	}
	if err := router.Send(msg); err != nil {
		style.PrintWarning("could not notify witness about skipped close: %v", err)
	} else {
		fmt.Printf("%s Witness notified: DONE_CLOSE_SKIPPED\n", style.Bold.Render("✓"))
	}
}

func noteVerifiedPushFailure(sourceBD *beads.Beads, cwd, issueID, branch, commit string, verifyErr error) {
	if issueID == "" || cwd == "" {
		return
	}
	bd := sourceBD
	if bd == nil {
		bd, _, _ = routedIssueBeads(cwd, issueID)
	}
	inProgress := "in_progress"
	_ = bd.Update(issueID, beads.UpdateOptions{Status: &inProgress})
	msg := fmt.Sprintf("verified_push_failed: commit %s not verified on origin/%s: %v", commit, branch, verifyErr)
	_ = bd.AddComment(issueID, msg)
}

func noteVerifiedPushSkipped(sourceBD *beads.Beads, cwd, issueID, branch, commit, reason string) {
	if issueID == "" || cwd == "" {
		return
	}
	msg := fmt.Sprintf("verified_push_skipped: commit %s branch origin/%s reason=%s", commit, branch, reason)
	bd := sourceBD
	if bd == nil {
		bd, _, _ = routedIssueBeads(cwd, issueID)
	}
	_ = bd.AddComment(issueID, msg)
}

// verifyPushLandedBeforeMR asserts that the remote branch tip is exactly the
// commit gt done is about to declare in an MR bead (gt-2wqt). It is the only
// source of the "Branch pushed" claim: a push that exits 0 while origin keeps
// an older tip is indistinguishable from success until ls-remote is compared
// against HEAD.
//
// Every error return is fatal to the submission — the caller must create no MR
// bead, because an MR whose commit_sha origin does not have makes the refinery
// gate and merge a tree that silently lacks the fix.
func verifyPushLandedBeforeMR(g *git.Git, townRoot, rigName, branch, commit string) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		head, headErr := g.Rev("HEAD")
		if headErr != nil {
			return fmt.Errorf("verified_push_failed: cannot resolve HEAD for branch %s: %w", branch, headErr)
		}
		commit = strings.TrimSpace(head)
	}
	verifyErr := g.VerifyPushedCommit("origin", branch, commit)
	if verifyErr == nil {
		return nil
	}

	// The worktree's git context can be broken (GH #1348), where the ls-remote
	// above fails to run rather than reporting a mismatch. Retry the same
	// remote assertion from the rig's bare repo, which shares the object
	// database.
	//
	// gt-2wqt: this fallback used to compare the bare repo's *local*
	// refs/heads/<branch>, which is the polecat's own HEAD whenever the worktree
	// is on that branch (worktrees share the ref store) — so it passed on
	// exactly the unpushed-commit case the guard exists to catch, and gt done
	// reported a verified push that never happened. A local ref is never
	// evidence of a remote push; only a query of the remote is.
	bareRepoPath := filepath.Join(townRoot, rigName, ".repo.git")
	if _, statErr := os.Stat(bareRepoPath); statErr == nil {
		bareGit := git.NewGitWithDir(bareRepoPath, "")
		if bareErr := bareGit.VerifyPushedCommit("origin", branch, commit); bareErr == nil {
			return nil
		}
	}
	return describePushVerificationFailure(g, branch, commit, verifyErr)
}

// describePushVerificationFailure renders a failed push assertion with the full
// local and remote SHAs (gt-2wqt): the operator has to see both to know how far
// behind origin is before the work is re-pushed.
func describePushVerificationFailure(g *git.Git, branch, commit string, cause error) error {
	remoteTip, tipErr := g.PushRemoteBranchTip("origin", branch)
	if tipErr != nil || strings.TrimSpace(remoteTip) == "" {
		remoteTip = "(missing on origin)"
	}
	return fmt.Errorf("verified_push_failed: branch %s is not at the commit this merge request would declare; no MR created\n"+
		"  local HEAD:  %s\n"+
		"  origin/%s:  %s\n"+
		"  %v", branch, commit, branch, remoteTip, cause)
}

// pushBranchToOrigin sends refspec to origin, falling back to the rig's bare
// repo when the worktree's git context cannot reach the remote (GH #1348).
//
// The landing retry at afterPush re-sends this same push, so the two share one
// function rather than two copies of the fallback: a retry that pushed
// differently from the attempt it retries would verify a state that attempt
// never aimed at (gt-0opm).
func pushBranchToOrigin(g *git.Git, townRoot, rigName, refspec string) error {
	err := g.Push("origin", refspec, false)
	if err == nil {
		return nil
	}
	style.PrintWarning("primary push failed: %v — trying bare repo fallback...", err)
	bareRepoPath := filepath.Join(townRoot, rigName, ".repo.git")
	if _, statErr := os.Stat(bareRepoPath); statErr != nil {
		return err
	}
	bareGit := git.NewGitWithDir(bareRepoPath, "")
	bareErr := bareGit.Push("origin", refspec, false)
	if bareErr != nil {
		style.PrintWarning("bare repo push also failed: %v", bareErr)
		return bareErr
	}
	fmt.Printf("%s Branch pushed via bare repo fallback\n", style.Bold.Render("✓"))
	return nil
}

// pushLandingRetryDelays is the wait before the single re-attempt a branch push
// gets after its first attempt failed to prove out on origin (gt-0opm).
//
// One retry, not a ladder: the failure this closes is a push whose command
// reported an error while origin was taking the objects anyway, which resolves
// in seconds. A remote that is genuinely refusing answers the same way on the
// retry, and a failed submission must not hold the polecat's slot open.
var pushLandingRetryDelays = []time.Duration{3 * time.Second}

// landBranchPushBeforeMR delivers the branch to origin and proves the commit
// arrived, retrying the push and the assertion as one unit. It is the gate
// between a push whose first attempt failed and the terminal "no merge request
// created" exit (gt-0opm).
//
// A failed first attempt is not a verdict: `git push` reports an error for
// outcomes that leave the branch on origin anyway — a client-side timeout after
// the receiving side took the objects, a worktree git context only the bare-repo
// fallback could work around. Exiting there cost the merge request rather than a
// retry: the branch was on origin, the issue stayed hooked, and the refinery,
// blind to anything outside the queue by protocol, had nothing to look at.
//
// Origin is queried before anything is re-sent, so a landing already in place is
// proven without a second push. attemptPush must therefore be idempotent —
// callers pass the same branch:branch refspec the first attempt used, which is a
// no-op fast-forward when origin has the commit and fails closed (never a force)
// when it does not. recovered reports whether the retry was what proved it.
func landBranchPushBeforeMR(attemptPush, verify func() error, sleep func(time.Duration)) (bool, error) {
	verifyErr := verify()
	for i := 0; verifyErr != nil && i < len(pushLandingRetryDelays); i++ {
		sleep(pushLandingRetryDelays[i])
		pushErr := attemptPush()
		if verifyErr = verify(); verifyErr == nil {
			return true, nil
		}
		if pushErr != nil {
			verifyErr = fmt.Errorf("%w (retry push: %v)", verifyErr, pushErr)
		}
	}
	return false, verifyErr
}

// unlandedPushMessage renders the terminal gt done error for a submission whose
// branch never proved out on origin.
//
// The push-command error says why the send broke and the assertion says where
// origin stands; a reader of a strand needs both. When only the assertion
// failed, it is the whole story (gt-0opm).
func unlandedPushMessage(branch string, pushErr error, pushFailureDetail string, verifyErr error) string {
	if pushErr == nil {
		return verifyErr.Error()
	}
	msg := fmt.Sprintf("push failed for branch '%s': %v", branch, pushErr)
	if pushFailureDetail != "" {
		msg = fmt.Sprintf("%s (%s)", msg, pushFailureDetail)
	}
	return fmt.Sprintf("%s [after retry: %v]", msg, verifyErr)
}

// shouldNudgeRefinery reports whether a gt done invocation may wake the
// refinery. Only COMPLETED exits create an MR bead; DEFERRED and ESCALATED
// exits (polecats finishing operational tasks with no code changes) must
// never emit MQ_SUBMIT, or the refinery wakes from backoff to find an empty
// merge queue (gh#3885). The exitType check is defensive: it holds the
// invariant even if a future code path populates mrID outside COMPLETED.
func shouldNudgeRefinery(exitType, mrID string) bool {
	return exitType == ExitCompleted && mrID != ""
}

// setDoneIntentLabel writes a done-intent:<type>:<unix-ts> label on the agent bead
// EARLY in gt done, before push/MR. This allows the Witness to detect polecats that
// crashed mid-gt-done: if the session is dead but done-intent exists, the polecat was
// trying to exit and should be auto-nuked.
//
// Follows the existing idle:N / backoff-until:TIMESTAMP label pattern.
// Non-fatal: if this fails, gt done continues without the safety net.
func setDoneIntentLabel(bd *beads.Beads, agentBeadID, exitType string) {
	if agentBeadID == "" {
		return
	}
	label := fmt.Sprintf("done-intent:%s:%d", exitType, time.Now().Unix())
	if err := bd.Update(agentBeadID, beads.UpdateOptions{
		AddLabels: []string{label},
	}); err != nil {
		// Non-fatal: warn but continue
		fmt.Fprintf(os.Stderr, "Warning: couldn't set done-intent label on %s: %v\n", agentBeadID, err)
	}
}

// clearDoneIntentLabel removes any done-intent:* label from the agent bead.
// Called at the end of updateAgentStateOnDone on clean exit.
// Uses read-modify-write pattern (same as clearAgentBackoffUntil).
func clearDoneIntentLabel(bd *beads.Beads, agentBeadID string) {
	if agentBeadID == "" {
		return
	}
	issue, err := bd.Show(agentBeadID)
	if err != nil {
		return // Agent bead gone, nothing to clear
	}

	var toRemove []string
	for _, label := range issue.Labels {
		if strings.HasPrefix(label, "done-intent:") {
			toRemove = append(toRemove, label)
		}
	}
	if len(toRemove) == 0 {
		return // No done-intent label to clear
	}

	if err := bd.Update(agentBeadID, beads.UpdateOptions{
		RemoveLabels: toRemove,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear done-intent label on %s: %v\n", agentBeadID, err)
	}
}

// DoneCheckpoint represents a checkpoint stage in the gt done flow (gt-aufru).
// Checkpoints are stored as labels on the agent bead, enabling resume after
// process interruption (context exhaustion, SIGTERM, etc.).
type DoneCheckpoint string

const (
	CheckpointPushed          DoneCheckpoint = "pushed"
	CheckpointMRCreated       DoneCheckpoint = "mr-created"
	CheckpointWitnessNotified DoneCheckpoint = "witness-notified"
)

// writeDoneCheckpoint writes a checkpoint label on the agent bead.
// Format: done-cp:<stage>:<value>:<unix-ts>
// The pushed stage stores "branch@sha" (see pushedCheckpointValue, gt-2wqt);
// other stages store their own opaque value.
// Non-fatal: if this fails, gt done continues without the checkpoint.
func writeDoneCheckpoint(bd *beads.Beads, agentBeadID string, cp DoneCheckpoint, value string) {
	if agentBeadID == "" {
		return
	}
	label := fmt.Sprintf("done-cp:%s:%s:%d", cp, value, time.Now().Unix())
	if err := bd.Update(agentBeadID, beads.UpdateOptions{
		AddLabels: []string{label},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't write checkpoint %s on %s: %v\n", cp, agentBeadID, err)
	}
}

// pushedCheckpointValue encodes the branch AND the commit a push checkpoint was
// written for (gt-2wqt). The checkpoint originally stored the branch alone,
// which made "already pushed" a claim about the branch name rather than about
// the commits on it: a gt done that pushed, failed its gate, and was re-run
// after a fix commit saw the same branch name, skipped both the push and the
// push verification, and created an MR declaring a commit origin never had.
func pushedCheckpointValue(branch, sha string) string {
	return branch + "@" + strings.TrimSpace(sha)
}

// pushedCheckpointBranch returns the branch recorded in a push checkpoint
// value. Values written before gt-2wqt hold the branch alone and still resolve.
func pushedCheckpointBranch(value string) string {
	if i := strings.LastIndex(value, "@"); i >= 0 {
		return value[:i]
	}
	return value
}

// pushedCheckpointMatches reports whether a push checkpoint was written for
// this exact branch at this exact commit. Anything else — a different branch, a
// different commit on the same branch, or a value that records no commit at all
// — is stale, so the push and its verification must run again.
func pushedCheckpointMatches(value, branch, sha string) bool {
	if value == "" || strings.TrimSpace(sha) == "" {
		return false
	}
	return value == pushedCheckpointValue(branch, sha)
}

// mrCheckpointStaleReason reports why a checkpointed merge-request bead no
// longer describes the submission this run is about to make, or "" when it
// still does.
//
// The checkpoint records an MR id and nothing else, so the bead is asked the
// questions FindMRForBranchAndSHA asks a fresh search: same branch, same
// commit, still in the queue. A checkpoint is a claim about a submission rather
// than about a name (gt-2wqt), and a rerun after an interrupted gt done reads a
// bead that may have been rejected and closed in between. Reading a closed MR
// as live reports "MR already created" and queues nothing, stranding the pushed
// work (gt-xgbv).
func mrCheckpointStaleReason(mr *beads.Issue, branch, commitSHA string) string {
	switch {
	case mr == nil:
		return "MR bead not found"
	case mr.Status != string(beads.StatusOpen):
		return "MR is " + mr.Status
	case !strings.HasPrefix(mr.Description, "branch: "+branch+"\n"):
		return "was for different branch"
	}
	// A legacy MR records no commit_sha, and an unreadable HEAD leaves nothing
	// to compare against: the branch and the queue status are all there is to
	// go on, the same fallback FindMRForBranchAndSHA makes.
	fields := beads.ParseMRFields(mr)
	if fields == nil || fields.CommitSHA == "" || commitSHA == "" {
		return ""
	}
	if fields.CommitSHA != commitSHA {
		return "was for commit " + shortSHA(fields.CommitSHA)
	}
	return ""
}

// resumedWork is what a push checkpoint means for a gt done that has been
// re-invoked on the same branch and commit.
type resumedWork string

const (
	// resumedWorkStale: the checkpoint names another branch or another commit,
	// so it is no evidence about this run's push and the push must run.
	resumedWorkStale resumedWork = "stale"
	// resumedWorkPushed: the checkpoint names this commit, so the push belongs
	// to an earlier run of this session.
	resumedWorkPushed resumedWork = "pushed"
	// resumedWorkLanded: the commit is on the run's merge target, so the work
	// was submitted and the merge deleted origin/<branch> — there is nothing
	// left to submit.
	resumedWorkLanded resumedWork = "landed"
)

// landedOnTargetGit is the single query classifyResumedPush asks, so a test can
// answer it without a remote.
type landedOnTargetGit interface {
	CommitLandedOnTarget(remote, target, commit string) bool
}

// classifyResumedPush decides what a push checkpoint on a re-invoked gt done
// means for the branch the run is about to push.
//
// The landed case exists because the merge that lands a branch deletes it from
// origin: a re-run of gt done the checkpoint then matches would push the branch
// back and read the missing ref as an unlanded push, reporting pushFailed for a
// submission that completed (gt-mik3). A commit on the target is proof of the
// opposite, so it is asked only once the checkpoint has named this commit —
// a stale checkpoint says nothing about where the work is.
func classifyResumedPush(g landedOnTargetGit, remote, target, checkpoint, branch, sha string) resumedWork {
	if !pushedCheckpointMatches(checkpoint, branch, sha) {
		return resumedWorkStale
	}
	if g.CommitLandedOnTarget(remote, target, sha) {
		return resumedWorkLanded
	}
	return resumedWorkPushed
}

// readDoneCheckpoints reads all done-cp:* labels from the agent bead.
// Returns a map of checkpoint stage -> value. Empty map if none found.
func readDoneCheckpoints(bd *beads.Beads, agentBeadID string) map[DoneCheckpoint]string {
	checkpoints := make(map[DoneCheckpoint]string)
	if agentBeadID == "" {
		return checkpoints
	}
	issue, err := bd.Show(agentBeadID)
	if err != nil {
		return checkpoints
	}
	for _, label := range issue.Labels {
		if strings.HasPrefix(label, "done-cp:") {
			// Format: done-cp:<stage>:<value>:<ts>
			parts := strings.SplitN(label, ":", 4)
			if len(parts) >= 3 {
				stage := DoneCheckpoint(parts[1])
				value := parts[2]
				checkpoints[stage] = value
			}
		}
	}
	return checkpoints
}

// clearDoneCheckpoints removes all done-cp:* labels from the agent bead.
// Called on clean exit to prevent stale checkpoints from interfering with future runs.
func clearDoneCheckpoints(bd *beads.Beads, agentBeadID string) {
	if agentBeadID == "" {
		return
	}
	issue, err := bd.Show(agentBeadID)
	if err != nil {
		return
	}
	var toRemove []string
	for _, label := range issue.Labels {
		if strings.HasPrefix(label, "done-cp:") {
			toRemove = append(toRemove, label)
		}
	}
	if len(toRemove) == 0 {
		return
	}
	if err := bd.Update(agentBeadID, beads.UpdateOptions{
		RemoveLabels: toRemove,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear done checkpoints on %s: %v\n", agentBeadID, err)
	}
}

// updateAgentStateOnDone closes the hooked work bead and reports cleanup status.
// Uses issueID directly to find the hooked bead instead of reading the agent bead's
// hook_bead slot (hq-l6mm5: direct bead tracking).
//
// Clean completions use "done" to prevent dead completed sessions from
// re-entering the idle reuse pool before witness/refinery cleanup finishes.
// Escalated/deferred exits use "stuck" because they need recovery.
//
// cleanup_status is NOT written here — notifyWitness self-reports it through
// selfReportCleanupStatus so that failed submissions record it too.
//
// BUG FIX (hq-3xaxy): This function must be resilient to working directory deletion.
// If the polecat's worktree is deleted before gt done finishes, we use env vars as fallback.
// All errors are warnings, not failures - gt done must complete even if bead ops fail.
func updateAgentStateOnDone(cwd, townRoot, exitType, issueID string) error {
	// Get role context - try multiple sources for resilience
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		// Fallback: try to construct role info from environment variables
		// This handles the case where cwd is deleted but env vars are set
		envRole := os.Getenv("GT_ROLE")
		envRig := os.Getenv("GT_RIG")
		envPolecat := os.Getenv("GT_POLECAT")

		if envRole == "" || envRig == "" {
			// Can't determine role, skip agent state update
			style.PrintWarning("could not determine role for agent state update (env: GT_ROLE=%q, GT_RIG=%q)", envRole, envRig)
			return nil
		}

		// Parse role string to get Role type
		parsedRole, _, _ := parseRoleString(envRole)

		roleInfo = RoleInfo{
			Role:     parsedRole,
			Rig:      envRig,
			Polecat:  envPolecat,
			TownRoot: townRoot,
			WorkDir:  cwd,
			Source:   "env-fallback",
		}
	}

	ctx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}

	agentBeadID := getAgentBeadID(ctx)
	if agentBeadID == "" {
		style.PrintWarning("no agent bead ID found for %s/%s, skipping agent state update", ctx.Rig, ctx.Polecat)
		return nil
	}

	// Use rig path for bd commands.
	// IMPORTANT: Use the rig's directory (not polecat worktree) so bd commands
	// work even if the polecat worktree is deleted.
	var beadsPath string
	switch ctx.Role {
	case RoleMayor, RoleDeacon:
		beadsPath = townRoot
	default:
		beadsPath = filepath.Join(townRoot, ctx.Rig)
	}
	bd := beads.New(beadsPath)
	// agentBd resolves agent beads dual-scope: their canonical (rig-local)
	// database first, with a town fallback for legacy beads created before
	// the rig-local migration. See beads.ForAgentBead docstring (gt-8we).
	agentBd := bd.ForAgentBead()

	// Best-effort lookup of the MR this session just submitted (set on the
	// agent bead's active_mr field earlier in the same gt done invocation —
	// see UpdateAgentActiveMR). Used below to leave the hooked bead open and
	// record which MR is outstanding (gt-pqqz) rather than closing it here;
	// the refinery closes it for real at merge success. Empty when this exit
	// had no MR (no-merge, escalated, etc.) — those go through a different
	// close path already.
	var pendingMRID string
	if agentIssue, err := agentBd.Show(agentBeadID); err == nil && agentIssue != nil {
		if fields := beads.ParseAgentFields(agentIssue.Description); fields != nil {
			pendingMRID = strings.TrimSpace(fields.ActiveMR)
		}
	}

	// Find the hooked bead to close. Use issueID directly instead of reading
	// agent bead's hook_bead slot (hq-l6mm5: direct bead tracking).
	hookedBeadID := issueID
	if hookedBeadID == "" {
		// Fallback: query for hooked beads assigned to this agent
		agentID := roleInfo.ActorString()
		if found := findHookedBeadForAgent(bd, agentID); found != "" {
			hookedBeadID = found
		}
	}

	// Workflow step beads (*-wfs-*) are ephemeral formula steps managed by the workflow
	// engine. For these, DEFERRED means "step complete, no code commits" not "work
	// paused for resumption". Close them on DEFERRED so the convoy can advance.
	isWorkflowStep := strings.Contains(hookedBeadID, "-wfs-")

	if hookedBeadID != "" && (exitType != ExitDeferred || isWorkflowStep) {
		// BUG FIX (gt-pftz): Close hooked bead unless already terminal (closed/tombstone).
		// Previously checked hookedBead.Status == StatusHooked, but polecats update
		// their work bead to in_progress during work. The exact-match check caused
		// gt done to skip closing the bead, leaving it as unassigned open work after
		// the hook was cleared — triggering infinite dispatch loops.
		//
		// DEFERRED exits preserve the bead: work is paused, not done. The bead
		// stays open/in_progress so it can be resumed on the next session.
		// Exception: workflow step beads (*-wfs-*) are always closed — see above.
		hookBd, _, _ := routedIssueBeads(beadsPath, hookedBeadID)
		if hookBd == nil {
			hookBd = bd
		}
		if hookedBead, err := hookBd.Show(hookedBeadID); err == nil && !beads.IssueStatus(hookedBead.Status).IsTerminal() {
			// Guard: never close a rig identity bead. Polecats dispatched with the
			// rig bead as their hook (via mol-polecat-work) must not close permanent
			// infrastructure. Skip close and fall through to idle state update.
			if beads.HasLabel(hookedBead, "gt:rig") {
				fmt.Fprintf(os.Stderr, "Note: hooked bead %s is a rig identity bead (gt:rig) — skipping close\n", hookedBeadID)
				goto doneStateUpdate
			}

			if skipReason, fatal := doneSourceCloseSkipReason(hookBd, hookedBeadID, hookedBead); skipReason != "" {
				style.PrintWarning("%s", skipReason)
				fmt.Fprintf(os.Stderr, "  The bead will remain open for witness/mayor review.\n")
				notifyDoneCloseSkipped(townRoot, ctx.Rig, detectSender(), hookedBeadID, skipReason)
				if fatal {
					return fmt.Errorf("cannot complete hooked work: %s", skipReason)
				}
				goto doneStateUpdate
			}

			// BUG FIX: Close attached molecule (wisp) BEFORE closing hooked bead.
			// When using formula-on-bead (gt sling formula --on bead), the base bead
			// has attached_molecule pointing to the wisp. Without this fix, gt done
			// only closed the hooked bead, leaving the wisp orphaned.
			// Order matters: wisp closes -> unblocks base bead -> base bead closes.
			attachment := beads.ParseAttachmentFields(hookedBead)
			if attachment != nil && attachment.AttachedMolecule != "" {
				// Close molecule step descendants before closing the wisp root.
				// bd close doesn't cascade — without this, open/in_progress steps
				// from the molecule stay stuck forever after gt done completes.
				// Order: step children -> wisp root -> base bead.
				if n := closeDescendants(hookBd, attachment.AttachedMolecule); n > 0 {
					fmt.Fprintf(os.Stderr, "Closed %d molecule step(s) for %s\n", n, attachment.AttachedMolecule)
				}

				// Close the wisp root with --force and audit reason.
				// ForceCloseWithReason handles any status (hooked, open, in_progress)
				// and records the reason + session for attribution.
				// Same pattern as gt mol burn/squash (#1879).
				if closeErr := hookBd.ForceCloseWithReason("done", attachment.AttachedMolecule); closeErr != nil {
					if !errors.Is(closeErr, beads.ErrNotFound) {
						fmt.Fprintf(os.Stderr, "Warning: couldn't close attached molecule %s: %v\n", attachment.AttachedMolecule, closeErr)
						// Don't try to close hookedBeadID - it may still be blocked.
						// But DO clear hooks and update agent state (goto doneStateUpdate)
						// so the polecat isn't stuck in 'working' state (za-o9e).
						goto doneStateUpdate
					}
					// Not found = already burned/deleted by another path, continue
				}
			}

			// Acceptance criteria gate: skip close if criteria are unchecked.
			if unchecked := beads.HasUncheckedCriteria(hookedBead); unchecked > 0 {
				style.PrintWarning("hooked bead %s has %d unchecked acceptance criteria — skipping close", hookedBeadID, unchecked)
				fmt.Fprintf(os.Stderr, "  The bead will remain open for witness/mayor review.\n")
			} else if skipReason := doneCloseTimeInvariantSkipReason(bd, cwd, townRoot, ctx.Rig, hookedBeadID, pendingMRID); skipReason != "" {
				// gt-6hmz: this routine self-close previously trusted a cached
				// active_mr field without re-verifying the MR was still open.
				// Refuse rather than close a bead whose branch carries unmerged
				// commits with nothing tracking them.
				style.PrintWarning("%s", skipReason)
				fmt.Fprintf(os.Stderr, "  The bead will remain open for witness/mayor review.\n")
				notifyDoneCloseSkipped(townRoot, ctx.Rig, detectSender(), hookedBeadID, skipReason)
			} else if pendingMRID != "" {
				// gt-pqqz: the source bead stays open through the merge queue
				// instead of closing here at MR-submission time. "Closed" now
				// means "merged" everywhere a human or a dependency check
				// reads it — the refinery's closeMergedWorkBead
				// (work_bead_close.go) is what actually closes this bead, at
				// real merge success, referencing the merge commit. A comment
				// records the outstanding MR for anyone reading the bead
				// while it's in flight.
				//
				// If the MR is rejected instead, recoverRejectedMRDeadWorker
				// (dead_worker_recovery.go) reopens the bead for redispatch
				// once this (transient) polecat's session is confirmed gone —
				// it already treats a still-open, still-assigned source bead
				// as one of its cases, gated on tmux session liveness rather
				// than on close-reason vocabulary.
				attempt := 1 + strings.Count(hookedBead.Notes, refinery.MergeRejectionNoteMarker+" (attempt")
				note := fmt.Sprintf("Submitted to merge queue: %s (attempt %d)", pendingMRID, attempt)
				if err := hookBd.AddComment(hookedBeadID, note); err != nil {
					// Non-fatal: warn but continue
					fmt.Fprintf(os.Stderr, "Warning: couldn't record pending-MR note on %s: %v\n", hookedBeadID, err)
				}
			} else if err := hookBd.Close(hookedBeadID); err != nil {
				// Non-fatal: warn but continue
				fmt.Fprintf(os.Stderr, "Warning: couldn't close hooked bead %s: %v\n", hookedBeadID, err)
			}
		}
	}

doneStateUpdate:
	// Clear hook_bead on the agent bead (gt-qbh). The hq-l6mm5 refactor made
	// SetHookBead/ClearHookBead no-ops, but the witness still reads the
	// hook_bead field from the agent bead snapshot. If the hooked bead is a
	// wisp that gets reaped, the witness can't verify it was closed and flags
	// the polecat as a zombie. Clearing hook_bead prevents this false positive.
	emptyHook := ""
	if err := agentBd.UpdateAgentDescriptionFields(agentBeadID, beads.AgentFieldUpdates{HookBead: &emptyHook}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear hook_bead on %s: %v\n", agentBeadID, err)
	}

	// Purge closed ephemeral beads (wisps) accumulated during this and prior sessions.
	// Without this, closed wisps from mol-polecat-work steps, mol-witness-patrol cycles,
	// etc. accumulate across sessions and pollute bd ready/list output (hq-6161m).
	// Best-effort: failures are non-fatal since the work is already done.
	purgeClosedEphemeralBeads(bd, townRoot)

	// Completion metadata (exit_type, MR ID, branch) remains on the agent bead
	// for audit purposes and anomaly detection by witness patrol.
	doneState := string(beads.AgentStateDone)
	if exitType != ExitCompleted {
		doneState = "stuck"
	}
	// Use UpdateAgentState to sync both column and description (gt-ulom).
	if err := agentBd.UpdateAgentState(agentBeadID, doneState); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't set agent %s to %s: %v\n", agentBeadID, doneState, err)
	}

	// ZFC #10 cleanup_status self-report moved to notifyWitness
	// (selfReportCleanupStatus): this function only runs when push and MR
	// submission both succeeded, so a failed submission recorded nothing at
	// all — the exact case where the witness needs "has_unpushed". The report
	// also used to be skipped whenever the status was empty/unknown, leaving
	// cleanup_status=<missing> on a slot that can never be reclaimed.

	// Clear done-intent label and checkpoints on clean exit — gt done completed
	// successfully. If we don't reach here (crash/stuck), the Witness uses the
	// lingering labels to detect the zombie and resume from checkpoints.
	clearDoneIntentLabel(agentBd, agentBeadID)
	clearDoneCheckpoints(agentBd, agentBeadID)
	return nil
}

// ensureAgentBeadExists recreates a missing agent bead so done-intent labels,
// checkpoints, and active_mr writes don't silently fail (hq-xu4p). Only
// rig-level agents are handled — town agents (mayor/deacon) are owned by
// gt doctor. Best-effort: failures are warned, never fatal.
func ensureAgentBeadExists(bd *beads.Beads, id string, ctx RoleContext) {
	if id == "" {
		return
	}
	if issue, err := bd.Show(id); err == nil && issue != nil && issue.Status != string(beads.StatusClosed) {
		return // exists and is active
	}

	fields := &beads.AgentFields{Rig: ctx.Rig, AgentState: "idle"}
	var title string
	switch ctx.Role {
	case RolePolecat:
		fields.RoleType = "polecat"
		title = fmt.Sprintf("Polecat worker %s in %s - autonomous worker with persistent identity.", ctx.Polecat, ctx.Rig)
	case RoleWitness:
		fields.RoleType = "witness"
		title = fmt.Sprintf("Witness for %s - monitors polecat health and progress.", ctx.Rig)
	case RoleRefinery:
		fields.RoleType = "refinery"
		title = fmt.Sprintf("Refinery for %s - processes merge queue.", ctx.Rig)
	default:
		return
	}

	if _, err := bd.CreateOrReopenAgentBead(id, title, fields); err != nil {
		style.PrintWarning("agent bead %s missing and recreate failed: %v", id, err)
	} else {
		fmt.Printf("%s Recreated/reopened missing agent bead: %s\n", style.Bold.Render("✓"), id)
	}
}

// isStaleBranchIssue reports whether a branch-derived issue id should be
// overridden by the agent's hooked bead (hq-l0fj stale-branch guard).
// True when both ids exist, they differ, and the branch id is not a subtask
// of the hooked bead (e.g. branch gt-abc.1 under hooked gt-abc is fine).
func isStaleBranchIssue(branchIssue, hookedIssue string) bool {
	if branchIssue == "" || hookedIssue == "" {
		return false
	}
	return branchIssue != hookedIssue && !strings.HasPrefix(branchIssue, hookedIssue+".")
}

// selectAssignedIssue returns the one authoritative assignment to use for
// done attribution. Ambiguous assignment state is deliberately not guessed.
func selectAssignedIssue(branchIssue string, assigned []string) (string, bool) {
	unique := make(map[string]bool, len(assigned))
	for _, id := range assigned {
		if id != "" {
			unique[id] = true
		}
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	if len(ids) == 0 {
		return "", false
	}
	if branchIssue != "" {
		for _, id := range ids {
			if branchIssue == id || strings.HasPrefix(branchIssue, id+".") {
				return "", false
			}
		}
	}
	if len(ids) > 1 {
		return "", true
	}
	return ids[0], false
}

// findAssignedBeadsForAgent queries the same assignment locations as gt hook:
// the current rig, the target rig for rig agents, then town beads. The assigned
// work bead is authoritative; agent-bead hook slots are intentionally ignored.
func findAssignedBeadsForAgent(workDir, agentID string) []string {
	if agentID == "" {
		return nil
	}

	assigned := assignedIssueIDs(queryAssignedBeads(beads.New(workDir), agentID))
	if len(assigned) > 0 {
		return assigned
	}

	townRoot, err := findTownRoot()
	if err != nil || townRoot == "" {
		return nil
	}

	parts := strings.Split(agentID, "/")
	rigName := ""
	if len(parts) > 0 {
		rigName = parts[0]
	}
	if rigName != "" && rigName != "mayor" && rigName != "deacon" {
		rigWorkDir := filepath.Join(townRoot, rigName, "mayor", "rig")
		if rigWorkDir != workDir {
			assigned = assignedIssueIDs(queryAssignedBeads(beads.New(rigWorkDir), agentID))
			if len(assigned) > 0 {
				return assigned
			}
		}
	}

	townBeadsDir := filepath.Join(townRoot, ".beads")
	if _, err := os.Stat(townBeadsDir); err == nil {
		assigned = assignedIssueIDs(queryAssignedBeads(beads.New(townBeadsDir), agentID))
		if len(assigned) > 0 {
			return assigned
		}
	}
	if isTownLevelRole(agentID) {
		return assignedIssueIDs(scanAllRigsForHookedBeads(townRoot, agentID))
	}
	return nil
}

func queryAssignedBeads(bd *beads.Beads, agentID string) []*beads.Issue {
	hooked, err := bd.List(beads.ListOptions{
		Status:   beads.StatusHooked,
		Assignee: agentID,
		Priority: -1,
	})
	if err == nil && len(hooked) > 0 {
		return hooked
	}
	inProgress, err := bd.List(beads.ListOptions{
		Status:   "in_progress",
		Assignee: agentID,
		Priority: -1,
	})
	if err == nil {
		return inProgress
	}
	return nil
}

func assignedIssueIDs(assigned []*beads.Issue) []string {
	ids := make([]string, 0, len(assigned))
	for _, issue := range assigned {
		if issue != nil && issue.ID != "" {
			ids = append(ids, issue.ID)
		}
	}
	return ids
}

// findHookedBeadForAgent queries for the agent's current assignment bead.
// This is the authoritative source for what work a polecat is doing, since the
// work bead itself tracks status and assignee (hq-l6mm5).
//
// Both hooked AND in_progress are checked (hq-xa4z): polecats routinely claim
// their assignment with `bd update --status=in_progress` when starting work,
// which made a hooked-only lookup blind to the active assignment — the stale-
// branch guard and the hook fallback silently no-op'd (same class of bug as
// gt-pftz in the close path). Hooked wins over in_progress when both exist.
// Returns empty string if no assignment bead is found.
func findHookedBeadForAgent(bd *beads.Beads, agentID string) string {
	issueID, _ := selectAssignedIssue("", assignedIssueIDs(queryAssignedBeads(bd, agentID)))
	return issueID
}

// parseCleanupStatus converts a string flag value to a CleanupStatus.
// ZFC: Agent observes git state and passes the appropriate status.
func parseCleanupStatus(s string) polecat.CleanupStatus {
	switch strings.ToLower(s) {
	case "clean":
		return polecat.CleanupClean
	case "uncommitted", "has_uncommitted":
		return polecat.CleanupUncommitted
	case "stash", "has_stash":
		return polecat.CleanupStash
	case "unpushed", "has_unpushed":
		return polecat.CleanupUnpushed
	default:
		return polecat.CleanupUnknown
	}
}

// isPolecatActor checks if a BD_ACTOR value represents a polecat.
// Polecat actors have format: rigname/polecats/polecatname
// Non-polecat actors have formats like: gastown/crew/name, rigname/witness, etc.
func isPolecatActor(actor string) bool {
	parts := strings.Split(strings.TrimSpace(actor), "/")
	return len(parts) == 3 && parts[0] != "" && parts[1] == "polecats" && parts[2] != ""
}

// stripOverlayCLAUDEmd detects and removes Gas Town overlay content from CLAUDE.md
// and CLAUDE.local.md before the branch is pushed. Polecats were committing the
// overlay (which contains polecat lifecycle boilerplate like "Idle Polecat Heresy",
// "gt done" protocol, etc.) into actual repos, overwriting project-specific CLAUDE.md
// content. (gt-p35)
//
// This runs after all commits but before push. If overlay files are detected in
// the branch diff, they are restored (CLAUDE.md) or removed (CLAUDE.local.md)
// and a cleanup commit is created.
//
// Returns true if a cleanup commit was created.
func stripOverlayCLAUDEmd(g *git.Git, defaultBranch, baseRef string) bool {
	// Check which files changed on this branch vs the clean target base.
	changedFiles, err := g.DiffNameOnly(baseRef, "HEAD")
	if err != nil {
		// Can't determine diff — skip silently (push will still work)
		return false
	}

	claudeChanged := false
	claudeLocalChanged := false
	for _, f := range changedFiles {
		switch f {
		case "CLAUDE.md":
			claudeChanged = true
		case "CLAUDE.local.md":
			claudeLocalChanged = true
		}
	}

	if !claudeChanged && !claudeLocalChanged {
		return false // Nothing to strip
	}

	needsCommit := false

	// Handle CLAUDE.md: check if the committed version contains overlay marker
	if claudeChanged {
		// Read current CLAUDE.md from HEAD
		currentContent, showErr := g.ShowFile("HEAD", "CLAUDE.md")
		if showErr == nil && strings.Contains(currentContent, templates.PolecatLifecycleMarker) {
			// Current CLAUDE.md has overlay content — restore from the clean base.
			origContent, origErr := g.ShowFile(baseRef, "CLAUDE.md")
			if origErr != nil {
				// CLAUDE.md didn't exist on the clean base — the overlay created it.
				// Remove it from tracking.
				if rmErr := g.RmCached("CLAUDE.md"); rmErr == nil {
					needsCommit = true
					fmt.Printf("%s Removed overlay CLAUDE.md (did not exist on %s)\n",
						style.Bold.Render("→"), defaultBranch)
				}
			} else {
				// CLAUDE.md existed on the clean base — restore original content
				_ = origContent // Restore via checkout
				if coErr := g.CheckoutFileFromRef(baseRef, "CLAUDE.md"); coErr == nil {
					if addErr := g.Add("CLAUDE.md"); addErr == nil {
						needsCommit = true
						fmt.Printf("%s Restored original CLAUDE.md (stripped Gas Town overlay)\n",
							style.Bold.Render("→"))
					}
				}
			}
		}
	}

	// Handle CLAUDE.local.md: always remove from commits (it's a runtime artifact)
	if claudeLocalChanged {
		if rmErr := g.RmCached("CLAUDE.local.md"); rmErr == nil {
			needsCommit = true
			fmt.Printf("%s Removed CLAUDE.local.md from branch (Gas Town overlay)\n",
				style.Bold.Render("→"))
		}
	}

	if !needsCommit {
		return false
	}

	// Create cleanup commit
	if commitErr := g.Commit("chore: strip Gas Town overlay from CLAUDE.md (gt-p35)"); commitErr != nil {
		style.PrintWarning("failed to create overlay cleanup commit: %v", commitErr)
		return false
	}

	fmt.Printf("%s Created cleanup commit to remove Gas Town overlay files\n",
		style.Bold.Render("✓"))
	return true
}

// defaultClosedWispDeleteAge is the grace period before a closed ephemeral
// bead becomes eligible for purge, used when no lifecycle.reaper.delete_age
// override is configured. Matches the wisp-reaper patrol's own default
// (internal/daemon/wisp_reaper.go) so the two purge paths agree.
const defaultClosedWispDeleteAge = "168h"

// closedWispDeleteAge returns the configured grace period (lifecycle.reaper.delete_age)
// before a closed ephemeral bead may be purged, falling back to
// defaultClosedWispDeleteAge if unset or invalid.
func closedWispDeleteAge(townRoot string) string {
	cfg := daemon.LoadPatrolConfig(townRoot)
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.WispReaper == nil {
		return defaultClosedWispDeleteAge
	}
	age := cfg.Patrols.WispReaper.DeleteAgeStr
	if age == "" {
		return defaultClosedWispDeleteAge
	}
	if _, err := time.ParseDuration(age); err != nil {
		return defaultClosedWispDeleteAge
	}
	return age
}

// purgeClosedEphemeralBeads removes closed ephemeral beads (wisps) that accumulated
// during this and prior sessions. Polecat/witness sessions create mol-polecat-work
// steps, mol-witness-patrol cycles, etc. as wisps. These get closed during normal
// operation but are never deleted, accumulating hundreds of rows that pollute
// bd ready/list output. (hq-6161m)
//
// An --older-than grace period (gt-1q46) is REQUIRED here: MR beads (label
// gt:merge-request) are also closed ephemeral wisps, and a same-session
// supersede/rejection close (see FindOpenMRsForIssue below) can land just
// moments before this purge runs. Purging unconditionally deletes that MR
// bead outright — destroying its close reason/verdict — instead of leaving
// a "superseded by X" or rejection-verdict record for the next attempt.
//
// Best-effort: errors are logged but don't block gt done completion.
func purgeClosedEphemeralBeads(bd *beads.Beads, townRoot string) {
	olderThan := closedWispDeleteAge(townRoot)
	out, err := bd.Run("purge", "--force", "--quiet", "--older-than", olderThan)
	if err != nil {
		// Non-fatal: purge failure shouldn't block session completion
		fmt.Fprintf(os.Stderr, "Warning: wisp purge failed: %v\n", err)
		return
	}
	// bd purge --force --quiet outputs the count of purged beads
	outStr := strings.TrimSpace(string(out))
	if outStr != "" && outStr != "0" {
		fmt.Fprintf(os.Stderr, "Purged closed ephemeral beads: %s\n", outStr)
	}
}
