package polecat

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/git"
)

// LiveGitState is one polecat worktree's live git evidence: the three facts
// the reuse verdict re-derives from on every evaluation (claude-41j.1 D9).
//
// It exists so the verdict's primary source is measured the same way wherever
// it is measured. The Manager's reuse gate (workstateInputForPolecat) gathers
// the identical three facts inline and then overlays MR target refs onto
// UnpushedCommits; callers that need a verdict but have no MR-index to overlay
// (gt polecat list) probe through here instead.
type LiveGitState struct {
	// Branch is the worktree's current branch, empty when it could not be read.
	Branch string
	// Dirty is true when the worktree has uncommitted work, excluding the
	// runtime artifacts every polecat worktree carries (git.CleanExcludingRuntime).
	Dirty bool
	// DirtyReason is the human-readable blocker text for a dirty worktree.
	DirtyReason string
	// StashCount is the number of stash entries in the worktree.
	StashCount int
	// UnpushedCommits is the number of commits not preserved on the remote,
	// by branch preservation (git.UnpushedCommits, which compares against the
	// pushed branch and its upstream). It is therefore stricter than the
	// Manager's count, which also credits MR target refs.
	UnpushedCommits int
	// Source is GitStateSourceLive when the probe answered, or
	// GitStateSourceUnknown when it failed — never GitStateSourceRecorded,
	// which is reserved for callers that did not probe at all.
	Source string
	// FailedReason explains an unknown Source.
	FailedReason string
}

// ProbeLiveGitState measures one worktree path. It never returns an error:
// a failed check is itself a fact, and callers must fail closed on it rather
// than silently falling back to a recorded value (see RecordedCleanupBlocks).
func ProbeLiveGitState(worktreePath string) LiveGitState {
	if !IsWorktreeRoot(worktreePath) {
		// See IsWorktreeRoot: without this the upward resolution below would
		// measure the enclosing repository (the rig root, for a leftover
		// polecat directory) and report *its* branch, dirt, and stashes as this
		// polecat's — a "live" answer about the wrong tree, which is worse than
		// no answer.
		return liveGitUnknown(worktreePath, "resolving worktree root", errors.New("path is not a git worktree root"))
	}

	g := git.NewGit(worktreePath)

	branch, err := g.CurrentBranch()
	if err != nil {
		return liveGitUnknown(worktreePath, "reading branch", err)
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		return liveGitUnknown(worktreePath, "checking worktree state", err)
	}

	state := LiveGitState{
		Branch:          branch,
		StashCount:      status.StashCount,
		UnpushedCommits: status.UnpushedCommits,
		Source:          GitStateSourceLive,
	}
	if !status.CleanExcludingRuntime() {
		state.Dirty = true
		state.DirtyReason = fmt.Sprintf("git_state=has_uncommitted uncommitted_files=%d", len(status.NonRuntimePaths()))
	}
	return state
}

// IsWorktreeRoot reports whether path is itself the root of a git worktree,
// as opposed to either not being in a repository at all or merely sitting
// inside one.
//
// Git resolves upward, so a directory that is not a worktree root reports the
// enclosing repository's branch, dirt, and stashes. For a polecat that
// directory is a leftover (an incomplete nuke, a layout the workspace no
// longer uses); the enclosing repo is the rig root, and its state says nothing
// about the polecat. Measuring it would fabricate a confident live answer out
// of another tree's state, so callers treat "not a worktree root" as an
// unmeasurable worktree — which the verdict fails closed on.
func IsWorktreeRoot(path string) bool {
	if path == "" {
		return false
	}
	top, err := git.NewGit(path).TopLevel()
	if err != nil {
		return false
	}
	return sameWorktreePath(top, path)
}

// sameWorktreePath compares two worktree paths after resolving symlinks,
// because git reports the physical path while callers may hold a logical one
// (macOS: /var/folders/... vs /private/var/folders/...).
func sameWorktreePath(a, b string) bool {
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// liveGitUnknown is the fail-closed result: the probe was attempted, so the
// source is "unknown" (measured-but-failed), not "recorded" (never measured).
func liveGitUnknown(worktreePath, what string, err error) LiveGitState {
	return LiveGitState{
		Source:       GitStateSourceUnknown,
		FailedReason: fmt.Sprintf("git_state=unknown path=%s: %s failed: %v", worktreePath, what, err),
	}
}

// ApplyFacts copies a probe's facts onto the WorkstateFacts a caller is
// assembling, including the provenance label the verdict reads. Branch is
// copied only when the probe resolved one, so a failed probe leaves whatever
// recorded branch the caller already had rather than blanking it.
func (s LiveGitState) ApplyFacts(f *WorkstateFacts) {
	if s.Branch != "" {
		f.Branch = s.Branch
	}
	f.GitDirty = s.Dirty
	f.GitDirtyReason = s.DirtyReason
	f.StashCount = s.StashCount
	f.UnpushedCommits = s.UnpushedCommits
	f.GitStateSource = s.Source
	if s.Source == GitStateSourceUnknown {
		f.GitCheckFailed = true
		f.GitCheckFailedReason = s.FailedReason
	}
}
