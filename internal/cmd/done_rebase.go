package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// divergedPushGit is the subset of *git.Git that recoverDivergedPush needs.
type divergedPushGit interface {
	Fetch(remote string) error
	Rev(ref string) (string, error)
	MergeBase(a, b string) (string, error)
	PatchID(base, head string) (string, error)
	FirstParentPatchIDs(base, head string) ([]git.PatchIDCommit, error)
	PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error
}

// recoverDivergedPush runs after a plain (non-force) push of branch to remote
// fails as non-fast-forward. That happens whenever local history was rebased
// after an earlier dispatch already pushed this same branch to origin — the
// branch-reuse formula step rebases onto origin/target unconditionally, and
// gt done's own contamination-triggered auto-rebase can miss a prior push
// from a different session/checkpoint. Either way, origin's tip and the new
// local tip usually carry the same work, just rebased onto a newer base — not
// real work loss (gt-bf5x).
//
// Two shapes of "same work" are safe to lease-over:
//
//   - Pure rebase: the two tips' ranges have the identical patch-id, so the
//     content is unchanged (this also covers a branch squashed or amended into
//     a different commit split on one side, where no individual commit is
//     shared but the total diff is).
//   - Rework after redispatch: local adds fix commits on top of the commits
//     origin already has (mol-polecat-work's branch-reuse step rebases *and*
//     re-implements), so origin's per-commit patch-ids are all present locally
//     even though the range patch-ids differ. Nothing on origin is lost by
//     pushing over it (gt-i0z3).
//
// This compares origin's patch-ids against local's, each relative to its own
// merge-base with target (patch-id is base-invariant, so a rebase with no new
// changes produces identical ids). When every origin change is accounted for
// locally, it force-with-leases local HEAD over the observed origin SHA —
// leased, not blind, so a genuinely concurrent push (not just this rebase)
// still aborts instead of being clobbered. When origin holds a change local
// does not, nothing is pushed; diagnosis explains why so callers can report it
// instead of a bare "possible work loss".
//
// The per-commit ids come from FirstParentPatchIDs, not PatchIDs: a merge
// commit carries real content of its own (whatever a manual conflict
// resolution folded in is not attached to any individual commit), and
// PatchIDs' --no-merges walk drops that content from the comparison
// entirely — origin could hold a merge commit's unique content and this
// function would call it "no origin work at risk" and lease over it. Walking
// --first-parent instead keeps merge commits in the id set, each keyed to its
// diff against its first parent (which is exactly a merge's net content, per
// FirstParentPatchIDs' doc). For the common case of a linear, rebase-only
// range the two functions agree commit-for-commit, so this changes nothing
// there (gt-5wp5).
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

	originPairs, err := g.FirstParentPatchIDs(originBase, originSHA)
	if err != nil {
		return false, "", fmt.Errorf("patch-ids of origin range %s..%s: %w", originBase, originSHA, err)
	}
	localPairs, err := g.FirstParentPatchIDs(localBase, localSHA)
	if err != nil {
		return false, "", fmt.Errorf("patch-ids of local range %s..%s: %w", localBase, localSHA, err)
	}
	originIDs := patchIDsOf(originPairs)
	localIDs := patchIDsOf(localPairs)

	missing := patchIDsMissingFrom(originIDs, localIDs)

	// Whole-range equality is the only signal that survives a squash/amend:
	// the patch-id of "A then B" is not the patch-id of "A and B squashed", so
	// the per-commit comparison above calls that a loss even though the two
	// branches carry byte-identical changes. Best-effort — PatchID errors on
	// an empty range (nothing to hash), and a failure here just leaves the
	// per-commit comparison to decide alone.
	rangeIdentical := false
	if len(originIDs) > 0 && len(localIDs) > 0 {
		if originPatchID, oErr := g.PatchID(originBase, originSHA); oErr == nil {
			if localPatchID, lErr := g.PatchID(localBase, localSHA); lErr == nil {
				// Guard the empty id explicitly: an unset value must never be
				// read as "identical" — "no patch-id here" is not evidence of
				// matching content.
				rangeIdentical = originPatchID != "" && originPatchID == localPatchID
			}
		}
	}

	switch {
	case len(missing) > 0 && !rangeIdentical:
		return false, fmt.Sprintf("origin=%s local=%s — origin has %d commit(s) whose changes are absent locally (%s) — real divergence, not just a rebase; nothing pushed so that work is not clobbered",
			shortSHA(originSHA), shortSHA(localSHA), len(missing), strings.Join(missing, ", ")), nil
	case len(originIDs) == 0:
		diagnosis = fmt.Sprintf("no origin work at risk: origin/%s (%s) carries no commits of its own against %s (origin=%s local=%s)",
			branch, shortSHA(originSHA), target, shortSHA(originSHA), shortSHA(localSHA))
	case rangeIdentical || len(originIDs) == len(localIDs):
		diagnosis = fmt.Sprintf("diverged by rebase, content identical (origin=%s local=%s, origin %d commit(s) vs local %d, same change-set)",
			shortSHA(originSHA), shortSHA(localSHA), len(originIDs), len(localIDs))
	default:
		diagnosis = fmt.Sprintf("diverged by rework-redispatch, no origin work lost (origin=%s local=%s: origin's %d commit(s) are all present in local's %d — %d new local commit(s))",
			shortSHA(originSHA), shortSHA(localSHA), len(originIDs), len(localIDs), len(localIDs)-len(originIDs))
	}

	if leaseErr := g.PushForceWithLease(remote, refspec, branch, originSHA); leaseErr != nil {
		return false, diagnosis, fmt.Errorf("leased force-push failed: %w", leaseErr)
	}
	return true, diagnosis, nil
}

// patchIDsOf extracts the patch-id half of each pair, in the same order
// FirstParentPatchIDs returned them. The paired commit sha is only needed to
// name a landing commit elsewhere; the divergence comparison here only ever
// needs the ids.
func patchIDsOf(pairs []git.PatchIDCommit) []string {
	ids := make([]string, len(pairs))
	for i, p := range pairs {
		ids[i] = p.PatchID
	}
	return ids
}

// patchIDsMissingFrom returns the patch-ids present in want but not in have,
// as a multiset difference: a change applied twice on the origin side needs
// two matching entries locally to count as accounted for. An empty result
// means every change want carries is also in have.
func patchIDsMissingFrom(want, have []string) []string {
	unmatched := make(map[string]int, len(have))
	for _, id := range have {
		unmatched[id]++
	}
	var missing []string
	for _, id := range want {
		if unmatched[id] > 0 {
			unmatched[id]--
			continue
		}
		missing = append(missing, id)
	}
	return missing
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

// pushedBranchGit is the subset of *git.Git that branchAlreadyOnRemote needs.
type pushedBranchGit interface {
	FetchBranch(remote, branch string) error
	Rev(ref string) (string, error)
}

// branchAlreadyOnRemote reports whether branch already exists on remote, along
// with a reason naming the ref that proves it — for skip messages that must
// distinguish "I pushed it earlier in this session" from "a previous dispatch
// left it there".
//
// refresh asks for a fetch of that one ref first. Callers normally fetched
// earlier, but only the remote behind their target branch: in a fork-backed
// rig that is upstream, which leaves origin/<branch> — the ref every branch
// push here actually targets — stale enough to miss a push from an earlier
// dispatch and wrongly conclude the branch is new (gt-i0z3). A fetch failure
// is not an error: the branch not existing yet is the ordinary case (nothing
// has been pushed), and a network failure is not evidence either way, so both
// fall back to the last local value.
func branchAlreadyOnRemote(g pushedBranchGit, remote, branch string, refresh bool) (exists bool, reason string) {
	ref := remote + "/" + branch
	if refresh {
		_ = g.FetchBranch(remote, branch)
	}
	if _, err := g.Rev(ref); err != nil {
		return false, ""
	}
	return true, fmt.Sprintf("%s already exists on %s from an earlier dispatch", ref, remote)
}

// autoRebaseOnTarget rebases the current branch onto base when the branch is
// behind the target. It is a no-op when there is nothing to rebase, when the
// polecat ran the formula's pre-verify step (rebasing again would invalidate
// the gate results that --pre-verified attests to), or when the branch is
// already on the remote (rebasing after pushing would require a force-push).
//
// pushedReason names *what* proves the branch is already on the remote — a
// checkpoint this session wrote, or a ref that exists from an earlier dispatch
// (the branch-reuse path). It is the skip reason when non-empty, so the
// warning the polecat sees says which one (gt-i0z3: it used to always claim
// "checkpoint", even when the checkpoint played no part).
//
// Returns:
//   - rebased: true if a rebase actually ran successfully.
//   - skipReason: non-empty when behind > 0 but the rebase was intentionally
//     skipped. Empty when behind == 0 (no rebase needed) or when rebased == true.
//   - err: rebase failure, after AbortRebase has been attempted to clean up.
//
// gh#3400.
func autoRebaseOnTarget(g rebaseGit, base string, behind int, preVerified bool, pushedReason string) (rebased bool, skipReason string, err error) {
	if behind <= 0 {
		return false, "", nil
	}
	switch {
	case preVerified:
		return false, "--pre-verified is set", nil
	case pushedReason != "":
		return false, pushedReason, nil
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
