package cmd

import (
	"context"
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
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/telemetry"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var doneCmd = &cobra.Command{
	Use:         "done",
	GroupID:     GroupWork,
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Submit your branch for landing and end the polecat session",
	Long: `Submit your finished branch for landing, notify the Witness, and end the
polecat session. gt done never lands anything on the target branch: the
daemon's landing worker does that (ADR 0004).

For COMPLETED, gt done:
1. Fetches origin and rebases the branch onto the target (default: the rig's
   default branch; --target or the bead's base_branch override it)
2. Squashes auto-save and checkpoint commits into one descriptive commit
3. Runs the local gate on the rebased tree: make lint, go build ./...,
   and the unit tier of make test (a rig without go.mod runs its
   lint_command, build_command and test_command)
4. Pushes the branch under a lease and reads the tip back
5. Marks the work bead ready to land (label gt:ready-to-land and a
   READY TO LAND notes block naming branch, head and target)
6. Notifies the Witness and retires the session

There is no way to skip the gate or to land directly.

Exit statuses:
  COMPLETED      - Work done, branch submitted for landing (default)
  ESCALATED      - Hit blocker, needs human intervention
  DEFERRED       - Work paused, issue still open

Process exit codes (the work was not submitted; the Witness is not told
"done" and the session stays up so you can fix it and re-run gt done):
  10  push failed: origin does not have the commit
  11  push unverified: origin is not at the commit gt done would declare
  12  the work bead could not be marked ready to land
  13  the no-code completion could not close the bead
  14  rebase onto the target conflicted
  15  the local gate failed

Examples:
  gt done                              # Submit branch, notify COMPLETED, exit session
  gt done --target feat/my-branch      # Explicit target branch
  gt done --issue gt-abc               # Explicit issue ID
  gt done --status ESCALATED           # Signal blocker, submit nothing
  gt done --status DEFERRED            # Pause work, submit nothing`,
	RunE:         runDone,
	SilenceUsage: true, // Don't print usage on operational errors (confuses agents)
}

var (
	doneIssue         string
	doneStatus        string
	doneCleanupStatus string
	doneTarget        string
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

// shouldRetirePolecatSessionAfterDone reports whether a reported exit retires
// the session. A run that failed never reaches here: it returns its coded
// error before reporting, and the session stays up to fix it.
func shouldRetirePolecatSessionAfterDone(exitType string, fromHandoff bool) bool {
	// A handoff-triggered DEFERRED defers the work mid-task; the polecat (or its
	// successor) is expected to keep going, so it must never be torn down here,
	// regardless of exit type (gt-5g3e).
	if fromHandoff {
		return false
	}
	// A polecat that has signaled a final status and stays alive keeps spending
	// tokens on work it already reported finished (gt-5g3e).
	return isFinalDoneExitType(exitType)
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

// doneLocalGate is gt done's pre-submit gate: the same land.Gate seam Land()
// runs on the merged tree, here in its unit tier (no container slot) on the
// rebased branch. D9's `make gate` replaces the steps, not the seam. Tests
// replace this variable.
var doneLocalGate = func(townRoot, rigName, dir string) (land.Gate, error) {
	g, err := land.RigGate(dir, rig.ResolveMergeQueueConfig(townRoot, rigName), true)
	if err != nil {
		return nil, err
	}
	g.Out = os.Stdout
	g.LogDir = filepath.Join(dir, constants.DirRuntime, "done-gate")
	return g, nil
}

// doneLocalGateBudget bounds the whole local gate. The lint step's lock
// retries need a deadline to plan against (lintlock.RoomForRetry).
const doneLocalGateBudget = 60 * time.Minute

func resolveDonePolecatWorktree() (donePolecatWorktree, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return donePolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable: %w", err)
	}
	return resolveDonePolecatWorktreeAt(cwd)
}

func resolveDonePolecatWorktreeAt(cwd string) (donePolecatWorktree, error) {
	return resolveDonePolecatWorktreeIn(cwd, os.Getenv)
}

// resolveDonePolecatWorktreeIn is resolveDonePolecatWorktreeAt reading the
// session's identity and town root through getenv.
func resolveDonePolecatWorktreeIn(cwd string, getenv func(string) string) (donePolecatWorktree, error) {
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
	if err := doneValidateSessionTownRoot(townRoot, getenv); err != nil {
		return donePolecatWorktree{}, err
	}

	actorRig, actorName, err := donePolecatActorIdentity(getenv("BD_ACTOR"))
	if err != nil {
		return donePolecatWorktree{}, err
	}
	roleRig, roleName, err := donePolecatEnvIdentity(getenv("GT_ROLE"), getenv("GT_RIG"), getenv("GT_POLECAT"))
	if err != nil {
		return donePolecatWorktree{}, err
	}
	if actorRig != roleRig || actorName != roleName {
		return donePolecatWorktree{}, fmt.Errorf("gt done identity mismatch: BD_ACTOR=%s/polecats/%s but GT_ROLE/GT_RIG/GT_POLECAT resolve to %s/polecats/%s", actorRig, actorName, roleRig, roleName)
	}
	if err := doneRejectGitEnvOverrides(getenv); err != nil {
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

func doneValidateSessionTownRoot(townRoot string, getenv func(string) string) error {
	current := doneCanonicalPath(townRoot)
	for _, envName := range []string{"GT_TOWN_ROOT", "GT_ROOT"} {
		envRoot := strings.TrimSpace(getenv(envName))
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

func doneRejectGitEnvOverrides(getenv func(string) string) error {
	for _, envName := range []string{
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE",
	} {
		if strings.TrimSpace(getenv(envName)) != "" {
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
func retirePolecatSessionAfterFinalExit(exitType string, fromHandoff bool, rigName, polecatName string, pid int) bool {
	if !shouldRetirePolecatSessionAfterDone(exitType, fromHandoff) {
		fmt.Printf("%s Session preserved for handoff continuation\n", style.Bold.Render("→"))
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
//   - A failed submission returns before reportDone, so runDone records the
//     status on that path too. A failed push is exactly the case where the
//     witness needs to see "has_unpushed".
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
	doneCmd.Flags().StringVar(&doneStatus, "status", ExitCompleted, "Exit status: COMPLETED, ESCALATED, or DEFERRED")
	doneCmd.Flags().StringVar(&doneCleanupStatus, "cleanup-status", "", "Git cleanup status: clean, uncommitted, unpushed, stash, unknown (ZFC: agent-observed)")
	doneCmd.Flags().StringVar(&doneTarget, "target", "", "Explicit target branch (overrides the bead's base_branch and the rig default)")
	doneCmd.Flags().BoolVar(&doneAllowReverts, "allow-reverts", false, "Submit a branch that undoes content already merged to the target (refused by default)")
	doneCmd.Flags().BoolVar(&doneAllowThrowawayPaths, "allow-throwaway-paths", false, "Submit a branch that adds scratch, backup or /tmp files to the target (refused by default)")

	rootCmd.AddCommand(doneCmd)
}

// doneRun is what one gt done invocation knows about itself once its
// identity, branch and issue are resolved.
type doneRun struct {
	g                *git.Git
	cwd              string
	townRoot         string
	rigName          string
	polecatName      string
	sender           string
	branch           string
	issueID          string
	agentBeadID      string
	defaultBranch    string
	heartbeatSession string
}

// doneSubmission is what a COMPLETED run handed over.
type doneSubmission struct {
	sourceIssue *beads.Issue
	sourceBD    *beads.Beads
	// head and target are set when a branch was submitted for landing.
	head   string
	target string
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

	worktree, err := resolveDonePolecatWorktree()
	if err != nil {
		return err
	}
	r := &doneRun{
		townRoot:    worktree.townRoot,
		cwd:         worktree.cwd,
		rigName:     worktree.rigName,
		polecatName: worktree.polecatName,
		sender:      worktree.actor,
	}
	r.g = git.NewGit(r.cwd)

	r.branch, err = r.g.CurrentBranch()
	if err != nil {
		return fmt.Errorf("getting current branch: %w", err)
	}
	if err := requireRealCurrentBranch(r.branch, "gt done"); err != nil {
		return err
	}

	// Auto-detect cleanup status if not explicitly provided
	// This prevents premature polecat cleanup by ensuring witness knows git state
	if doneCleanupStatus == "" {
		doneCleanupStatus = observeCleanupStatus(r.g, r.branch)
	}
	if doneCleanupStatus == "stash" {
		popBranchStashes(r.g)
	}
	if doneCleanupStatus == "uncommitted" {
		if err := autoSaveUncommittedWork(r.g, r.cwd, r.branch); err != nil {
			return err
		}
	}

	info := parseBranchName(r.branch)
	r.issueID = doneIssue
	if r.issueID == "" {
		r.issueID = info.Issue
	}

	// Get agent bead ID for cross-referencing.
	ctx, actorID := resolveDoneAgentIdentity(r.cwd, r.townRoot, r.rigName, r.polecatName)
	if actorID != "" {
		r.sender = actorID
	}
	r.agentBeadID = getAgentBeadID(ctx)

	// Recreate the agent bead if it's missing (hq-xu4p). Done-intent labels
	// and completion metadata write to it; when it's gone every write fails
	// 'issue not found' and witness zombie detection silently degrades.
	ensureAgentBeadExists(beads.New(r.cwd).ForAgentBead(), r.agentBeadID, ctx)
	var assignedIssueIDs []string
	loadAssignedIssueIDs := func() []string {
		if assignedIssueIDs == nil && r.sender != "" {
			assignedIssueIDs = findAssignedBeadsForAgent(r.cwd, r.sender)
		}
		return assignedIssueIDs
	}

	// If issue ID not set by flag or branch name, query for hooked beads
	// assigned to this agent (hq-l6mm5: direct bead tracking).
	if r.issueID == "" && r.sender != "" {
		if hookIssue, ambiguous := selectAssignedIssue("", loadAssignedIssueIDs()); hookIssue != "" {
			r.issueID = hookIssue
		} else if ambiguous {
			return fmt.Errorf("multiple active assignments found for %s; cannot infer issue from hook. Use --issue to disambiguate", r.sender)
		}
	}

	// Stale-branch guard (hq-l0fj): a redispatched polecat that reuses its
	// previous work branch carries the OLD bead-id in the branch name. When the
	// branch-derived id differs from the hooked bead, trust the hook. An
	// explicit --issue flag still wins, and subtask branches of the hooked bead
	// (e.g. gt-abc.1 under hooked gt-abc) are left alone.
	if doneIssue == "" && info.Issue != "" && r.sender != "" {
		if hookIssue, ambiguous := selectAssignedIssue(info.Issue, loadAssignedIssueIDs()); isStaleBranchIssue(info.Issue, hookIssue) {
			style.PrintWarning("branch %q embeds issue %s but your hooked bead is %s — submitting for %s (stale branch reuse?)", r.branch, info.Issue, hookIssue, hookIssue)
			fmt.Printf("  Fresh branches must be named polecat/<name>/<bead-id>+<suffix> for the bead you are working.\n")
			fmt.Printf("  Use --issue to override if the branch-derived id is actually correct.\n\n")
			r.issueID = hookIssue
		} else if ambiguous {
			return fmt.Errorf("branch %q embeds issue %s but %s has multiple active assignments; use --issue to disambiguate", r.branch, info.Issue, r.sender)
		}
	}

	// Write the done-intent label before the long stages, so the Witness can
	// tell a polecat that died inside gt done from one still working. A run
	// that fails does not report done and keeps its session to fix the
	// failure, so the label must not outlive it: a stale done-intent label
	// gets a working polecat restarted (gt-wmpy). Every error return is such a
	// run: reportDone's only error comes before the Witness nudge. A run that
	// succeeds leaves the label to updateAgentStateOnDone, which clears it last.
	if r.agentBeadID != "" {
		agentBd := beads.New(r.cwd).ForAgentBead()
		setDoneIntentLabel(agentBd, r.agentBeadID, exitType)
		defer func() {
			if retErr != nil {
				clearDoneIntentLabel(agentBd, r.agentBeadID)
			}
		}()
	}

	// Write heartbeat state="exiting" (gt-3vr5: heartbeat v2): the witness
	// trusts the agent until the heartbeat goes stale.
	r.heartbeatSession = os.Getenv("GT_SESSION")
	if r.heartbeatSession != "" && r.townRoot != "" {
		polecat.TouchSessionHeartbeatWithState(r.townRoot, r.heartbeatSession, polecat.HeartbeatExiting, "gt done", r.issueID)
	}

	r.defaultBranch = "main" // fallback
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(r.townRoot, r.rigName)); err == nil && rigCfg.DefaultBranch != "" {
		r.defaultBranch = rigCfg.DefaultBranch
	}

	var sub doneSubmission
	if exitType == ExitCompleted {
		sub, err = submitForLanding(r)
		if err != nil {
			// Nothing is reported done, but the worktree's git state is still
			// recorded: a session that dies before re-running gt done must not
			// strand its slot (hq-vx224).
			selfReportCleanupStatus(r.g, r.branch, beads.New(filepath.Join(r.townRoot, r.rigName)).ForAgentBead(), r.agentBeadID, doneCleanupStatus)
			return err
		}
	} else {
		fmt.Printf("%s Signaling %s\n", style.Bold.Render("→"), exitType)
		if r.issueID != "" {
			fmt.Printf("  Issue: %s\n", r.issueID)
		}
		fmt.Printf("  Branch: %s\n", r.branch)
	}
	return reportDone(r, exitType, sub)
}

// popBranchStashes pops this branch's stashes oldest first so the auto-save
// below commits their contents (gt-pvx stash recovery). Agents have been
// observed running `git stash` before a rebase and dying before
// `git stash pop`; popping on the way out turns a lost stash into a commit. A
// conflicting pop stops the chain: surfacing the conflict beats silently
// dropping a stash.
func popBranchStashes(g *git.Git) {
	entries, err := g.StashListForBranch()
	if err != nil {
		style.PrintWarning("auto-pop: could not list stashes: %v — orphaned stashes may remain", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	fmt.Printf("\n%s %d stash(es) detected on this branch — auto-popping (gt-pvx safety net)\n",
		style.Bold.Render("⚠"), len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		fmt.Printf("  popping %s — %s\n", e.Ref, e.Message)
		if popErr := g.StashPop(e.Ref); popErr != nil {
			style.PrintWarning("auto-pop %s failed (likely conflict): %v", e.Ref, popErr)
			style.PrintWarning("stopping pop chain — resolve conflict manually then re-run gt done")
			return
		}
		// After each pop, stash refs shift; re-fetch the list before next pop.
		entries, err = g.StashListForBranch()
		if err != nil || len(entries) == 0 {
			break
		}
	}
	if workStatus, wsErr := g.CheckUncommittedWork(); wsErr == nil && workStatus.HasUncommittedChanges {
		doneCleanupStatus = "uncommitted"
		fmt.Printf("%s Stash content moved to working tree — will auto-commit below.\n", style.Bold.Render("✓"))
	} else {
		// Pops succeeded but produced nothing dirty; recompute normally.
		doneCleanupStatus = ""
	}
}

// autoSaveUncommittedWork commits uncommitted work before any exit path
// (gt-pvx): polecats have run gt done without committing thousands of lines,
// then died. The commit is marked as an auto-save, runtime and overlay files
// and throwaway files are left out, and deletions of tracked files are never
// committed. Only unmerged conflicts refuse.
func autoSaveUncommittedWork(g *git.Git, cwd, branch string) error {
	workStatus, err := g.CheckUncommittedWork()
	if err != nil || !workStatus.HasUncommittedChanges || workStatus.CleanExcludingRuntime() {
		return nil
	}
	if len(workStatus.UnmergedFiles) > 0 {
		return fmt.Errorf("cannot auto-save unmerged conflicts: %s\nResolve conflicts first, or use --status DEFERRED to exit without completing", strings.Join(workStatus.UnmergedFiles, ", "))
	}

	fmt.Printf("\n%s Uncommitted changes detected — auto-saving to prevent work loss\n", style.Bold.Render("⚠"))
	fmt.Printf("  Files: %s\n\n", workStatus.String())

	if addErr := g.Add("-A"); addErr != nil {
		style.PrintWarning("auto-commit: git add failed: %v — uncommitted work may be at risk", addErr)
		return nil
	}
	// Unstage Gas Town overlay files that git add -A picked up (gt-p35).
	_ = g.ResetFiles("CLAUDE.local.md")
	if claudeData, readErr := os.ReadFile(filepath.Join(cwd, "CLAUDE.md")); readErr == nil {
		if strings.Contains(string(claudeData), templates.PolecatLifecycleMarker) {
			_ = g.ResetFiles("CLAUDE.md")
		}
	}
	for _, path := range workStatus.RuntimeArtifactPaths() {
		_ = g.ResetFiles(path)
	}
	// Throwaway files stay untracked and are named (gt-ozo4).
	if throwaway := checkpoint.ThrowawayPaths(workStatus.UntrackedFiles); len(throwaway) > 0 {
		_ = g.ResetFiles(throwaway...)
		style.PrintWarning("auto-commit: left %d throwaway file(s) uncommitted: %s",
			len(throwaway), strings.Join(throwaway, ", "))
	}
	// A safety-net commit preserves work and never destroys it.
	if stagedDeletions, delErr := g.StagedDeletions(); delErr == nil && len(stagedDeletions) > 0 {
		_ = g.ResetFiles(stagedDeletions...)
	}
	autoMsg := "fix: auto-save uncommitted implementation work (gt-pvx safety net)"
	if issueFromBranch := parseBranchName(branch).Issue; issueFromBranch != "" {
		autoMsg = fmt.Sprintf("fix: auto-save uncommitted implementation work (%s, gt-pvx safety net)", issueFromBranch)
	}
	if commitErr := g.Commit(autoMsg); commitErr != nil {
		style.PrintWarning("auto-commit: git commit failed: %v — uncommitted work may be at risk", commitErr)
		return nil
	}
	fmt.Printf("%s Auto-committed uncommitted work (safety net)\n", style.Bold.Render("✓"))
	fmt.Printf("  The agent should have committed before running gt done.\n")
	fmt.Printf("  This auto-save prevents work loss.\n\n")
	doneCleanupStatus = "unpushed"
	return nil
}

// submitForLanding is the COMPLETED path: rebase, squash, gate, push and mark the
// work bead ready to land. It never lands anything. Every failure returns
// before the Witness is told anything.
func submitForLanding(r *doneRun) (doneSubmission, error) {
	var sub doneSubmission
	if r.branch == r.defaultBranch || r.branch == "master" {
		// A conflict-resolution pass ends on the base branch by design: its
		// work is a rewritten head already pushed to the branch of an existing
		// MR (mol-polecat-conflict-resolve). It submits nothing of its own
		// (gt-rv8h, gt-tne1). Deleted with the merge queue (gt-v4ssj.6).
		if task := conflictResolutionCompletionTask(r.cwd, r.agentBeadID, r.issueID); task != nil {
			if !beads.IssueStatus(task.Status).IsTerminal() {
				return sub, fmt.Errorf("cannot complete %s: conflict-resolution task %s is still open, and its MR stays blocked until it closes\nClose it first: bd close %s --reason=\"resolved conflicts\"",
					r.defaultBranch, task.ID, task.ID)
			}
			fmt.Printf("%s Conflict-resolution completion on %s — no branch of its own to submit\n", style.Bold.Render("→"), r.defaultBranch)
			fmt.Printf("  %s is closed; the refinery wake below names the MR it released.\n", task.ID)
			return sub, nil
		}
		return sub, fmt.Errorf("cannot submit the %s/master branch for landing", r.defaultBranch)
	}

	// Refuse uncommitted changes (hq-xthqf): they would be lost. Runtime
	// artifacts (.claude/, .beads/, .runtime/ ...) are toolchain-managed and
	// excluded.
	workStatus, err := r.g.CheckUncommittedWork()
	if err != nil {
		return sub, fmt.Errorf("checking git status: %w", err)
	}
	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
		return sub, fmt.Errorf("cannot complete: uncommitted changes would be lost\nCommit your changes first, or use --status DEFERRED to exit without completing\nUncommitted: %s", workStatus.String())
	}

	// no_merge / review_only are non-code tasks where zero commits is
	// expected (GH#2496, gt-kvf); read them before the zero-commit guard.
	isNoMergeTask := false
	reviewOnlySource := false
	if r.issueID != "" {
		sourceInfo, sourceErr := resolveSubmitSourceIssue(r.cwd, r.issueID)
		if sourceErr != nil {
			return sub, fmt.Errorf("source issue validation failed: %w", sourceErr)
		}
		sub.sourceIssue = sourceInfo.Issue
		sub.sourceBD = sourceInfo.BD
		if af := beads.ParseAttachmentFields(sub.sourceIssue); af != nil {
			isNoMergeTask = af.NoMerge || af.ReviewOnly
			reviewOnlySource = af.ReviewOnly
		}
	}

	target, err := resolveDoneTarget(r, sub.sourceIssue)
	if err != nil {
		return sub, err
	}
	// In fork-backed rigs the clean base is upstream/<target>, never the
	// fork's origin/<target>.
	baseRef := r.g.CleanBaseRef("origin", r.defaultBranch, target)
	fetchRemote := git.RemoteForRef(baseRef)
	if fetchRemote == "" {
		fetchRemote = "origin"
	}
	if err := r.g.Fetch(fetchRemote); err != nil {
		return sub, fmt.Errorf("fetching %s before rebasing onto %s: %w", fetchRemote, baseRef, err)
	}

	aheadCount, err := r.g.CommitsAhead(baseRef, "HEAD")
	if err != nil {
		return sub, fmt.Errorf("counting commits ahead of %s: %w", baseRef, err)
	}
	if aheadCount == 0 {
		return sub, completeWithoutCode(r, sub, baseRef, isNoMergeTask)
	}
	if reviewOnlySource {
		return sub, fmt.Errorf("cannot complete review-only issue %s with commits ahead of %s; add a fresh review evidence comment and complete without code changes", r.issueID, baseRef)
	}
	if r.issueID == "" {
		return sub, fmt.Errorf("cannot determine source issue from branch '%s'; use --issue to specify", r.branch)
	}

	if err := rebaseOntoTarget(r.g, baseRef); err != nil {
		return sub, err
	}

	// Refuse a branch that reverts work already merged to the target
	// (gt-63sz). After the rebase: a rebase replays the same diff, so the
	// check must see the branch as it will be pushed.
	if doneAllowReverts {
		style.PrintWarning("skipping merged-work revert check (--allow-reverts): the branch may undo work merged to %s", baseRef)
	} else if err := reportRevertedMerges(r.g, baseRef); err != nil {
		return sub, err
	}
	// Refuse a branch that would add throwaway files to the target (gt-ozo4).
	if doneAllowThrowawayPaths {
		style.PrintWarning("skipping throwaway-file check (--allow-throwaway-paths): the branch may add scratch files to %s", baseRef)
	} else if err := reportThrowawayPaths(r.g, baseRef); err != nil {
		return sub, err
	}
	// Refuse a rework byte-identical to a rejected attempt (gt-0jzd5).
	var sourceNotes string
	if sub.sourceIssue != nil {
		sourceNotes = sub.sourceIssue.Notes
	}
	rejectedTip := func(mrID string) (string, bool) { return rejectedTipFromMR(sub.sourceBD, mrID) }
	if err := reportUnchangedSinceRejection(r.g, sourceNotes, r.issueID, baseRef, rejectedTip); err != nil {
		return sub, err
	}

	if err := squashAutoSaveBeforeSubmit(r.g, r.cwd, r.branch, baseRef, sub.sourceIssue, r.issueID); err != nil {
		return sub, err
	}
	// Strip Gas Town overlay from CLAUDE.md / CLAUDE.local.md (gt-p35).
	stripOverlayCLAUDEmd(r.g, r.defaultBranch, baseRef)

	head, err := r.g.Rev("HEAD")
	if err != nil {
		return sub, fmt.Errorf("resolving HEAD: %w", err)
	}
	if err := runDoneLocalGate(r, head); err != nil {
		return sub, err
	}

	// Push submodule commits first, so the parent's pointer never names a
	// commit the submodule's remote lacks (gt-dzs).
	pushSubmoduleChanges(r.g, baseRef)
	if err := pushBranchForLanding(r, sub.sourceBD, head, baseRef); err != nil {
		return sub, err
	}
	doneCleanupStatus = cleanupStatusAfterSuccessfulPush(doneCleanupStatus)

	work := land.Work{BeadID: r.issueID, Rig: r.rigName, Branch: r.branch, Head: head, Target: target, Worker: r.polecatName}
	if err := markReadyToLand(sub.sourceBD, work); err != nil {
		return sub, doneExit(doneExitReadyFailed, fmt.Sprintf("branch %s is on origin at %s but the work bead could not be marked ready to land", r.branch, shortSHA(head)), err)
	}
	sub.head, sub.target = head, target

	fmt.Printf("%s Submitted for landing\n", style.Bold.Render("✓"))
	fmt.Printf("  Branch: %s @ %s\n", r.branch, shortSHA(head))
	fmt.Printf("  Target: %s\n", target)
	fmt.Printf("  Issue:  %s\n", r.issueID)
	fmt.Printf("  Worker: %s\n\n", r.polecatName)
	fmt.Printf("%s\n", style.Dim.Render("The daemon's landing worker merges it after gating the merged tree."))
	return sub, nil
}

// resolveDoneTarget picks the branch to land on: --target, then the bead's
// formula_vars base_branch, then the rig default. resolveMRTarget refuses a
// self-target and an unexplained polecat/* target (gt-a8i3, gt-w2jc).
func resolveDoneTarget(r *doneRun, source *beads.Issue) (string, error) {
	target := r.defaultBranch
	explicit := false
	if doneTarget != "" {
		target = doneTarget
		explicit = true
		fmt.Printf("  Target branch: %s (from --target flag)\n", target)
	} else if source != nil {
		if af := beads.ParseAttachmentFields(source); af != nil {
			if bb := extractFormulaVar(af.FormulaVars, "base_branch"); bb != "" && bb != r.defaultBranch {
				target = bb
				fmt.Printf("  Target branch override: %s (from formula_vars)\n", target)
			}
		}
	}
	return resolveMRTarget(target, r.branch, r.defaultBranch, explicit)
}

// completeWithoutCode finishes a run whose branch has nothing ahead of the
// target: report-only, no_merge and review_only work, or a fix that already
// landed. It closes the bead because no landing will; it pushes nothing.
// Polecats must have at least one commit unless the work is non-code
// (gastown#1484). The error text must not mention --cleanup-status=clean:
// agents read errors and self-bypass.
func completeWithoutCode(r *doneRun, sub doneSubmission, baseRef string, isNoMergeTask bool) error {
	if os.Getenv("GT_POLECAT") != "" && doneCleanupStatus != "clean" && !isNoMergeTask {
		// A branch already pushed with its work whose target has since moved
		// on is not empty-handed (GH#wd7).
		pushed, unpushed, pushErr := r.g.BranchPushedToRemote(r.branch, "origin")
		if pushErr != nil || !pushed || unpushed != 0 {
			return fmt.Errorf("cannot complete: no commits on branch ahead of %s\n"+
				"Polecats must have at least 1 commit to submit.\n"+
				"If the bug was already fixed upstream: gt done --status DEFERRED\n"+
				"If you're blocked: gt done --status ESCALATED",
				baseRef)
		}
	}

	fmt.Printf("%s Branch has no commits ahead of %s\n", style.Bold.Render("→"), baseRef)
	fmt.Printf("  Work was likely already landed or report-only; nothing to submit.\n\n")
	if r.issueID == "" {
		return nil
	}
	bd := sub.sourceBD
	if bd == nil {
		bd = beads.New(r.cwd)
	}
	if skipReason, fatal := doneSourceCloseSkipReason(bd, r.issueID, sub.sourceIssue); skipReason != "" {
		style.PrintWarning("%s", skipReason)
		fmt.Printf("  The bead will remain open for witness/mayor review.\n")
		notifyDoneCloseSkipped(r.townRoot, r.rigName, r.sender, r.issueID, skipReason)
		if fatal {
			return fmt.Errorf("cannot complete review-only/no-code work: %s", skipReason)
		}
		return nil
	}

	closeReason := "Completed with no code changes (already fixed or already landed)"
	if !isNoMergeTask {
		if r.g.ForkBackedRemote("origin") {
			return fmt.Errorf("cannot close no-code bead in fork/upstream mode: %s has no commits ahead of %s; use the fork PR flow instead", r.branch, baseRef)
		}
		headSHA, _ := r.g.Rev("HEAD")
		if verifyErr := r.g.VerifyPushedCommitReachableFromPushTarget("origin", r.defaultBranch, headSHA); verifyErr != nil {
			noteVerifiedPushFailure(bd, r.cwd, r.issueID, r.defaultBranch, headSHA, verifyErr)
			return fmt.Errorf("cannot close no-code bead: %w", verifyErr)
		}
		if headSHA != "" {
			closeReason = fmt.Sprintf("%s\ntarget_branch: %s\ncommit_sha: %s", closeReason, r.defaultBranch, headSHA)
		}
	}
	// Force-close bypasses molecule dependency checks; the retry absorbs
	// transient Dolt lock contention (A2).
	if closeErr := forceCloseIssueWithRetry(bd.ForceCloseWithReason, r.issueID, closeReason, "Issue %s closed (no code to land)"); closeErr != nil {
		return doneExit(doneExitCloseFailed, fmt.Sprintf("could not close issue %s after 3 attempts", r.issueID), closeErr)
	}
	return nil
}

// rebaseOntoTarget rebases the branch onto baseRef when it is behind. A
// conflict aborts the rebase, leaving the branch as it was, and exits 14
// naming the conflicting files.
func rebaseOntoTarget(g *git.Git, baseRef string) error {
	behind, err := g.CommitsAhead("HEAD", baseRef)
	if err != nil {
		return fmt.Errorf("counting commits behind %s: %w", baseRef, err)
	}
	if behind == 0 {
		return nil
	}
	fmt.Printf("→ Rebasing onto %s (%d commit(s) behind)\n", baseRef, behind)
	if rebaseErr := g.Rebase(baseRef); rebaseErr != nil {
		files, _ := g.GetConflictingFiles()
		_ = g.AbortRebase()
		return doneExit(doneExitRebaseConflict,
			fmt.Sprintf("rebase onto %s conflicts in %s; resolve it (git fetch origin && git rebase %s), commit the resolution, then re-run gt done",
				baseRef, strings.Join(files, ", "), baseRef), rebaseErr)
	}
	fmt.Printf("%s Branch rebased onto %s\n", style.Bold.Render("✓"), baseRef)
	return nil
}

// squashAutoSaveBeforeSubmit folds machine-generated commits (the gt-pvx
// safety net, the checkpoint dog) into one commit named after the work
// (gt-3wf), so none reaches the target (gt-iki6). The branch is pushed under
// a lease afterwards, so a branch origin already has is rewritten too.
func squashAutoSaveBeforeSubmit(g *git.Git, cwd, branch, baseRef string, issue *beads.Issue, issueID string) error {
	tip, tipErr := checkpoint.InspectAutoSaveTip(cwd, baseRef, "HEAD")
	headBefore, headBeforeErr := g.Rev("HEAD")
	squashed, squashErr := checkpoint.SquashAutoSaveCommits(cwd, baseRef, autoSaveSquashTitle(issue, issueID))
	if squashErr != nil {
		// A squash that failed after its soft reset leaves the branch
		// holding no commits at all; refuse before anything pushes it.
		if headAfter, revErr := g.Rev("HEAD"); headBeforeErr == nil && revErr == nil && headAfter != headBefore {
			return autoSaveSquashResetError(branch, baseRef, squashErr)
		}
		if tipErr == nil && tip.AutoSave {
			return autoSaveTipRefusalError(tip, branch, fmt.Sprintf("the squash failed: %v.", squashErr))
		}
		style.PrintWarning("could not rewrite auto-save commit messages: %v (submitting as-is)", squashErr)
		return nil
	}
	if squashed > 0 {
		fmt.Printf("%s Squashed %d auto-save/WIP commit(s) into a single descriptive commit\n", style.Bold.Render("✓"), squashed)
	}
	return nil
}

// runDoneLocalGate runs the local pre-submit gate on the rebased tree at
// head. A red gate exits 15 before anything is pushed.
func runDoneLocalGate(r *doneRun, head string) error {
	gate, err := doneLocalGate(r.townRoot, r.rigName, r.cwd)
	if err != nil {
		return doneExit(doneExitGateFailed, "no local gate to run", err)
	}
	fmt.Printf("→ Running the local gate on %s\n", shortSHA(head))
	// The gate can run for many minutes; keep the exiting heartbeat fresh so
	// no consumer reads this polecat as hung (gt-azmw).
	stopHeartbeat := polecat.StartExitingHeartbeatKeepAlive(r.townRoot, r.heartbeatSession, "gt done", r.issueID)
	ctx, cancel := context.WithTimeout(context.Background(), doneLocalGateBudget)
	res := gate.Run(ctx, r.cwd)
	cancel()
	stopHeartbeat()
	if res.Err != nil {
		return doneExit(doneExitGateFailed, "the local gate could not run: "+res.Summary(), res.Err)
	}
	if !res.Passed {
		return doneExit(doneExitGateFailed, fmt.Sprintf("the local gate failed on %s: %s\n%s", shortSHA(head), res.Summary(), res.FailureTail()), nil)
	}
	fmt.Printf("%s Local gate passed: %s\n", style.Bold.Render("✓"), res.Summary())
	return nil
}

// pushBranchForLanding pushes head to origin/<branch> under a lease on the
// tip origin had, then asserts origin holds exactly head (gt-2wqt). A rebased
// or squashed branch replaces an earlier attempt's tip; a concurrent push to
// the branch makes the lease fail instead of being clobbered. One retry
// absorbs a push that errored while origin took the objects (gt-0opm).
func pushBranchForLanding(r *doneRun, sourceBD *beads.Beads, head, baseRef string) error {
	fmt.Printf("Pushing branch to origin...\n")
	var lastPushErr error
	attempt := func() error {
		lastPushErr = pushBranchToOrigin(r.g, r.townRoot, r.rigName, r.branch, head, baseRef)
		return lastPushErr
	}
	firstErr := attempt()
	if firstErr != nil {
		style.PrintWarning("push failed for branch '%s': %v — re-checking origin before treating the work as unlanded", r.branch, firstErr)
	}
	recovered, verifyErr := landBranchPush(attempt,
		func() error { return verifyPushLanded(r.g, r.townRoot, r.rigName, r.branch, head) },
		time.Sleep)
	if verifyErr != nil {
		noteVerifiedPushFailure(sourceBD, r.cwd, r.issueID, r.branch, head, verifyErr)
		msg := unlandedPushMessage(r.branch, firstErr, verifyErr)
		if lastPushErr != nil {
			return doneExit(doneExitPushFailed, msg, verifyErr)
		}
		return doneExit(doneExitPushUnverified, msg, verifyErr)
	}
	if recovered && firstErr != nil {
		fmt.Printf("%s Branch pushed to origin (recovered: the first attempt reported an error, origin has commit %s)\n",
			style.Bold.Render("✓"), shortSHA(head))
	} else {
		fmt.Printf("%s Branch pushed to origin\n", style.Bold.Render("✓"))
	}
	return nil
}

// markReadyToLand writes the READY TO LAND block, then the label the landing
// worker picks by, then reads the bead back. The block goes first so a bead
// that carries the label always says what to land.
func markReadyToLand(bd *beads.Beads, w land.Work) error {
	if bd == nil {
		return errors.New("no beads client for the work bead")
	}
	if err := bd.AppendNotes(w.BeadID, land.FormatReadyNote(w)); err != nil {
		return fmt.Errorf("writing the READY TO LAND note: %w", err)
	}
	if err := bd.Update(w.BeadID, beads.UpdateOptions{
		AddLabels:    []string{land.LabelReadyToLand},
		RemoveLabels: []string{land.LabelRework},
	}); err != nil {
		return fmt.Errorf("adding %s: %w", land.LabelReadyToLand, err)
	}
	// bd reporting success is not proof the write persisted (GH#1945).
	issue, err := bd.Show(w.BeadID)
	if err != nil {
		return fmt.Errorf("reading %s back: %w", w.BeadID, err)
	}
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		return fmt.Errorf("%s does not carry %s after the write", w.BeadID, land.LabelReadyToLand)
	}
	return nil
}

// reportDone tells the Witness and the agent bead how the run ended, then
// retires the session. Only runs that succeeded reach it.
func reportDone(r *doneRun, exitType string, sub doneSubmission) error {
	// A conflict-resolution completion releases an MR that already exists,
	// and nothing else emits a wake for its blocked->ready transition
	// (gt-rv8h). The candidates are read before updateAgentStateOnDoneFn
	// clears hook_bead, and checked after it closes the hooked task (gt-ue2h).
	var wakeConflictCandidates []string
	var wakeConflictBD *beads.Beads
	if sub.head == "" {
		wakeConflictBD = sub.sourceBD
		if wakeConflictBD == nil {
			wakeConflictBD = beads.New(r.cwd)
		}
		wakeConflictCandidates = conflictResolutionCandidates(r.cwd, r.agentBeadID, r.issueID)
	}

	// Completion metadata on the agent bead is the audit trail the witness
	// patrol reads for anomalies and crash recovery (gt-1qlg).
	fmt.Printf("\nNotifying Witness...\n")
	if r.agentBeadID != "" {
		completionBd := beads.New(r.cwd).ForAgentBead()
		meta := &beads.CompletionMetadata{
			ExitType:       exitType,
			Branch:         r.branch,
			HookBead:       r.issueID,
			CompletionTime: time.Now().UTC().Format(time.RFC3339),
		}
		if err := completionBd.UpdateAgentCompletion(r.agentBeadID, meta); err != nil {
			style.PrintWarning("could not write completion metadata to agent bead: %v", err)
		}
	}

	// Self-report cleanup_status (ZFC #10), addressed through the rig
	// directory so it still resolves if the worktree is already gone.
	selfReportCleanupStatus(r.g, r.branch, beads.New(filepath.Join(r.townRoot, r.rigName)).ForAgentBead(), r.agentBeadID, doneCleanupStatus)

	if err := LogDone(r.townRoot, r.sender, r.issueID); err != nil {
		style.PrintWarning("could not log done event: %v", err)
	}
	if err := events.LogFeed(events.TypeDone, r.sender, events.DonePayload(r.issueID, r.branch)); err != nil {
		style.PrintWarning("could not log feed event: %v", err)
	}

	// Update agent bead state (ZFC: self-report completion).
	if err := updateAgentStateOnDoneFn(r.cwd, r.townRoot, exitType, r.issueID); err != nil {
		return err
	}
	if wakeConflictCandidates != nil {
		wakeRefineryForReadyConflict(wakeConflictBD.Show, r.rigName, wakeConflictCandidates...)
	}

	// Nudge the witness only after hook/cleanup state is updated, or it
	// evaluates slot availability against stale state.
	nudgeWitness(r.rigName, fmt.Sprintf("POLECAT_DONE %s exit=%s", r.polecatName, exitType))
	fmt.Printf("%s Witness notified of %s (via nudge)\n", style.Bold.Render("✓"), exitType)

	fromHandoff := os.Getenv(envDoneFromHandoff) == "1"
	isPolecat := false
	if roleInfo, err := GetRoleWithContext(r.cwd, r.townRoot); err == nil && roleInfo.Role == RolePolecat {
		isPolecat = true
	}
	fmt.Println()
	if !isPolecat {
		fmt.Printf("%s Session exiting\n", style.Bold.Render("→"))
		fmt.Printf("  Witness will handle cleanup.\n")
		return nil
	}
	// Retire the live session as the final action. The PID exclusion keeps
	// gt done alive until everything above is written.
	retirePolecatSessionAfterFinalExit(exitType, fromHandoff, r.rigName, r.polecatName, os.Getpid())
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

// verifyPushLanded asserts that the remote branch tip is exactly the commit
// gt done is about to declare ready to land (gt-2wqt). It is the only source
// of the "Branch pushed" claim: a push that exits 0 while origin keeps an older
// tip is indistinguishable from success until ls-remote is compared against
// HEAD.
//
// Every error return is fatal to the submission: a ready mark naming a commit
// origin does not have would make the landing worker merge a tree that lacks
// the fix.
func verifyPushLanded(g *git.Git, townRoot, rigName, branch, commit string) error {
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
	return fmt.Errorf("verified_push_failed: branch %s is not at the commit gt done would declare ready to land\n"+
		"  local HEAD:  %s\n"+
		"  origin/%s:  %s\n"+
		"  %v", branch, commit, branch, remoteTip, cause)
}

// pushBranchToOrigin pushes head to origin/<branch> under a lease on the tip
// origin has now ("" = the branch must not exist yet), falling back to the
// rig's bare repo when the worktree's git context cannot reach the remote
// (GH #1348). A tip already at head is not re-sent. The retry in
// landBranchPush calls this again, and it re-reads the tip each time.
//
// A lease alone would let gt done replace any tip it had just read, including
// another session's rework of the same branch. So when origin's tip is not an
// ancestor of head, the change-sets are compared first (recoverDivergedPush):
// a rebase or a rework on top of origin's commits is pushed over under the
// lease, and real divergence is refused (gt-bf5x, gt-i0z3).
func pushBranchToOrigin(g *git.Git, townRoot, rigName, branch, head, baseRef string) error {
	expected, err := g.PushRemoteBranchTip("origin", branch)
	if err != nil {
		return fmt.Errorf("reading origin/%s before the push: %w", branch, err)
	}
	if expected == head {
		return nil
	}
	refspec := "refs/heads/" + branch + ":refs/heads/" + branch
	if expected != "" {
		if contained, ancErr := g.IsAncestor(expected, head); ancErr != nil || !contained {
			recovered, diagnosis, recoverErr := recoverDivergedPush(g, "origin", refspec, branch, baseRef)
			switch {
			case recovered:
				fmt.Printf("%s Replaced origin/%s: %s\n", style.Bold.Render("✓"), branch, diagnosis)
				return nil
			case recoverErr != nil:
				return fmt.Errorf("origin/%s has diverged from this branch (%s): %w", branch, diagnosis, recoverErr)
			default:
				return fmt.Errorf("refusing to push over origin/%s: %s", branch, diagnosis)
			}
		}
	}
	err = g.PushForceWithLease("origin", refspec, "refs/heads/"+branch, expected)
	if err == nil {
		return nil
	}
	style.PrintWarning("primary push failed: %v — trying bare repo fallback...", err)
	bareRepoPath := filepath.Join(townRoot, rigName, ".repo.git")
	if _, statErr := os.Stat(bareRepoPath); statErr != nil {
		return err
	}
	bareGit := git.NewGitWithDir(bareRepoPath, "")
	if bareErr := bareGit.PushForceWithLease("origin", refspec, "refs/heads/"+branch, expected); bareErr != nil {
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

// landBranchPush delivers the branch to origin and proves the commit arrived,
// retrying the push and the assertion as one unit. It is the gate between a
// push whose first attempt failed and the terminal exit (gt-0opm).
//
// A failed first attempt is not a verdict: `git push` reports an error for
// outcomes that leave the branch on origin anyway — a client-side timeout after
// the receiving side took the objects, a worktree git context only the bare-repo
// fallback could work around. Exiting there used to cost the submission rather
// than a retry, with the branch on origin and the issue still hooked.
//
// Origin is queried before anything is re-sent, so a landing already in place is
// proven without a second push. attemptPush must therefore be idempotent:
// pushBranchToOrigin re-reads origin's tip each time, sends nothing when it is
// already the commit, and pushes under a lease on the tip it read (after the
// divergence check), so a retry never clobbers work origin has and HEAD lacks.
// recovered reports whether the retry was what proved it.
func landBranchPush(attemptPush, verify func() error, sleep func(time.Duration)) (bool, error) {
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
// branch never proved out on origin. The push-command error says why the send
// broke and the assertion says where origin stands; a reader needs both. When
// only the assertion failed, it is the whole story (gt-0opm).
func unlandedPushMessage(branch string, pushErr, verifyErr error) string {
	if pushErr == nil {
		return verifyErr.Error()
	}
	return fmt.Sprintf("push failed for branch '%s': %v [after retry: %v]", branch, pushErr, verifyErr)
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

// clearDoneCheckpoints removes done-cp:* labels from the agent bead. gt done
// no longer writes them (its push is idempotent under a lease), but agent
// beads from earlier runs still carry them.
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
// cleanup_status is NOT written here — reportDone self-reports it through
// selfReportCleanupStatus so that failed submissions record it too.
//
// BUG FIX (hq-3xaxy): This function must be resilient to working directory deletion.
// If the polecat's worktree is deleted before gt done finishes, we use env vars as fallback.
// All errors are warnings, not failures - gt done must complete even if bead ops fail.
func updateAgentStateOnDone(cwd, townRoot, exitType, issueID string) error {
	return updateAgentStateOnDoneIn(doneStateEnv{}, cwd, townRoot, exitType, issueID)
}

// doneStateEnv is what updateAgentStateOnDone reads beyond its arguments:
// the environment, bd, and the HEAD that review evidence is checked against.
// The zero value is the real process: os.Getenv, bd on PATH, and git in the
// process's working directory. A test passes an env map and an in-process
// bd, so it needs no PATH stub, t.Setenv or chdir and can run in parallel.
type doneStateEnv struct {
	getenv     func(string) string
	bd         beads.BDRunner
	reviewHead func() (string, error)
}

func (e doneStateEnv) lookup() func(string) string {
	if e.getenv == nil {
		return os.Getenv
	}
	return e.getenv
}

func (e doneStateEnv) head() func() (string, error) {
	if e.reviewHead == nil {
		return currentReviewEvidenceHead
	}
	return e.reviewHead
}

func updateAgentStateOnDoneIn(e doneStateEnv, cwd, townRoot, exitType, issueID string) error {
	getenv := e.lookup()
	// Get role context - try multiple sources for resilience
	roleInfo, err := getRoleWithContextEnv(cwd, townRoot, getenv)
	if err != nil {
		// Fallback: try to construct role info from environment variables
		// This handles the case where cwd is deleted but env vars are set
		envRole := getenv("GT_ROLE")
		envRig := getenv("GT_RIG")
		envPolecat := getenv("GT_POLECAT")

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
	bd := beads.NewWithBeadsDirAndRunner(beadsPath, "", e.bd)
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
		hookBd, _, _ := routedIssueBeadsRun(beadsPath, hookedBeadID, e.bd)
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

			currentHead, _ := e.head()()
			if skipReason, fatal := doneSourceCloseSkipReasonForHead(hookBd, hookedBeadID, hookedBead, currentHead); skipReason != "" {
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
				//
				// Then close the wisp root with --force and audit reason.
				// ForceCloseWithReason handles any status (hooked, open, in_progress)
				// and records the reason + session for attribution.
				// Not found = already burned/deleted by another path, continue.
				n, molErr := closeStepsThenRoot(hookBd, attachment.AttachedMolecule, func() error {
					if err := hookBd.ForceCloseWithReason("done", attachment.AttachedMolecule); err != nil && !errors.Is(err, beads.ErrNotFound) {
						return err
					}
					return nil
				})
				if n > 0 {
					fmt.Fprintf(os.Stderr, "Closed %d molecule step(s) for %s\n", n, attachment.AttachedMolecule)
				}
				if molErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: couldn't close attached molecule %s: %v\n", attachment.AttachedMolecule, molErr)
					// Don't try to close hookedBeadID - it may still be blocked.
					// But DO clear hooks and update agent state (goto doneStateUpdate)
					// so the polecat isn't stuck in 'working' state (za-o9e).
					goto doneStateUpdate
				}
			}

			// Acceptance criteria gate: skip close if criteria are unchecked.
			if beads.HasLabel(hookedBead, land.LabelReadyToLand) {
				// Submitted for landing: the landing worker closes it when it
				// lands, with the landed commit (ADR 0004). "Closed" means
				// landed; closing here would claim that for a pushed branch.
				attempt := 1 + land.CountRejections(hookedBead.Notes)
				note := fmt.Sprintf("Submitted for landing (attempt %d)", attempt)
				if w, ok := land.ParseReadyNote(hookedBead.Notes); ok {
					note = fmt.Sprintf("Submitted for landing: %s @ %s onto %s (attempt %d)", w.Branch, shortSHA(w.Head), w.Target, attempt)
				}
				if err := hookBd.AddComment(hookedBeadID, note); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: couldn't record the submission on %s: %v\n", hookedBeadID, err)
				}
			} else if unchecked := beads.HasUncheckedCriteria(hookedBead); unchecked > 0 {
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

	// ZFC #10 cleanup_status self-report moved to reportDone and runDone's
	// failure path (selfReportCleanupStatus): this function only ever ran when
	// the submission succeeded, so a failed submission recorded nothing at
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
