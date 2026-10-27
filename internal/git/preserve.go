package git

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrBranchNotPreserved is matched (errors.Is) by a preserve that ran but could
// not put the tip on the remote: the push was refused, or the remote tip is not
// the one pushed. A caller that sees it knows the work exists only locally.
// Errors from an earlier step (resolving the branch, fetching, making the
// checkout) do not carry it: those say the preserve could not be judged, not
// that the remote lacks the tip.
var ErrBranchNotPreserved = errors.New("branch not preserved on remote")

// PreserveResult is where a preserve push left a branch's tip on the remote.
type PreserveResult struct {
	// RemoteRef is the remote-tracking ref confirmed to contain the tip, e.g.
	// "origin/main" or "origin/polecat/x-1a2b3c4".
	RemoteRef string
	// AlreadyThere is true when the tip was reachable from a remote-tracking
	// branch before this call, so nothing was pushed.
	AlreadyThere bool
}

// preserveUnsetEnv is the environment every preserve command is run without.
// An inherited GIT_DIR (or GIT_WORK_TREE) points git at another repository, and
// a preserve push that lands in the wrong repository reports work preserved
// that is not (rpkg #769).
var preserveUnsetEnv = []string{"GIT_DIR", "GIT_WORK_TREE"}

// PreserveBranch pushes branch's tip to remote from a checkout whose HEAD is
// branch, and reports the remote ref that holds it; it returns an error unless
// it confirmed one. A push that did not leave the tip on the remote wraps
// ErrBranchNotPreserved; an error before the push (the branch cannot be
// resolved, the remote fetched, a checkout made) is returned as it happened.
func (g *Git) PreserveBranch(branch, remote, workRoot string) (PreserveResult, error) {
	g = g.WithoutEnv(preserveUnsetEnv...)

	ref := "refs/heads/" + branch
	tip, err := g.Rev(ref)
	if err != nil {
		return PreserveResult{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	tip = strings.TrimSpace(tip)
	if tip == "" {
		return PreserveResult{}, fmt.Errorf("resolve %s: empty sha", ref)
	}

	// Refresh remote-tracking refs before deciding anything: every check below
	// reads the remote's state, and a stale origin/main is how work that exists
	// only locally gets classified as preserved. Prune too: a branch deleted
	// unmerged on the remote leaves a stale refs/remotes/<remote>/<branch> at
	// the tip, and the daemon's prune would drop it right after the delete.
	if err := g.FetchPrune(remote); err != nil {
		return PreserveResult{}, fmt.Errorf("fetch %s before preserve: %w", remote, err)
	}
	// An unreadable answer is not a verdict here: control falls through to the
	// push, which verifies the tip on the remote and cannot preserve less than
	// this check would.
	if refs, err := g.RemoteRefsContaining(tip); err == nil && len(refs) > 0 {
		return PreserveResult{RemoteRef: refs[0], AlreadyThere: true}, nil
	}

	push, cleanup, err := g.preserveCheckout(branch, workRoot)
	if err != nil {
		return PreserveResult{}, err
	}
	defer cleanup()

	// The branch's own name first: the merge queue and the refinery look for
	// the work there.
	pushTo := branch
	if err := push.Push(remote, branch+":"+pushTo, false); err != nil {
		// Non-fast-forward: the remote branch is at a commit this tip is not a
		// descendant of. A side ref cannot conflict, so it is the fallback —
		// not a reason to delete the branch anyway.
		pushTo = branch + "-" + preserveSuffix(tip)
		if fallbackErr := push.Push(remote, branch+":"+pushTo, false); fallbackErr != nil {
			return PreserveResult{}, fmt.Errorf("%w: push %s:%s failed (%v) and fallback push %s:%s failed (%v)",
				ErrBranchNotPreserved, branch, branch, err, branch, pushTo, fallbackErr)
		}
	}

	// Never trust the push exit code alone — the entire point of this step is
	// to know the work is on the remote before the only other copy goes.
	if err := push.VerifyPushedCommit(remote, pushTo, tip); err != nil {
		return PreserveResult{}, fmt.Errorf("%w: %w", ErrBranchNotPreserved, err)
	}
	return PreserveResult{RemoteRef: remote + "/" + pushTo}, nil
}

// preserveCheckout returns a handle whose HEAD is branch, and the cleanup that
// undoes whatever it made. A worktree already standing on the branch is used as
// it is; when none does, a worktree of branch is added under workRoot and the
// cleanup removes it, on success and on every error path.
//
// The HEAD is what makes the push legal. `git push <remote> <branch>` pushes
// the local ref whatever HEAD is, and the repo's pre-push guard refuses that
// push from a session standing on another branch — which is what the bare
// repository (HEAD on the default branch) and a reused worktree are, so their
// pushes were refused and the seat could not be replaced (gt-7cps5).
func (g *Git) preserveCheckout(branch, workRoot string) (*Git, func(), error) {
	trees, err := g.WorktreeList()
	if err != nil {
		return nil, func() {}, fmt.Errorf("list worktrees: %w", err)
	}
	for _, tree := range trees {
		if tree.Path == "" {
			continue
		}
		checkout := g.checkoutAt(tree.Path)
		head, err := checkout.CurrentBranch()
		if err != nil || head != branch {
			continue
		}
		return checkout, func() {}, nil
	}

	// A registration whose directory is gone still blocks `worktree add` ("is
	// already used by worktree at ..."), so drop those first.
	if err := g.WorktreePrune(); err != nil {
		return nil, func() {}, fmt.Errorf("prune worktrees: %w", err)
	}
	dir, err := preserveTempDir(workRoot)
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() {
		_ = g.WorktreeRemove(dir, true)
		_ = g.WorktreePrune()
		_ = os.RemoveAll(dir)
	}
	if err := g.WorktreeAddExistingNoSubmodules(dir, branch); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("add a worktree of %s at %s: %w", branch, dir, err)
	}
	return g.checkoutAt(dir), cleanup, nil
}

// checkoutAt returns a copy of g that runs in dir as a checkout. The bare
// repository's --git-dir is dropped: it overrides repository discovery, so a
// handle carrying it would read the bare repository's HEAD from inside the
// checkout and the push would run from the wrong one. The runner, the deadline
// and the environment settings carry over.
func (g *Git) checkoutAt(dir string) *Git {
	c := *g
	c.workDir = dir
	c.gitDir = ""
	return &c
}

// preserveTempDir makes the directory a temporary preserve worktree is added
// at, under workRoot, creating workRoot when it does not exist yet. Both
// failures are one answer: without a writable directory under workRoot there is
// nowhere to put the checkout.
func preserveTempDir(workRoot string) (string, error) {
	var (
		dir string
		err error
	)
	if err = os.MkdirAll(workRoot, 0o700); err == nil {
		dir, err = os.MkdirTemp(workRoot, "preserve-")
	}
	if err != nil {
		return "", fmt.Errorf("temporary worktree under %s: %w", workRoot, err)
	}
	return dir, nil
}

// preserveSuffix is the short-sha suffix of a fallback preserve ref
// (<branch>-<sha7>): enough to identify the commit, short enough to read.
func preserveSuffix(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
