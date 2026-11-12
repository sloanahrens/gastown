package done

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/workspace"
)

// A crew session whose worktree is not in this process (a pod, where the
// caller's Mac path does not exist) names the submission with --branch and
// --head instead. The branch and head are the facts the worktree would have
// supplied, so nothing in the worktree is read: the rig's canonical clone
// reads origin/<branch> and the head commit, and the local gate cannot run
// here (gt-pyh5p6).

// crewFlagFacts validates the --branch and --head pair. Both or neither: a
// lone flag names no submission. The branch is passed to the remote query, so
// it must not read as an option or hold a space. The head is compared with
// origin's full tip, which a short prefix could never equal, so it is refused
// here rather than failing later as an unverified push.
func crewFlagFacts(opts Options) (branch, head string, err error) {
	branch, head = strings.TrimSpace(opts.Branch), strings.TrimSpace(opts.Head)
	if branch == "" || head == "" {
		return "", "", fmt.Errorf("gt done: --branch and --head go together; give both, or neither")
	}
	if strings.HasPrefix(branch, "-") || strings.IndexFunc(branch, unicode.IsSpace) >= 0 || strings.IndexFunc(branch, unicode.IsControl) >= 0 {
		return "", "", fmt.Errorf("gt done: --branch %q is not a branch name", branch)
	}
	if !isFullCommitID(head) {
		return "", "", fmt.Errorf("gt done: --head %q is not a full commit id (40 or 64 lowercase hex digits)", head)
	}
	return branch, head, nil
}

// isFullCommitID reports whether s is a complete SHA-1 or SHA-256 commit id
// in lowercase hex, the form origin reports a tip in.
func isFullCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// runDoneCrewFromFlags submits a crew branch named by --branch and --head.
// It reads the town and rig from the environment and the caller's directory
// name, never the worktree, and runs the submission against the rig's
// canonical clone.
func runDoneCrewFromFlags(opts Options, getenv func(string) string) error {
	branch, head, err := crewFlagFacts(opts)
	if err != nil {
		return err
	}
	if err := doneRejectGitEnvOverrides(getenv); err != nil {
		return err
	}

	// The caller's directory may be absent here, so it only names a town
	// when a workspace marker is found above it; GT_TOWN_ROOT covers the rest.
	cwd, err := opts.getwd()()
	if err != nil {
		return fmt.Errorf("gt done: current directory unavailable: %w", err)
	}
	townRoot, _ := workspace.Find(cwd)
	if townRoot == "" {
		townRoot = workspace.TownRootFromEnv(getenv)
	}
	if townRoot == "" {
		return fmt.Errorf("gt done: not in a Gas Town workspace and GT_TOWN_ROOT is unset")
	}

	issueID := firstNonEmpty(opts.Bead, opts.Issue)
	if issueID == "" {
		issueID = crewBeadFromBranch(branch, func(prefix string) bool {
			return beads.GetRigNameForPrefix(townRoot, prefix) != ""
		})
	}
	if issueID == "" {
		return fmt.Errorf("gt done: crew branch %s names no bead; pass --bead <id>", branch)
	}

	rigName := strings.TrimSpace(getenv("GT_RIG"))
	if rigName == "" {
		rigName = beads.GetRigNameForPrefix(townRoot, beads.ExtractPrefix(issueID))
	}
	if rigName == "" || rigName == "." || rigName == ".." || strings.ContainsAny(rigName, `/\`) {
		return fmt.Errorf("gt done: no rig names bead %s; set GT_RIG to the rig", issueID)
	}

	cloneDir := filepath.Join(townRoot, rigName, "mayor", "rig")
	var repo doneRepo = git.NewGit(cloneDir)
	if opts.CloneRepo != nil {
		repo = opts.CloneRepo(cloneDir)
	}
	// The worktree's git config is not this process's to read, so a session
	// that names no agent gets no committer fallback: checkCrewSubmitRun
	// refuses it before any write.
	r := newCrewDoneRun(getenv, func() string { return "" }, cloneDir, townRoot, rigName, branch, issueID)
	r.opts = doneOptions{target: opts.Target}
	if def := crewRigDefaultBranch(townRoot, rigName); def != "" {
		r.defaultBranch = def
	}
	r.deps = crewSubmitDeps(r, opts, repo)
	return submitCrewAbsentForLanding(r, head)
}

// submitCrewAbsentForLanding marks a crew branch ready to land when the
// worktree is not in this process. origin/<branch> must be at head, read
// from the run's repo, the rig's canonical clone. The worktree's own gate
// cannot run here, so the output says so and the bead is marked the same way
// the gated path marks it.
func submitCrewAbsentForLanding(r *doneRun, head string) error {
	if err := checkCrewSubmitRun(r); err != nil {
		return err
	}
	issue, bd, err := r.deps.source(r.issueID)
	if err != nil {
		return fmt.Errorf("source issue validation failed: %w", err)
	}
	if bd == nil {
		return fmt.Errorf("no beads client for %s", r.issueID)
	}
	target, err := resolveDoneTarget(r, issue)
	if err != nil {
		return err
	}
	if err := r.deps.repo.VerifyPushedCommit("origin", r.branch, head); err != nil {
		return doneExit(doneExitPushUnverified,
			fmt.Sprintf("origin/%s is not at %s; push it from your worktree on the Mac first (git push origin HEAD:%s), then re-run gt done", r.branch, ShortSHA(head), r.branch), err)
	}
	if err := refuseUncheckedCriteria(r.issueID, issue); err != nil {
		return err
	}
	if err := refuseHumanIdentity(r, head); err != nil {
		return err
	}
	fmt.Fprintln(r.stdout(), "presubmit not run: the worktree is not in this process; run make presubmit on the Mac first")
	return markCrewSubmitted(r, issue, bd, target, head)
}
