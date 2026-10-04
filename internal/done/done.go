package done

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

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/role"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Valid exit types for gt done
const (
	ExitCompleted = "COMPLETED"
	ExitEscalated = "ESCALATED"
	ExitDeferred  = "DEFERRED"
)

// EnvFromHandoff marks a `gt done` subprocess as gt handoff's polecat
// redirect (handoff.go), not a directly- or agent-issued final status report.
// Session retirement must not apply to this path (gt-5g3e): polecat-CLAUDE.md
// promises a mid-work handoff continues the work.
const EnvFromHandoff = "GT_DONE_FROM_HANDOFF"

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

type PolecatWorktree struct {
	TownRoot    string
	Cwd         string
	RigName     string
	PolecatName string
	Actor       string
}

// doneLocalGate is gt done's pre-submit gate: the same land.Gate seam Land()
// runs on the merged tree, here in its unit tier (no container slot) on the
// rebased branch. It is `make presubmit`, the changed packages only, because
// the landing worker runs the full `make gate` on the merged tree (gt-ssyxd).
// Tests replace this variable.
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

func resolveDonePolecatWorktree(opts Options) (PolecatWorktree, error) {
	cwd, err := opts.getwd()()
	if err != nil {
		return PolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable: %w", err)
	}
	return ResolvePolecatWorktreeIn(cwd, opts.env(), GitTopLevel)
}

func resolveDonePolecatWorktreeAt(cwd string) (PolecatWorktree, error) {
	return ResolvePolecatWorktreeIn(cwd, os.Getenv, GitTopLevel)
}

// resolveDonePolecatWorktreeIn is resolveDonePolecatWorktreeAt reading the
// session's identity and town root through getenv and the git root of a
// directory through topLevel (GitTopLevel).
func ResolvePolecatWorktreeIn(cwd string, getenv func(string) string, topLevel func(dir string) (string, error)) (PolecatWorktree, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return PolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable")
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return PolecatWorktree{}, fmt.Errorf("resolving current directory: %w", err)
	}
	if info, err := os.Stat(absCwd); err != nil {
		return PolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable: %w", err)
	} else if !info.IsDir() {
		return PolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current path is not a directory: %s", absCwd)
	}

	townRoot, err := workspace.FindOrError(absCwd)
	if err != nil {
		return PolecatWorktree{}, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if err := doneValidateSessionTownRoot(townRoot, getenv); err != nil {
		return PolecatWorktree{}, err
	}

	actorRig, actorName, err := donePolecatActorIdentity(getenv("BD_ACTOR"))
	if err != nil {
		return PolecatWorktree{}, err
	}
	roleRig, roleName, err := donePolecatEnvIdentity(getenv("GT_ROLE"), getenv("GT_RIG"), getenv("GT_POLECAT"))
	if err != nil {
		return PolecatWorktree{}, err
	}
	if actorRig != roleRig || actorName != roleName {
		return PolecatWorktree{}, fmt.Errorf("gt done identity mismatch: BD_ACTOR=%s/polecats/%s but GT_ROLE/GT_RIG/GT_POLECAT resolve to %s/polecats/%s", actorRig, actorName, roleRig, roleName)
	}
	if err := doneRejectGitEnvOverrides(getenv); err != nil {
		return PolecatWorktree{}, err
	}

	gitRoot, err := topLevel(absCwd)
	if err != nil {
		return PolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat git worktree: %w", err)
	}
	gitRoot = CanonicalPath(gitRoot)
	canonicalCwd := CanonicalPath(absCwd)
	if !donePathWithin(gitRoot, canonicalCwd) {
		return PolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory %s is outside git root %s", canonicalCwd, gitRoot)
	}

	candidates, err := donePolecatWorktreeCandidates(townRoot, actorRig, actorName)
	if err != nil {
		return PolecatWorktree{}, err
	}
	for _, candidate := range candidates {
		if gitRoot == CanonicalPath(candidate) {
			return PolecatWorktree{
				TownRoot:    townRoot,
				Cwd:         gitRoot,
				RigName:     actorRig,
				PolecatName: actorName,
				Actor:       fmt.Sprintf("%s/polecats/%s", actorRig, actorName),
			}, nil
		}
	}

	return PolecatWorktree{}, fmt.Errorf("gt done must be run from assigned polecat worktree %s; current git root is %s", strings.Join(candidates, " or "), gitRoot)
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
	if gtRole == string(role.Polecat) {
		return "", "", nil
	}
	parts := strings.Split(gtRole, "/")
	switch len(parts) {
	case 2:
		parsed, roleRig, rolePolecat := role.Parse(gtRole)
		if parsed != role.Polecat || roleRig == "" || rolePolecat == "" {
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
	current := CanonicalPath(townRoot)
	if envRoot := strings.TrimSpace(getenv("GT_TOWN_ROOT")); envRoot != "" {
		if CanonicalPath(envRoot) != current {
			return fmt.Errorf("gt done town root mismatch: GT_TOWN_ROOT=%s but current workspace is %s", CanonicalPath(envRoot), current)
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

func GitTopLevel(cwd string) (string, error) {
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

func CanonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

func polecatSessionRetirementTarget(reg *session.PrefixRegistry, rigName, polecatName string, pid int) (string, []string, bool) {
	if rigName == "" || polecatName == "" || pid <= 0 {
		return "", nil, false
	}
	return session.PolecatSessionName(reg.PrefixForRig(rigName), polecatName), []string{fmt.Sprintf("%d", pid)}, true
}

func retirePolecatSessionAfterDone(killer doneSessionKiller, reg *session.PrefixRegistry, rigName, polecatName string, pid int) error {
	sessionName, excludePIDs, ok := polecatSessionRetirementTarget(reg, rigName, polecatName, pid)
	if !ok {
		return nil
	}
	return killer.KillSessionWithProcessesExcluding(sessionName, excludePIDs)
}

// retirePolecatSessionAfterFinalExit decides whether this exit retires the live
// polecat session and, when it does, tears the session down. The decision and
// the kill share one function so a final status cannot report itself retired
// and then skip the kill; tests drive this path directly (gt-5g3e).
//
// Call it as gt done's last action. retirePolecatSessionAfterDone excludes the
// caller's own PID, so the durable handoff writes above it still finish.
//
// killer tears the session down (tmux in production).
func retirePolecatSessionAfterFinalExit(killer doneSessionKiller, reg *session.PrefixRegistry, exitType string, fromHandoff bool, rigName, polecatName string, pid int) bool {
	if !shouldRetirePolecatSessionAfterDone(exitType, fromHandoff) {
		fmt.Printf("%s Session preserved for handoff continuation\n", style.Bold.Render("→"))
		return false
	}
	fmt.Printf("%s Polecat session retiring after durable handoff\n", style.Bold.Render("✓"))
	fmt.Printf("%s Terminating polecat session\n", style.Bold.Render("→"))
	if err := retirePolecatSessionAfterDone(killer, reg, rigName, polecatName, pid); err != nil {
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

// cleanupStatusGit is the part of *git.Git the cleanup-status observation
// reads.
type cleanupStatusGit interface {
	CheckUncommittedWork() (*git.UncommittedWorkStatus, error)
	BranchPushedToRemote(localBranch, remote string) (bool, int, error)
}

// branchStashGit is the part of *git.Git popBranchStashes reads. A seam, so
// the pop chain's stop conditions are unit-testable without a real worktree.
type branchStashGit interface {
	StashListForBranch() ([]git.StashEntry, error)
	StashPop(ref string) error
	CheckUncommittedWork() (*git.UncommittedWorkStatus, error)
}

// observeCleanupStatus derives the polecat's self-reported cleanup status from
// the live worktree: uncommitted files, stashes, and whether the branch is
// pushed to origin. It returns "" when git cannot be read at all.
//
// CheckUncommittedWork.UnpushedCommits doesn't work for branches without
// upstream tracking (common for polecats), so the pushed check goes through the
// more robust BranchPushedToRemote, which compares against origin/main.
func observeCleanupStatus(g cleanupStatusGit, branch string) string {
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

// resolveDoneAgentIdentity returns the identity gt done writes its agent-bead
// lifecycle metadata through, plus the actor string it logs under.
//
// The polecat identity is already proven by resolveDonePolecatWorktree: gt done
// refuses to run unless BD_ACTOR and GT_ROLE/GT_RIG/GT_POLECAT agree on one
// polecat and cwd is that polecat's worktree. Seeding the context from those
// validated identifiers, and letting env/cwd detection merely refine it, means
// the agent-bead ID no longer depends on that detection succeeding at all.
//
// That dependency was the silent hole: Agent.BeadID returns "" for an
// unknown/rig-less identity, and every agent-bead write in gt done is guarded
// by `agentBeadID != ""`. When role detection degraded, gt done skipped the
// resume checkpoints, active_mr, the completion metadata, agent_state AND the
// cleanup_status self-report, then exited 0 and logged "[done]" — leaving a
// slot that reads cleanup_status=<missing> with no later writer to repair it
// (see selfReportCleanupStatus and reclaim.go).
//
// detect is the CLI layer's role detection (nil when the caller has none);
// getenv reads GT_ROLE and friends through it.
func resolveDoneAgentIdentity(detect Detect, getenv func(string) string, cwd, townRoot, rigName, polecatName string) (Agent, string) {
	ctx := Agent{
		Role:     role.Polecat,
		Rig:      rigName,
		Polecat:  polecatName,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	if detect == nil {
		return ctx, ""
	}
	found, actor, named := detect(cwd, townRoot, getenv)
	// A detection that names no role (RoleUnknown, what the CLI layer reports
	// for a failed or deleted GT_ROLE) must not clobber the seeded polecat
	// identity: ctx.Role stays RolePolecat so Agent.BeadID() stays non-empty
	// and the agent-bead writes (completion metadata, cleanup_status, the
	// hooked-bead close) still happen. Overwriting it with Unknown would blank
	// every one of them and strand the slot (see the resolveDoneAgentIdentity
	// doc comment above).
	if found.Role != "" && found.Role != role.Unknown {
		ctx.Role = found.Role
	}
	if found.Rig != "" {
		ctx.Rig = found.Rig
	}
	if found.Polecat != "" {
		ctx.Polecat = found.Polecat
	}
	// Only a named detection contributes a log actor. The actor string degrades
	// to the literal "unknown" for an unknown role, which would otherwise
	// replace the already-validated BD_ACTOR sender on the "[done]" event.
	if !named {
		return ctx, ""
	}
	return ctx, actor
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
//     status on that path too. A failed push is exactly the case where
//     polecat reclaim needs to see "has_unpushed".
//   - A status that could not be observed stays "" or parses to CleanupUnknown.
//     Re-observing the live worktree at completion time is the last chance to
//     record a real value; if even that fails, "unknown" is recorded rather
//     than nothing. "unknown" is not a clearance — CleanupStatus.IsSafe() is
//     false for it and workstate.go's gates treat it exactly like an empty
//     value, so recording it is fail-closed. It only makes "gt done ran and
//     could not prove the tree was safe" distinguishable from "gt done never
//     ran", which is what the blocked slots were indistinguishable from.
func selfReportCleanupStatus(g cleanupStatusGit, branch string, updater beads.Client, agentBeadID, doneCleanupStatus string) {
	if agentBeadID == "" {
		style.PrintWarning("no agent bead ID for this polecat; cleanup_status not recorded — the slot will read as cleanup_status=<missing> and cannot be reclaimed")
		return
	}
	observed := ""
	if parseCleanupStatus(doneCleanupStatus) == polecat.CleanupUnknown {
		observed = observeCleanupStatus(g, branch)
	}
	status := resolveCleanupStatusForSelfReport(doneCleanupStatus, observed)
	if err := beads.UpdateAgentCleanupStatus(updater, agentBeadID, string(status)); err != nil {
		// Non-fatal: the rest of gt done still runs (za-o9e)
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

func doneSourceCloseSkipReason(bd beads.Client, issueID string, issue *beads.Issue) (string, bool) {
	currentHead, _ := CurrentReviewEvidenceHead()
	return DoneSourceCloseSkipReasonForHead(bd, issueID, issue, currentHead)
}

func DoneSourceCloseSkipReasonForHead(bd beads.Client, issueID string, issue *beads.Issue, currentHead string) (string, bool) {
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

func doneReviewOnlyCloseSkipReason(bd beads.Client, issueID string, issue *beads.Issue) (string, bool) {
	issue, skipReason, fatal := loadDoneSourceIssue(bd, issueID, issue)
	if skipReason != "" {
		return skipReason, fatal
	}
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil || !attachment.ReviewOnly {
		return "", false
	}
	currentHead, err := CurrentReviewEvidenceHead()
	if err != nil {
		return fmt.Sprintf("could not verify review evidence for %s: %v", issueID, err), true
	}
	return doneReviewOnlyCloseSkipReasonForHead(bd, issueID, issue, currentHead)
}

func doneReviewOnlyCloseSkipReasonForHead(bd beads.Client, issueID string, issue *beads.Issue, currentHead string) (string, bool) {
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

func loadDoneSourceIssue(bd beads.Client, issueID string, issue *beads.Issue) (*beads.Issue, string, bool) {
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

func CurrentReviewEvidenceHead() (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving current HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func hasFreshReviewReportEvidence(bd beads.Client, issueID string, issue *beads.Issue, assignmentAt time.Time, assignee, currentHead string) (bool, error) {
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
	// cleanupStatus is the worktree's git state as last observed or set by
	// --cleanup-status; a successful push moves it on.
	cleanupStatus string
	// agent is the identity the agent-bead writes address, and env reads the
	// environment the run's seams do.
	agent Agent
	env   func(string) string
	// recordAgentState is the CLI layer's agent-bead state write; nil when the
	// caller has no agent bead to write.
	recordAgentState func(cwd, townRoot, exitType, issueID string) error
	// sourceIssue and sourceBD are the same pair doneSubmission carries; the
	// revert guard reads the bead's label and description from here and appends
	// the deletes-by-spec waiver through its client.
	sourceIssue *beads.Issue
	sourceBD    beads.Client
	opts        doneOptions
	deps        doneSubmitDeps
}

// doneSubmission is what a COMPLETED run handed over.
type doneSubmission struct {
	sourceIssue *beads.Issue
	sourceBD    beads.Client
}

func Run(opts Options) error {
	getenv := opts.env()

	// Guard: Only polecats should call gt done
	// Polecat sessions end with gt done — the session is cleaned up, but the
	// polecat's persistent identity (agent bead, CV chain) survives across assignments.
	// Crew submit their pushed branch for landing (gt-3e7tk); every other
	// identity is a polecat or refused below.
	actor := getenv("BD_ACTOR")

	// Validate exit status
	exitType := strings.ToUpper(opts.Status)
	if exitType != ExitCompleted && exitType != ExitEscalated && exitType != ExitDeferred {
		return fmt.Errorf("invalid exit status '%s': must be COMPLETED, ESCALATED, or DEFERRED", opts.Status)
	}
	cwd, _ := opts.getwd()()
	if IsCrewRun(getenv, cwd) {
		return runDoneCrew(opts, exitType, getenv)
	}
	if actor != "" && !isPolecatActor(actor) {
		return fmt.Errorf("gt done is for polecats and crew only (you are %s)\nPolecat sessions end with gt done — the session is cleaned up, but identity persists.\nCrew submit a pushed branch with gt done --bead <id>. Other roles don't use gt done.", actor)
	}
	if opts.PreVerified {
		return fmt.Errorf("--pre-verified is for crew submissions; a polecat's gt done always runs the local gate")
	}
	issue := doneIssueFromFlags(opts)

	worktree, err := resolveDonePolecatWorktree(opts)
	if err != nil {
		return err
	}
	r := &doneRun{
		townRoot:         worktree.TownRoot,
		cwd:              worktree.Cwd,
		rigName:          worktree.RigName,
		polecatName:      worktree.PolecatName,
		sender:           worktree.Actor,
		env:              getenv,
		recordAgentState: opts.RecordAgentState,
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
	// This prevents premature polecat cleanup by recording the git state
	cleanupStatus := opts.CleanupStatus
	if cleanupStatus == "" {
		cleanupStatus = observeCleanupStatus(r.g, r.branch)
	}
	if cleanupStatus == "stash" {
		cleanupStatus = popBranchStashes(r.g, cleanupStatus)
	}
	if cleanupStatus == "uncommitted" {
		if err := autoSaveUncommittedWork(r.g, r.cwd, r.branch); err != nil {
			return err
		}
		// Re-observe rather than assume the safety net caught everything: a
		// failed add or commit leaves the work uncommitted, and the status the
		// run records must say so.
		if observed := observeCleanupStatus(r.g, r.branch); observed != "" {
			cleanupStatus = observed
		}
	}

	info := parseBranchName(r.branch)
	r.issueID = issue
	if r.issueID == "" {
		r.issueID = info.Issue
	}

	// Get agent bead ID for cross-referencing.
	ctx, actorID := resolveDoneAgentIdentity(opts.Detect, getenv, r.cwd, r.townRoot, r.rigName, r.polecatName)
	if actorID != "" {
		r.sender = actorID
	}
	r.agentBeadID = ctx.BeadID()
	r.agent = ctx

	// Recreate the agent bead if it's missing (hq-xu4p). Completion metadata
	// writes to it; when it's gone every write fails
	// 'issue not found'.
	ensureAgentBeadExists(beads.ForAgentBead(beads.New(r.cwd)), r.agentBeadID, ctx)
	var assignedIssueIDs []string
	loadAssignedIssueIDs := func() []string {
		if assignedIssueIDs == nil && r.sender != "" {
			assignedIssueIDs = findAssignedBeadsForAgent(r.cwd, r.townRoot, r.sender)
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
	// explicit issue — --issue, or --bead, which fills the same slot the flag
	// does — still wins, and subtask branches of the hooked bead (e.g. gt-abc.1
	// under hooked gt-abc) are left alone.
	if staleBranchGuardApplies(issue, info.Issue, r.sender) {
		if hookIssue, ambiguous := selectAssignedIssue(info.Issue, loadAssignedIssueIDs()); isStaleBranchIssue(info.Issue, hookIssue) {
			style.PrintWarning("branch %q embeds issue %s but your hooked bead is %s — submitting for %s (stale branch reuse?)", r.branch, info.Issue, hookIssue, hookIssue)
			fmt.Printf("  Fresh branches must be named polecat/<name>/<bead-id>+<suffix> for the bead you are working.\n")
			fmt.Printf("  Use --issue to override if the branch-derived id is actually correct.\n\n")
			r.issueID = hookIssue
		} else if ambiguous {
			return fmt.Errorf("branch %q embeds issue %s but %s has multiple active assignments; use --issue to disambiguate", r.branch, info.Issue, r.sender)
		}
	}

	// Write heartbeat state="exiting" (gt-3vr5: heartbeat v2): the agent is
	// trusted until the heartbeat goes stale.
	r.heartbeatSession = getenv("GT_SESSION")
	if r.heartbeatSession != "" && r.townRoot != "" {
		polecat.TouchSessionHeartbeatWithState(r.townRoot, r.heartbeatSession, polecat.HeartbeatExiting, "gt done", r.issueID)
	}

	r.defaultBranch = "main" // fallback
	rigPath := filepath.Join(r.townRoot, r.rigName)
	rigCfg, cfgErr := rig.LoadRigConfigIfPresent(rigPath)
	if cfgErr != nil {
		rig.WarnRigConfigOnce(rigPath, cfgErr)
	}
	if rigCfg != nil && rigCfg.DefaultBranch != "" {
		r.defaultBranch = rigCfg.DefaultBranch
	}

	r.cleanupStatus = cleanupStatus
	if exitType == ExitCompleted {
		r.useRealSubmitDeps(opts)
		err = submitForLanding(r)
		cleanupStatus = r.cleanupStatus
		if err != nil {
			// Nothing is reported done, but the worktree's git state is still
			// recorded: a session that dies before re-running gt done must not
			// strand its slot (hq-vx224).
			selfReportCleanupStatus(r.g, r.branch, beads.ForAgentBead(beads.New(filepath.Join(r.townRoot, r.rigName))), r.agentBeadID, cleanupStatus)
			return err
		}
	} else {
		fmt.Printf("%s Signaling %s\n", style.Bold.Render("→"), exitType)
		if r.issueID != "" {
			fmt.Printf("  Issue: %s\n", r.issueID)
		}
		fmt.Printf("  Branch: %s\n", r.branch)
	}
	return reportDone(r, exitType)
}

// popBranchStashes pops this branch's stashes oldest first so the auto-save
// below commits their contents (gt-pvx stash recovery). Agents have been
// observed running `git stash` before a rebase and dying before
// `git stash pop`; popping on the way out turns a lost stash into a commit. A
// conflicting pop stops the chain: surfacing the conflict beats silently
// dropping a stash.
//
// It returns the cleanup status the pop left behind: "uncommitted" when the
// stash content is now in the working tree, and "" when a full pop chain
// produced nothing dirty (recompute normally). A chain that stopped — a
// failed pop, or a stash list that could not be read — returns current
// unchanged, so the caller keeps "stash": the stashes are still there and the
// slot must not read as reclaimed (gt-638go.14 finding 5).
func popBranchStashes(g branchStashGit, current string) string {
	entries, err := g.StashListForBranch()
	if err != nil {
		style.PrintWarning("auto-pop: could not list stashes: %v — orphaned stashes may remain", err)
		return current
	}
	if len(entries) == 0 {
		return current
	}
	fmt.Printf("\n%s %d stash(es) detected on this branch — auto-popping (gt-pvx safety net)\n",
		style.Bold.Render("⚠"), len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		fmt.Printf("  popping %s — %s\n", e.Ref, e.Message)
		if popErr := g.StashPop(e.Ref); popErr != nil {
			style.PrintWarning("auto-pop %s failed (likely conflict): %v", e.Ref, popErr)
			style.PrintWarning("stopping pop chain — resolve conflict manually then re-run gt done")
			return current
		}
		// After each pop, stash refs shift; re-fetch the list before next pop.
		entries, err = g.StashListForBranch()
		if err != nil || len(entries) == 0 {
			break
		}
	}
	// Pops that succeeded but produced nothing dirty recompute normally: "".
	if workStatus, wsErr := g.CheckUncommittedWork(); wsErr == nil && workStatus.HasUncommittedChanges {
		fmt.Printf("%s Stash content moved to working tree — will auto-commit below.\n", style.Bold.Render("✓"))
		return "uncommitted"
	}
	return ""
}

// autoSaveUncommittedWork commits uncommitted work before any exit path
// (gt-pvx): polecats have run gt done without committing thousands of lines,
// then died. The commit is marked as an auto-save, runtime and overlay files
// and throwaway files are left out, and deletions of tracked files are never
// committed. Only unmerged conflicts refuse.
//
// A best-effort failure warns rather than returning: the safety net may miss,
// but the run still records what it observed. The caller re-observes the
// worktree's cleanup status afterwards instead of reading a "did it save"
// flag, so a missed save stays visible as uncommitted work.
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
	return nil
}

// submitForLanding is the COMPLETED path: rebase, squash, gate, push and mark the
// work bead ready to land. It never lands anything. Every failure returns
// before any completion is recorded.
func submitForLanding(r *doneRun) error {
	var sub doneSubmission
	if r.branch == r.defaultBranch || r.branch == "master" {
		return fmt.Errorf("cannot submit the %s/master branch for landing", r.defaultBranch)
	}

	// Refuse uncommitted changes (hq-xthqf): they would be lost. Runtime
	// artifacts (.claude/, .beads/, .runtime/ ...) are toolchain-managed and
	// excluded.
	workStatus, err := r.deps.repo.CheckUncommittedWork()
	if err != nil {
		return fmt.Errorf("checking git status: %w", err)
	}
	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
		return fmt.Errorf("cannot complete: uncommitted changes would be lost\nCommit your changes first, or use --status DEFERRED to exit without completing\nUncommitted: %s", workStatus.String())
	}

	// no_merge / review_only are non-code tasks where zero commits is
	// expected (GH#2496, gt-kvf); read them before the zero-commit guard.
	isNoMergeTask := false
	reviewOnlySource := false
	if r.issueID != "" {
		issue, issueBD, sourceErr := r.deps.source(r.issueID)
		if sourceErr != nil {
			return fmt.Errorf("source issue validation failed: %w", sourceErr)
		}
		sub.sourceIssue = issue
		sub.sourceBD = issueBD
		r.sourceIssue = issue
		r.sourceBD = issueBD
		if af := beads.ParseAttachmentFields(sub.sourceIssue); af != nil {
			isNoMergeTask = af.NoMerge || af.ReviewOnly
			reviewOnlySource = af.ReviewOnly
		}
	}

	target, err := resolveDoneTarget(r, sub.sourceIssue)
	if err != nil {
		return err
	}
	// In fork-backed rigs the clean base is upstream/<target>, never the
	// fork's origin/<target>. landingRemote is the rig's configured one
	// (gt-fn9e6.9).
	repo := r.deps.repo
	landingRemote := rig.ResolveLandingRemote(r.townRoot, r.rigName)
	baseRef := repo.CleanBaseRef(landingRemote, r.defaultBranch, target)
	fetchRemote := git.RemoteForRef(baseRef, landingRemote)
	if fetchRemote == "" {
		fetchRemote = "origin"
	}
	if err := repo.Fetch(fetchRemote); err != nil {
		return fmt.Errorf("fetching %s before rebasing onto %s: %w", fetchRemote, baseRef, err)
	}

	aheadCount, err := repo.CommitsAhead(baseRef, "HEAD")
	if err != nil {
		return fmt.Errorf("counting commits ahead of %s: %w", baseRef, err)
	}
	if aheadCount == 0 {
		return completeWithoutCode(r, sub, target, baseRef, isNoMergeTask)
	}
	if reviewOnlySource {
		return fmt.Errorf("cannot complete review-only issue %s with commits ahead of %s; add a fresh review evidence comment and complete without code changes", r.issueID, baseRef)
	}
	if r.issueID == "" {
		return fmt.Errorf("cannot determine source issue from branch '%s'; use --issue to specify", r.branch)
	}
	// Before the rebase and the gate, which take minutes: unchecked criteria
	// are the landing worker's policy rejection, after the session retires.
	if err := refuseUncheckedCriteria(r.issueID, sub.sourceIssue); err != nil {
		return err
	}

	if err := rebaseOntoTarget(repo, baseRef); err != nil {
		return err
	}

	// After the rebase: a rebase replays the same diff, so the checks must
	// see the branch as it will be pushed.
	if err := r.deps.checkBranch(baseRef, sub); err != nil {
		return err
	}
	if err := r.deps.rewriteBranch(baseRef, sub); err != nil {
		return err
	}

	head, err := repo.Rev("HEAD")
	if err != nil {
		return fmt.Errorf("resolving HEAD: %w", err)
	}
	if err := runDoneLocalGate(r, head); err != nil {
		return err
	}

	// Push submodule commits first, so the parent's pointer never names a
	// commit the submodule's remote lacks (gt-dzs).
	r.deps.pushSubmodules(baseRef)
	if err := pushBranchForLanding(r, sub.sourceBD, head, baseRef); err != nil {
		return err
	}
	r.cleanupStatus = cleanupStatusAfterSuccessfulPush(r.cleanupStatus)

	work := land.Work{BeadID: r.issueID, Rig: r.rigName, Branch: r.branch, Head: head, Target: target, Worker: r.polecatName}
	if err := markReadyToLand(sub.sourceBD, work); err != nil {
		return doneExit(doneExitReadyFailed, fmt.Sprintf("branch %s is on origin at %s but the work bead could not be marked ready to land", r.branch, ShortSHA(head)), err)
	}
	recordSubmittedIntent(r)

	fmt.Printf("%s Submitted for landing\n", style.Bold.Render("✓"))
	fmt.Printf("  Branch: %s @ %s\n", r.branch, ShortSHA(head))
	fmt.Printf("  Target: %s\n", target)
	fmt.Printf("  Issue:  %s\n", r.issueID)
	fmt.Printf("  Worker: %s\n\n", r.polecatName)
	fmt.Printf("%s\n", style.Dim.Render("The daemon's landing worker merges it after gating the merged tree."))
	return nil
}

// checkBranchForSubmit refuses a rebased branch that reverts work already
// merged to the target (gt-63sz), adds throwaway files (gt-ozo4), or is
// byte-identical to a rejected attempt (gt-0jzd5).
func checkBranchForSubmit(r *doneRun, sub doneSubmission, baseRef string) error {
	if r.opts.allowReverts {
		style.PrintWarning("skipping merged-work revert check (--allow-reverts): the branch may undo work merged to %s", baseRef)
	} else if err := reportRevertedMergesRecording(r, baseRef); err != nil {
		return err
	}
	if r.opts.allowThrowawayPaths {
		style.PrintWarning("skipping throwaway-file check (--allow-throwaway-paths): the branch may add scratch files to %s", baseRef)
	} else if err := reportThrowawayPaths(r.g, baseRef); err != nil {
		return err
	}
	var sourceNotes string
	if sub.sourceIssue != nil {
		sourceNotes = sub.sourceIssue.Notes
	}
	rejectedTip := func(mrID string) (string, bool) { return rejectedTipFromMR(sub.sourceBD, mrID) }
	return reportUnchangedSinceRejection(r.g, sourceNotes, r.issueID, baseRef, rejectedTip)
}

// rewriteBranchForSubmit squashes auto-save commits, then strips AI
// attribution trailers (gt-v4ssj.10; after the squash, so the messages
// checked are the ones that will land) and the Gas Town overlay from
// CLAUDE.md / CLAUDE.local.md (gt-p35).
func rewriteBranchForSubmit(r *doneRun, sub doneSubmission, baseRef string) error {
	if err := squashAutoSaveBeforeSubmit(r.g, r.cwd, r.branch, baseRef, sub.sourceIssue, r.issueID); err != nil {
		return err
	}
	if err := stripAttributionTrailers(r.g, baseRef); err != nil {
		return err
	}
	stripOverlayCLAUDEmd(r.g, r.defaultBranch, baseRef)
	return nil
}

// resolveDoneTarget picks the branch to land on: --target, then the bead's
// formula_vars base_branch, then the rig default. resolveMRTarget refuses a
// self-target and an unexplained polecat/* target (gt-a8i3, gt-w2jc).
func resolveDoneTarget(r *doneRun, source *beads.Issue) (string, error) {
	target := r.defaultBranch
	explicit := false
	if r.opts.target != "" {
		target = r.opts.target
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
// target is the resolved landing branch (--target, the bead's base_branch or
// the rig default): the already-landed check and the close record use it, so
// work bound for a non-default branch is verified there, not on the default.
// Polecats must have at least one commit unless the work is non-code
// (gastown#1484). The error text must not mention --cleanup-status=clean:
// agents read errors and self-bypass.
func completeWithoutCode(r *doneRun, sub doneSubmission, target, baseRef string, isNoMergeTask bool) error {
	repo := r.deps.repo
	if r.opts.polecatEnv && r.cleanupStatus != "clean" && !isNoMergeTask {
		// A branch already pushed with its work whose target has since moved
		// on is not empty-handed (GH#wd7).
		pushed, unpushed, pushErr := repo.BranchPushedToRemote(r.branch, "origin")
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
	reviewHead, _ := repo.Rev("HEAD")
	if skipReason, fatal := DoneSourceCloseSkipReasonForHead(bd, r.issueID, sub.sourceIssue, reviewHead); skipReason != "" {
		style.PrintWarning("%s", skipReason)
		fmt.Printf("  The bead will remain open; the reason is recorded on it.\n")
		NotifyDoneCloseSkipped(bd, r.issueID, skipReason)
		if fatal {
			return fmt.Errorf("cannot complete review-only/no-code work: %s", skipReason)
		}
		return nil
	}

	closeReason := "Completed with no code changes (already fixed or already landed)"
	if !isNoMergeTask {
		if repo.ForkBackedRemote("origin") {
			return fmt.Errorf("cannot close no-code bead in fork/upstream mode: %s has no commits ahead of %s; use the fork PR flow instead", r.branch, baseRef)
		}
		headSHA, _ := repo.Rev("HEAD")
		if verifyErr := repo.VerifyPushedCommitReachableFromPushTarget("origin", target, headSHA); verifyErr != nil {
			noteVerifiedPushFailure(bd, r.cwd, r.issueID, target, headSHA, verifyErr)
			return fmt.Errorf("cannot close no-code bead: %w", verifyErr)
		}
		if headSHA != "" {
			closeReason = fmt.Sprintf("%s\ntarget_branch: %s\ncommit_sha: %s", closeReason, target, headSHA)
		}
	}
	// Force-close bypasses molecule dependency checks; the retry absorbs
	// transient Dolt lock contention (A2).
	if closeErr := forceCloseIssueWithRetrySleep(bd.ForceCloseWithReason, r.issueID, closeReason, "Issue %s closed (no code to land)", r.deps.sleep); closeErr != nil {
		return doneExit(doneExitCloseFailed, fmt.Sprintf("could not close issue %s after 3 attempts", r.issueID), closeErr)
	}
	return nil
}

// rebaseOntoTarget rebases the branch onto baseRef when it is behind. A
// conflict aborts the rebase, leaving the branch as it was, and exits 14
// naming the conflicting files.
func rebaseOntoTarget(g doneRepo, baseRef string) error {
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
	gate, err := r.deps.localGate(r.townRoot, r.rigName, r.cwd)
	if err != nil {
		return doneExit(doneExitGateUnavailable, "no local gate could be built; this is not a verdict on your change, so escalate (gt escalate -s medium) rather than edit code", err)
	}
	fmt.Printf("→ Running the local gate on %s\n", ShortSHA(head))
	// The gate can run for many minutes; keep the exiting heartbeat fresh so
	// no consumer reads this polecat as hung (gt-azmw).
	stopHeartbeat := polecat.StartExitingHeartbeatKeepAlive(r.townRoot, r.heartbeatSession, "gt done", r.issueID)
	ctx, cancel := context.WithTimeout(context.Background(), doneLocalGateBudget)
	res := gate.Run(ctx, r.cwd)
	cancel()
	stopHeartbeat()
	if res.Err != nil {
		return doneExit(doneExitGateUnavailable, "the local gate could not run ("+res.Summary()+"); this is not a verdict on your change, so re-run gt done once, and escalate (gt escalate -s medium) if it repeats", res.Err)
	}
	if !res.Passed {
		return doneExit(doneExitGateFailed, fmt.Sprintf("the local gate failed on %s: %s\n%s", ShortSHA(head), res.Summary(), res.FailureTail()), nil)
	}
	fmt.Printf("%s Local gate passed: %s\n", style.Bold.Render("✓"), res.Summary())
	return nil
}

// pushBranchForLanding pushes head to remote/<branch> under a lease on the
// tip the remote had, then asserts the remote holds exactly head (gt-2wqt). A
// rebased or squashed branch replaces an earlier attempt's tip; a concurrent
// push to the branch makes the lease fail instead of being clobbered. One retry
// absorbs a push that errored while the remote took the objects (gt-0opm).
//
// The remote is the rig's configured landing remote (gt-fn9e6.9), not an
// assumed origin.
func pushBranchForLanding(r *doneRun, sourceBD beads.Client, head, baseRef string) error {
	remote := rig.ResolveLandingRemote(r.townRoot, r.rigName)
	fmt.Printf("Pushing branch to %s...\n", remote)
	var lastPushErr error
	attempt := func() error {
		lastPushErr = pushBranchToOrigin(r.deps.repo, remote, r.townRoot, r.rigName, r.branch, head, baseRef)
		return lastPushErr
	}
	firstErr := attempt()
	if firstErr != nil {
		style.PrintWarning("push failed for branch '%s': %v — re-checking %s before treating the work as unlanded", r.branch, firstErr, remote)
	}
	recovered, verifyErr := landBranchPush(attempt,
		func() error { return verifyPushLanded(r.deps.repo, remote, r.townRoot, r.rigName, r.branch, head) },
		r.deps.sleep, r.deps.retryDelays)
	if verifyErr != nil {
		noteVerifiedPushFailure(sourceBD, r.cwd, r.issueID, r.branch, head, verifyErr)
		msg := unlandedPushMessage(r.branch, firstErr, verifyErr)
		if lastPushErr != nil {
			return doneExit(doneExitPushFailed, msg, verifyErr)
		}
		return doneExit(doneExitPushUnverified, msg, verifyErr)
	}
	if recovered && firstErr != nil {
		fmt.Printf("%s Branch pushed to %s (recovered: the first attempt reported an error, %s has commit %s)\n",
			style.Bold.Render("✓"), remote, remote, ShortSHA(head))
	} else {
		fmt.Printf("%s Branch pushed to %s\n", style.Bold.Render("✓"), remote)
	}
	return nil
}

// recordSubmittedIntent writes desired=submitted into the polecat's intent
// record once its bead carries gt:ready-to-land. Between here and the landing
// the seat has no session and its hook still holds the bead, which every
// crash detector reads as a dead polecat with work; the record is the answer
// they read before Dolt (gt-obbx2). The label stays authoritative, so a failed
// write is a warning: the detectors that read the bead still see the label.
func recordSubmittedIntent(r *doneRun) {
	seat := supervisor.IntentSeat(supervisor.SeatFor(r.rigName, string(role.Polecat), r.polecatName))
	if err := intent.MarkSubmitted(r.townRoot, seat, r.issueID, "gt done", time.Now()); err != nil {
		style.PrintWarning("couldn't record %s as submitted in its intent record: %v", seat, err)
	}
}

// markReadyToLand writes the READY TO LAND block, then the label the landing
// worker picks by, then reads the bead back. The block goes first so a bead
// that carries the label always says what to land. The block carries the
// submission time, which is what the landing worker orders the queue by
// (gt-t2jhf).
func markReadyToLand(bd beads.Client, w land.Work) error {
	if bd == nil {
		return errors.New("no beads client for the work bead")
	}
	w.Submitted = time.Now().UTC()
	if err := bd.AppendNotes(w.BeadID, land.FormatReadyNote(w)); err != nil {
		return fmt.Errorf("writing the READY TO LAND note: %w", err)
	}
	if err := bd.Update(w.BeadID, beads.UpdateOptions{
		AddLabels: []string{land.LabelReadyToLand},
		// An overseer review covers one head; a submission brings a new one,
		// so it must face om again (gt-g8t3m).
		RemoveLabels: []string{land.LabelRework, land.LabelOverseerReviewed},
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

// reportDone records on the agent bead how the run ended, then retires the
// session. Only runs that succeeded reach it.
func reportDone(r *doneRun, exitType string) error {
	// Completion metadata on the agent bead is the audit trail for
	// anomalies and crash recovery (gt-1qlg).
	fmt.Printf("\nRecording completion...\n")
	if r.agentBeadID != "" {
		completionBd := beads.ForAgentBead(beads.New(r.cwd))
		meta := &beads.CompletionMetadata{
			ExitType:       exitType,
			Branch:         r.branch,
			HookBead:       r.issueID,
			CompletionTime: time.Now().UTC().Format(time.RFC3339),
		}
		if err := beads.UpdateAgentCompletion(completionBd, r.agentBeadID, meta); err != nil {
			style.PrintWarning("could not write completion metadata to agent bead: %v", err)
		}
	}

	// Self-report cleanup_status (ZFC #10), addressed through the rig
	// directory so it still resolves if the worktree is already gone.
	selfReportCleanupStatus(r.g, r.branch, beads.ForAgentBead(beads.New(filepath.Join(r.townRoot, r.rigName))), r.agentBeadID, r.cleanupStatus)

	if err := events.LogFeed(events.TypeDone, r.sender, events.DonePayload(r.issueID, r.branch)); err != nil {
		style.PrintWarning("could not log done event: %v", err)
	}

	// Update agent bead state (ZFC: self-report completion). The write lives
	// in the CLI layer (it closes the hooked bead against the close
	// invariants); a caller with no agent bead leaves it nil.
	if r.recordAgentState != nil {
		if err := r.recordAgentState(r.cwd, r.townRoot, exitType, r.issueID); err != nil {
			return err
		}
	}

	getenv := r.env
	fromHandoff := getenv(EnvFromHandoff) == "1"
	fmt.Println()
	if r.agent.Role != role.Polecat {
		fmt.Printf("%s Session exiting\n", style.Bold.Render("→"))
		return nil
	}
	// Retire the live session as the final action. The PID exclusion keeps
	// gt done alive until everything above is written.
	retirePolecatSessionAfterFinalExit(tmux.NewTmux(), session.DefaultRegistry(), exitType, fromHandoff, r.rigName, r.polecatName, os.Getpid())
	return nil
}

// pushSubmoduleChanges detects submodules modified between baseRef
// and HEAD, and pushes each submodule's new commit to its remote before the
// parent repo push. This prevents the parent's submodule pointer from
// referencing commits that don't exist on the submodule's remote (gt-dzs).
// submodulePusher is what pushSubmoduleChanges needs from the worktree.
type submodulePusher interface {
	SubmoduleChanges(base, head string) ([]git.SubmoduleChange, error)
	PushSubmoduleCommit(path, sha, remote string) error
}

func pushSubmoduleChanges(g submodulePusher, baseRef string) {
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
		sha := ShortSHA(sc.NewSHA)
		fmt.Printf("Pushing submodule %s (%s)...\n", sc.Path, sha)
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

// NotifyDoneCloseSkipped records on the skipped bead itself why gt done left it
// open: one comment carrying the reason gt done already builds, no mail and no
// new bead per skip (gt-zx8t4 — mailing it to the retired mayor/ role left every
// skip as an unread dead letter in hq). A failed write warns and never fails gt
// done, so bd must be the client routed to the skipped bead's database, not the
// caller's.
func NotifyDoneCloseSkipped(bd beads.Client, issueID, reason string) {
	if bd == nil || issueID == "" {
		return
	}
	comment := fmt.Sprintf("DONE_CLOSE_SKIPPED: %s", reason)
	if err := bd.AddComment(issueID, comment); err != nil {
		style.PrintWarning("could not record the skipped close on %s: %v", issueID, err)
		return
	}
	fmt.Printf("%s Recorded skipped close on %s\n", style.Bold.Render("✓"), issueID)
}

func noteVerifiedPushFailure(sourceBD beads.Client, cwd, issueID, branch, commit string, verifyErr error) {
	if issueID == "" || cwd == "" {
		return
	}
	bd := sourceBD
	if bd == nil {
		routed, _, _ := routedIssueBeads(cwd, issueID)
		bd = routed
	}
	inProgress := "in_progress"
	_ = bd.Update(issueID, beads.UpdateOptions{Status: &inProgress})
	msg := fmt.Sprintf("verified_push_failed: commit %s not verified on origin/%s: %v", commit, branch, verifyErr)
	_ = bd.AddComment(issueID, msg)
}

// verifyPushLanded asserts that the remote branch tip is exactly the commit
// gt done is about to declare ready to land (gt-2wqt). It is the only source
// of the "Branch pushed" claim: a push that exits 0 while the remote keeps an
// older tip is indistinguishable from success until ls-remote is compared
// against HEAD.
//
// Every error return is fatal to the submission: a ready mark naming a commit
// the remote does not have would make the landing worker merge a tree that
// lacks the fix.
func verifyPushLanded(g doneRepo, remote, townRoot, rigName, branch, commit string) error {
	var bare pushVerifier
	bareRepoPath := filepath.Join(townRoot, rigName, ".repo.git")
	if _, statErr := os.Stat(bareRepoPath); statErr == nil {
		bare = git.NewGitWithDir(bareRepoPath, "")
	}
	return verifyPushLandedVia(g, bare, remote, branch, commit)
}

// pushVerifier asks a remote whether it holds commit on branch.
type pushVerifier interface {
	VerifyPushedCommit(remote, branch, commit string) error
}

// verifyPushLandedVia is verifyPushLanded with the rig's bare repo given
// (nil when the rig has none) and the landing remote named.
func verifyPushLandedVia(g doneRepo, bare pushVerifier, remote, branch, commit string) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		head, headErr := g.Rev("HEAD")
		if headErr != nil {
			return fmt.Errorf("verified_push_failed: cannot resolve HEAD for branch %s: %w", branch, headErr)
		}
		commit = strings.TrimSpace(head)
	}
	verifyErr := g.VerifyPushedCommit(remote, branch, commit)
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
	if bare != nil {
		if bareErr := bare.VerifyPushedCommit(remote, branch, commit); bareErr == nil {
			return nil
		}
	}
	return describePushVerificationFailure(g, remote, branch, commit, verifyErr)
}

// describePushVerificationFailure renders a failed push assertion with the full
// local and remote SHAs (gt-2wqt): the operator has to see both to know how far
// behind the landing remote is before the work is re-pushed.
func describePushVerificationFailure(g doneRepo, remote, branch, commit string, cause error) error {
	remoteTip, tipErr := g.PushRemoteBranchTip(remote, branch)
	if tipErr != nil || strings.TrimSpace(remoteTip) == "" {
		remoteTip = "(missing on " + remote + ")"
	}
	return fmt.Errorf("verified_push_failed: branch %s is not at the commit gt done would declare ready to land\n"+
		"  local HEAD:  %s\n"+
		"  %s/%s:  %s\n"+
		"  %v", branch, commit, remote, branch, remoteTip, cause)
}

// pushBranchToOrigin pushes head to remote/<branch> under a lease on the tip
// the remote has now ("" = the branch must not exist yet), falling back to the
// rig's bare repo when the worktree's git context cannot reach the remote
// (GH #1348). A tip already at head is not re-sent. The retry in
// landBranchPush calls this again, and it re-reads the tip each time.
//
// remote is the rig's configured landing remote (gt-fn9e6.9).
//
// A lease alone would let gt done replace any tip it had just read, including
// another session's rework of the same branch. So when the remote's tip is not
// an ancestor of head, the change-sets are compared first
// (recoverDivergedPush): a rebase or a rework on top of the remote's commits is
// pushed over under the lease, and real divergence is refused (gt-bf5x,
// gt-i0z3).
func pushBranchToOrigin(g doneRepo, remote, townRoot, rigName, branch, head, baseRef string) error {
	expected, err := g.PushRemoteBranchTip(remote, branch)
	if err != nil {
		return fmt.Errorf("reading %s/%s before the push: %w", remote, branch, err)
	}
	if expected == head {
		return nil
	}
	refspec := "refs/heads/" + branch + ":refs/heads/" + branch
	if expected != "" {
		if contained, ancErr := g.IsAncestor(expected, head); ancErr != nil || !contained {
			recovered, diagnosis, recoverErr := recoverDivergedPush(g, remote, refspec, branch, baseRef)
			switch {
			case recovered:
				fmt.Printf("%s Replaced %s/%s: %s\n", style.Bold.Render("✓"), remote, branch, diagnosis)
				return nil
			case recoverErr != nil:
				return fmt.Errorf("%s/%s has diverged from this branch (%s): %w", remote, branch, diagnosis, recoverErr)
			default:
				return fmt.Errorf("refusing to push over %s/%s: %s", remote, branch, diagnosis)
			}
		}
	}
	err = g.PushForceWithLease(remote, refspec, "refs/heads/"+branch, expected)
	if err == nil {
		return nil
	}
	style.PrintWarning("primary push failed: %v — trying bare repo fallback...", err)
	bareRepoPath := filepath.Join(townRoot, rigName, ".repo.git")
	if _, statErr := os.Stat(bareRepoPath); statErr != nil {
		return err
	}
	bareGit := git.NewGitWithDir(bareRepoPath, "")
	if bareErr := bareGit.PushForceWithLease(remote, refspec, "refs/heads/"+branch, expected); bareErr != nil {
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
func landBranchPush(attemptPush, verify func() error, sleep func(time.Duration), delays []time.Duration) (bool, error) {
	verifyErr := verify()
	for i := 0; verifyErr != nil && i < len(delays); i++ {
		sleep(delays[i])
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

// ensureAgentBeadExists recreates a missing agent bead so completion metadata,
// checkpoints, and active_mr writes don't silently fail (hq-xu4p). Only
// rig-level agents are handled — town agents (mayor/deacon) are owned by
// gt doctor. Best-effort: failures are warned, never fatal.
func ensureAgentBeadExists(bd beads.Client, id string, ctx Agent) {
	if id == "" {
		return
	}
	if issue, err := bd.Show(id); err == nil && issue != nil && issue.Status != string(beads.StatusClosed) {
		return // exists and is active
	}

	fields := &beads.AgentFields{Rig: ctx.Rig, AgentState: "idle"}
	var title string
	switch ctx.Role {
	case role.Polecat:
		fields.RoleType = "polecat"
		title = fmt.Sprintf("Polecat worker %s in %s - autonomous worker with persistent identity.", ctx.Polecat, ctx.Rig)
	default:
		return
	}

	if _, err := beads.CreateOrReopenAgentBead(bd, id, title, fields); err != nil {
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

// findAssignedBeadsForAgent queries the same assignment locations as gt hook,
// in the same order: the caller's own workdir, then the rig's mayor/rig
// directory, then the town .beads store, then — for a town-level actor — a
// scan of every rig. The assigned work bead is authoritative; agent-bead hook
// slots are intentionally ignored.
func findAssignedBeadsForAgent(workDir, townRoot, agentID string) []string {
	return findAssignedBeadsForAgentIn(workDir, townRoot, agentID, func(dir string) beads.Client {
		return beads.New(dir)
	})
}

// assignedBeadStore opens the bead store at dir. It is the seam the fallback
// ladder runs through, so a unit test can prove each location is queried
// without spawning bd.
type assignedBeadStore func(dir string) beads.Client

func findAssignedBeadsForAgentIn(workDir, townRoot, agentID string, open assignedBeadStore) []string {
	if agentID == "" {
		return nil
	}

	assigned := assignedIssueIDs(queryAssignedBeads(open(workDir), agentID))
	if len(assigned) > 0 {
		return assigned
	}
	if townRoot == "" {
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
			assigned = assignedIssueIDs(queryAssignedBeads(open(rigWorkDir), agentID))
			if len(assigned) > 0 {
				return assigned
			}
		}
	}

	// Town beads: the assignment may have been filed at the town level rather
	// than in the rig's store (a route moves the id but the write landed here
	// first). gt hook falls back the same way.
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if _, err := os.Stat(townBeadsDir); err == nil {
		assigned = assignedIssueIDs(queryAssignedBeads(open(townBeadsDir), agentID))
		if len(assigned) > 0 {
			return assigned
		}
	}

	// Town-level actors (the mayor) may hold their work in any rig's store;
	// gt hook scans them all. Mirrors internal/cmd's scanAllRigsForHookedBeads,
	// which this leaf cannot call (D10); the rig list comes from the same
	// town routes file.
	if isTownLevelActor(agentID) {
		return assignedIssueIDs(scanRigBeadsForAssigned(townRoot, agentID, open))
	}
	return nil
}

// doneIssueFromFlags is the issue an explicit flag named: --issue wins, then
// --bead. --bead fills the same slot --issue does, which is what keeps it
// suppressing the stale-branch guard in the Options form, the way the flag
// variable it replaced did (gt-638go.14 finding 4).
func doneIssueFromFlags(opts Options) string {
	if opts.Issue != "" {
		return opts.Issue
	}
	return opts.Bead
}

// staleBranchGuardApplies reports whether gt done runs the stale-branch guard
// (hq-l0fj): the branch must embed an issue id, the run must have an agent to
// check that id against, and no explicit issue may name one instead.
//
// explicitIssue is --issue, else --bead: both fill the same slot, so --bead
// suppresses the guard exactly as --issue does (gt-638go.14 finding 4).
func staleBranchGuardApplies(explicitIssue, branchIssue, sender string) bool {
	return explicitIssue == "" && branchIssue != "" && sender != ""
}

// isTownLevelActor reports whether an actor id belongs to a town-level role
// (the mayor), the only identity whose work can live in an arbitrary rig.
// Mirrors internal/cmd's isTownLevelRole.
func isTownLevelActor(agentID string) bool {
	return agentID == "mayor" || agentID == "mayor/"
}

// scanRigBeadsForAssigned walks every rig in the town's route table and
// returns the first assignment found for agentID, or nil. Mirrors
// internal/cmd's scanAllRigsForHookedBeads (D10 keeps this leaf from calling
// it); the routes file at <townRoot>/.beads is the same source both read.
func scanRigBeadsForAssigned(townRoot, agentID string, open assignedBeadStore) []*beads.Issue {
	routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return nil
	}
	for _, route := range routes {
		rigBeadsDir := route.Path
		if !filepath.IsAbs(rigBeadsDir) {
			rigBeadsDir = filepath.Join(townRoot, rigBeadsDir)
		}
		if _, err := os.Stat(rigBeadsDir); err != nil {
			continue
		}
		if assigned := queryAssignedBeads(open(rigBeadsDir), agentID); len(assigned) > 0 {
			return assigned
		}
	}
	return nil
}

func queryAssignedBeads(bd beads.Client, agentID string) []*beads.Issue {
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

// FindHookedBeadForAgent queries for the agent's current assignment bead.
// This is the authoritative source for what work a polecat is doing, since the
// work bead itself tracks status and assignee (hq-l6mm5).
//
// Both hooked AND in_progress are checked (hq-xa4z): polecats routinely claim
// their assignment with `bd update --status=in_progress` when starting work,
// which made a hooked-only lookup blind to the active assignment — the stale-
// branch guard and the hook fallback silently no-op'd (same class of bug as
// gt-pftz in the close path). Hooked wins over in_progress when both exist.
// Returns empty string if no assignment bead is found.
func FindHookedBeadForAgent(bd beads.Client, agentID string) string {
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
// Non-polecat actors have formats like: gastown/crew/name, mayor, etc.
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
