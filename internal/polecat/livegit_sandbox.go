package polecat

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// SandboxSeatGitState is the gateway's git answer for a sandbox seat. It never
// reads the host worktree, because a sandbox polecat edits and commits inside
// its sandbox and the host copy goes stale.
//
//   - While the sandbox runs, or when the gateway errors, the work may live in
//     the sandbox and the host cannot know: the answer is unknown. A gateway
//     blip must never read as a gone sandbox, or a live one would look clean.
//   - Once the gateway affirmatively reports no sandbox of that name, only what
//     was pushed survives, and this answer is a clean seat with no measured
//     facts. A caller that can name the seat's host worktree replaces it with
//     the host's measurement (SandboxSeatHostGitState); a caller with no
//     worktree to measure (the gateway probe itself) keeps this answer. Clean
//     is not "safe to discard": a seat holding a bead with no pushed or resume
//     branch is still lost work, and the recovery verdict turns it into a
//     rework.
//
// branch is the host worktree's branch name, kept as a label only.
func SandboxSeatGitState(branch string, exists bool, existsErr error, sandboxName string) LiveGitState {
	switch {
	case existsErr != nil:
		return LiveGitState{
			Branch:       branch,
			Source:       GitStateSourceUnknown,
			FailedReason: fmt.Sprintf("could not ask the gateway whether sandbox %s exists: %v", sandboxName, existsErr),
		}
	case exists:
		return LiveGitState{
			Branch:       branch,
			Source:       GitStateSourceUnknown,
			FailedReason: fmt.Sprintf("git state lives in the running sandbox %s", sandboxName),
		}
	}
	return LiveGitState{Branch: branch, Source: GitStateSourceLive}
}

// SandboxSeatHostGitState judges a gone sandbox seat from what survives on the
// host: its worktree while that directory exists, the branch ref it left in the
// rig's shared repository once it does not. A worktree's HEAD is compared with
// the pushed branch on origin by patch identity (git cherry) and its dirt and
// stashes are read too; a shared repository's copy of the seat's local branch
// is compared the same way. Commits already on the pushed branch and a clean
// worktree read clean and pushed; a unique commit, a stash or dirt keeps the
// seat blocked with the fact that blocks it. A path the caller could not
// resolve, a branch ref that cannot be read, a shared repository that cannot be
// located, or an unreadable comparison is unknown, which is never clean. A
// running sandbox or a gateway error passes through unchanged. worktreePath is
// the seat's host clone; rigRoot holds the shared repository that survives the
// clone; pushedBranch is the branch the sandbox pushed (empty compares against
// the worktree's own branch while it exists, and names no local ref once it is
// gone, which leaves the lost-work rule to judge the seat).
func SandboxSeatHostGitState(gone LiveGitState, worktreePath, rigRoot, pushedBranch string) LiveGitState {
	return sandboxSeatHostGitState(gitOpener{}, gone, worktreePath, rigRoot, pushedBranch)
}

// sandboxSeatHostGitState is SandboxSeatHostGitState over the injected git
// seam. Only a gone seat (a live source) is measured; every other answer is
// returned unchanged, so a caller may pass any sandbox seat's state.
func sandboxSeatHostGitState(gits gitOpener, gone LiveGitState, worktreePath, rigRoot, pushedBranch string) LiveGitState {
	if gone.Source != GitStateSourceLive {
		return gone
	}
	return sandboxSeatGoneHostGitState(gits, worktreePath, rigRoot, pushedBranch, gone.Branch)
}

// sandboxSeatGoneHostGitState is the host measurement behind
// SandboxSeatHostGitState. An unresolved path, or one that is present but
// cannot be read, is unknown, never clean, because a comparison that did not
// run says nothing about the work it holds. A worktree directory that is gone
// is judged from the shared repository the rig still holds: the seat's branch
// ref outlives its worktree, and a commit only that ref holds has no equivalent
// on the pushed branch (gt-zgoo3).
func sandboxSeatGoneHostGitState(gits gitOpener, worktreePath, rigRoot, pushedBranch, label string) LiveGitState {
	path := strings.TrimSpace(worktreePath)
	if path == "" {
		return liveGitUnknown("(none)", "resolving the host worktree",
			errors.New("the caller resolved no host worktree path"))
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return sandboxSeatGoneSharedRepoGitState(gits, path, rigRoot, pushedBranch, label)
		}
		return liveGitUnknown(path, "reading the host worktree", err)
	}
	if !isWorktreeRoot(gits, path) {
		// Git resolves upward, so a directory that is not a worktree root would
		// answer with the enclosing repository's branch and commits — a "live"
		// verdict about the wrong tree. Fail closed instead.
		return liveGitUnknown(path, "resolving worktree root", errors.New("path is not a git worktree root"))
	}

	g := gits.Open(path)
	hostBranch, err := g.CurrentBranch()
	if err != nil {
		return liveGitUnknown(path, "reading branch", err)
	}
	// The label stays a label: the agent bead's recorded branch names what the
	// sandbox pushed, and replacing it with the stale host clone's branch would
	// mislabel the seat. Only the comparison falls back to the host branch.
	pushed := strings.TrimSpace(pushedBranch)
	if pushed == "" {
		pushed = hostBranch
	}

	// Patch identity, not ancestry: a commit whose patch is already on the
	// pushed branch is not unique work, however its SHA was rewritten. When the
	// pushed branch is not on this clone's view of origin — the seat pushed
	// nothing, or the branch was pruned after landing — the remote's default
	// branch is the comparison that still proves the host's commits are not
	// unique; a missing default branch too fails the comparison, and unknown is
	// never clean.
	ref := "origin/" + pushed
	if _, err := g.Rev(ref); err != nil {
		ref = "origin/" + g.RemoteDefaultBranch()
	}
	preservation, err := g.RefPreservedByRef("HEAD", ref)
	if err != nil {
		return liveGitUnknown(path, "comparing HEAD with "+ref, err)
	}
	status, err := g.CheckUncommittedWork()
	if err != nil {
		return liveGitUnknown(path, "checking worktree state", err)
	}

	state := LiveGitState{
		Branch:          strings.TrimSpace(label),
		StashCount:      status.StashCount,
		UnpushedCommits: preservation.UnpreservedPatchCount,
		Source:          GitStateSourceLive,
	}
	if !status.CleanExcludingRuntimeAndIndexSkew(g) {
		state.Dirty = true
		state.DirtyReason = fmt.Sprintf("git_state=has_uncommitted uncommitted_files=%d", len(status.NonRuntimeNonSkewPaths(g)))
	}
	return state
}

// sandboxSeatGoneSharedRepoGitState judges a gone seat whose host worktree
// directory is missing by the branch ref that worktree leaves in the rig's
// shared repository. A seat with no ref there has nothing unpushed; a ref whose
// commits have no equivalent patch on the pushed branch keeps them counted. A
// recorded branch that cannot name a ref, a rig with no repository, and a
// comparison that cannot be read are all unknown, never clean (gt-zgoo3).
func sandboxSeatGoneSharedRepoGitState(gits gitOpener, worktreePath, rigRoot, pushedBranch, label string) LiveGitState {
	branch := strings.TrimSpace(pushedBranch)
	if branch == "" {
		// No branch was recorded, so no ref is named to compare. The
		// lost-work rule is the guard for such a seat, and it fails closed
		// unless the issue landed with nothing left on origin (gt-xhnn3).
		return LiveGitState{Branch: strings.TrimSpace(label), Source: GitStateSourceLive}
	}
	if branch == "HEAD" || strings.ContainsAny(branch, " \t\n\r") {
		return liveGitUnknown(worktreePath, "naming the seat's local branch",
			errors.New("the recorded branch names no local ref"))
	}
	root := strings.TrimSpace(rigRoot)
	if root == "" {
		return liveGitUnknown(worktreePath, "locating the shared repository",
			errors.New("the caller named no rig root"))
	}
	repo := rigGitRepo(root)
	if repo == "" {
		return liveGitUnknown(worktreePath, "locating the shared repository",
			errors.New("no .repo.git or mayor/rig under the rig root"))
	}
	g := openRigRepo(gits, repo)

	localRef := "refs/heads/" + branch
	exists, err := g.RefExists(localRef)
	if err != nil {
		return liveGitUnknown(worktreePath, "reading "+localRef, err)
	}
	if !exists {
		return LiveGitState{Branch: strings.TrimSpace(label), Source: GitStateSourceLive}
	}

	// The pushed branch is read from the shared repo's own refs; one it never
	// fetched falls back to the remote's default branch, which can keep
	// already-pushed commits counted. That over-counts (blocks a recoverable
	// seat) rather than clearing a seat holding unpushed work.
	ref := "origin/" + branch
	if _, err := g.Rev(ref); err != nil {
		ref = "origin/" + g.RemoteDefaultBranch()
	}
	preservation, err := g.RefPreservedByRef(localRef, ref)
	if err != nil {
		return liveGitUnknown(worktreePath, "comparing "+localRef+" with "+ref, err)
	}
	return LiveGitState{
		Branch:          strings.TrimSpace(label),
		UnpushedCommits: preservation.UnpreservedPatchCount,
		Source:          GitStateSourceLive,
	}
}
