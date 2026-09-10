package cmd

import (
	"fmt"
)

// divergedPushGit is the subset of *git.Git that recoverDivergedPush needs.
type divergedPushGit interface {
	Fetch(remote string) error
	Rev(ref string) (string, error)
	MergeBase(a, b string) (string, error)
	PatchID(base, head string) (string, error)
	PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error
}

// recoverDivergedPush runs after a plain (non-force) push of branch to remote
// fails as non-fast-forward. That happens whenever local history was rebased
// after an earlier dispatch already pushed this same branch to origin — the
// branch-reuse formula step rebases onto origin/target unconditionally, and
// gt done's own contamination-triggered auto-rebase can miss a prior push
// from a different session/checkpoint. Either way, origin's tip and the new
// local tip usually carry the identical change-set, just rebased onto a
// newer base — not real work loss (gt-bf5x).
//
// This compares the patch-id of origin's tip against local HEAD, each
// relative to its own merge-base with target (patch-id is base-invariant, so
// a pure rebase with no new changes produces identical ids). When they
// match, it force-with-leases local HEAD over the observed origin SHA —
// leased, not blind, so a genuinely concurrent push (not just this rebase)
// still aborts instead of being clobbered. When they don't match, nothing is
// pushed; diagnosis explains why so callers can report it instead of a bare
// "possible work loss".
//
// Returns recovered=true only when the leased push actually landed.
// diagnosis is set whenever the comparison could be made, for inclusion in
// error/alarm messages either way.
func recoverDivergedPush(g divergedPushGit, remote, refspec, branch, target string) (recovered bool, diagnosis string, err error) {
	// A failed push doesn't itself update the local remote-tracking ref, so
	// without a fresh fetch here origin/<branch> could still be whatever was
	// last observed — stale enough to misjudge the comparison below (this is
	// also the mechanism behind the separate stale-read false alarm the
	// mayor flagged on garnet/gt-en6o).
	if err := g.Fetch(remote); err != nil {
		return false, "", fmt.Errorf("fetch %s before divergence check: %w", remote, err)
	}

	originSHA, revErr := g.Rev(remote + "/" + branch)
	if revErr != nil {
		return false, "", fmt.Errorf("%s/%s has no ref: %w", remote, branch, revErr)
	}
	localSHA, revErr := g.Rev("HEAD")
	if revErr != nil {
		return false, "", fmt.Errorf("resolve local HEAD: %w", revErr)
	}

	originBase, err := g.MergeBase(target, originSHA)
	if err != nil {
		return false, "", fmt.Errorf("merge-base(%s, origin tip): %w", target, err)
	}
	localBase, err := g.MergeBase(target, localSHA)
	if err != nil {
		return false, "", fmt.Errorf("merge-base(%s, local HEAD): %w", target, err)
	}

	originPatchID, err := g.PatchID(originBase, originSHA)
	if err != nil {
		return false, "", fmt.Errorf("patch-id of origin tip: %w", err)
	}
	localPatchID, err := g.PatchID(localBase, localSHA)
	if err != nil {
		return false, "", fmt.Errorf("patch-id of local HEAD: %w", err)
	}

	if originPatchID == "" || originPatchID != localPatchID {
		return false, fmt.Sprintf("origin=%s local=%s NOT patch-identical (origin patch-id %s vs local %s) — real divergence, not just a rebase",
			shortSHA(originSHA), shortSHA(localSHA), originPatchID, localPatchID), nil
	}

	diagnosis = fmt.Sprintf("diverged by rebase, content identical (origin=%s local=%s patch-id=%s)",
		shortSHA(originSHA), shortSHA(localSHA), originPatchID)

	if leaseErr := g.PushForceWithLease(remote, refspec, branch, originSHA); leaseErr != nil {
		return false, diagnosis, fmt.Errorf("leased force-push failed: %w", leaseErr)
	}
	return true, diagnosis, nil
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// rebaseGit is the subset of *git.Git that autoRebaseOnTarget needs. Defined as
// an interface so tests can drive the decision logic without standing up a full
// git repo for every gating case.
type rebaseGit interface {
	Rebase(onto string) error
	AbortRebase() error
}

// autoRebaseOnTarget rebases the current branch onto base when the branch is
// behind the target. It is a no-op when there is nothing to rebase, when the
// polecat ran the formula's pre-verify step (rebasing again would invalidate
// the gate results that --pre-verified attests to), or when a prior push
// checkpoint exists (rebasing after pushing would require a force-push).
//
// Returns:
//   - rebased: true if a rebase actually ran successfully.
//   - skipReason: non-empty when behind > 0 but the rebase was intentionally
//     skipped. Empty when behind == 0 (no rebase needed) or when rebased == true.
//   - err: rebase failure, after AbortRebase has been attempted to clean up.
//
// gh#3400.
func autoRebaseOnTarget(g rebaseGit, base string, behind int, preVerified, alreadyPushed bool) (rebased bool, skipReason string, err error) {
	if behind <= 0 {
		return false, "", nil
	}
	switch {
	case preVerified:
		return false, "--pre-verified is set", nil
	case alreadyPushed:
		return false, "prior push checkpoint exists", nil
	}

	fmt.Printf("→ Auto-rebasing onto %s (%d commits behind)\n", base, behind)
	if rebaseErr := g.Rebase(base); rebaseErr != nil {
		_ = g.AbortRebase()
		return false, "", fmt.Errorf("auto-rebase onto %s failed: %w\n"+
			"Resolve conflicts manually (git fetch origin && git rebase %s), commit the resolution, then rerun gt done.",
			base, rebaseErr, base)
	}
	return true, "", nil
}
