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
	//
	// Which probe produced it decides where the "pushed branch" tip was read
	// from: ProbeLiveGitState asks the remote, ProbeLiveGitStateLocal reads the
	// local remote-tracking ref. Swapping probes can raise this count (a ref
	// this clone never fetched) or lower it (a ref that outlives its remote
	// branch) — git.BranchPreservationStatusLocal has both cases.
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
//
// It reads the branch's preservation from the remote (one network round trip).
// A caller measuring one worktree should use it; a caller measuring every seat
// in the town should use ProbeLiveGitStateLocal.
func ProbeLiveGitState(worktreePath string) LiveGitState {
	return probeLiveGitState(worktreePath, (*git.Git).CheckUncommittedWork)
}

// ProbeLiveGitStateLocal is ProbeLiveGitState without the network round trip:
// the branch-preservation fact is read from this clone's local remote-tracking
// refs instead of an ls-remote against the remote.
//
// The distinction is cost, and it is the difference between a listing that
// works and one that does not. `gt polecat list --all --json` probes one
// worktree per seat; with the networked probe that is an ls-remote — plus the
// remote-https helper it spawns — per seat, which measured 14.4s of a 15.0s
// run across 15 seats and is what put the command over 90s at the dashboard's
// 47 seats (gt-8q0s). Making those probes concurrent shrank that to 1.9s but
// left the round trips in place; the local probe removes them, and the pool
// then has only local subprocesses left to overlap. It keeps every other fact
// identical.
//
// The local unpushed count tracks the live one wherever this clone's view of
// the branch tip matches the remote's, and both directions of drift are real:
// a ref this clone never fetched reports the work as unpreserved, which flags
// a seat for recovery rather than clearing it, while a ref that outlives its
// remote branch reports it as preserved (gt-dt0k). Enumerating seats is a
// read-only inventory, which is the fidelity this buys — see
// git.BranchPreservationStatusLocal for the boundary and for the callers that
// need the live probe instead.
func ProbeLiveGitStateLocal(worktreePath string) LiveGitState {
	return probeLiveGitState(worktreePath, (*git.Git).CheckUncommittedWorkLocal)
}

// probeLiveGitState is the shared body of the two probes. The worktree check,
// the branch read, and the unknown/answered classification are identical; only
// how the unpushed-commit count is derived differs, and that is the single
// parameter, so the two fidelity levels cannot drift into disagreeing about
// what a probe measures.
func probeLiveGitState(worktreePath string, checkUncommittedWork func(*git.Git) (*git.UncommittedWorkStatus, error)) LiveGitState {
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

	status, err := checkUncommittedWork(g)
	if err != nil {
		return liveGitUnknown(worktreePath, "checking worktree state", err)
	}

	state := LiveGitState{
		Branch:          branch,
		StashCount:      status.StashCount,
		UnpushedCommits: status.UnpushedCommits,
		Source:          GitStateSourceLive,
	}
	if !status.CleanExcludingRuntimeAndIndexSkew(g) {
		state.Dirty = true
		state.DirtyReason = fmt.Sprintf("git_state=has_uncommitted uncommitted_files=%d", len(status.NonRuntimeNonSkewPaths(g)))
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
