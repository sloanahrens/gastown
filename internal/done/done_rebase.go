package done

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

// divergedPushGit is the subset of *git.Git that recoverDivergedPush needs.
type divergedPushGit interface {
	Fetch(remote string) error
	FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error
	Rev(ref string) (string, error)
	MergeBase(a, b string) (string, error)
	PatchID(base, head string) (string, error)
	FirstParentPatchIDs(base, head string) ([]git.PatchIDCommit, error)
	FirstParentCommitSummaries(base, head string) ([]git.CommitSummary, error)
	FullRangeCommitSummaries(base, head string) ([]git.CommitSummary, error)
	PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error
}

// recoverDivergedPush runs when origin's tip for branch is not an ancestor of
// the local head: gt done's pushBranchToOrigin calls it before pushing, and
// the tap guard uses it for pushes a plain push refused. That happens whenever
// local history was rebased after an earlier dispatch already pushed this same
// branch to origin — gt done always rebases onto the target, and so does the
// branch-reuse formula step. Usually origin's tip and the new local tip carry
// the same work, just rebased onto a newer base — not real work loss
// (gt-bf5x).
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
// A third shape is safe to lease-over only for a bead in rework: origin's tip
// is the rejected head and holds only this seat's commits (see reworkPush), and
// local HEAD carries at least one commit origin lacks. A content-changing
// rework alters patch-ids, so the comparison above would call it divergence;
// that tip is the refused submission, so overwriting it is the point of the
// rework (gt-dq6p8). A stale local behind that tip is not a rework (gt-k44d0).
//
// Returns recovered=true only when the leased push actually landed.
// diagnosis is set whenever the comparison could be made, for inclusion in
// error/alarm messages either way.
func recoverDivergedPush(g divergedPushGit, remote, refspec, branch, target string, rework reworkPush) (recovered bool, diagnosis string, err error) {
	// A failed push doesn't itself update the local remote-tracking ref, so
	// without a fresh fetch here origin/<branch> could still be whatever was
	// last observed — stale enough to misjudge the comparison below (this is
	// also the mechanism behind the separate stale-read false alarm on
	// garnet/gt-en6o).
	if err := g.Fetch(remote); err != nil {
		return false, "", fmt.Errorf("fetch %s before divergence check: %w", remote, err)
	}
	// The bare fetch runs only the configured refspecs, and a polecat clone
	// names only main in them, so origin/<branch> is refreshed by name here.
	ownRefspec := "+refs/heads/" + branch + ":refs/remotes/" + remote + "/" + branch
	if err := g.FetchRefspecWithTimeout(remote, ownRefspec, git.RemoteQueryTimeout); err != nil {
		if !isMissingRemoteRef(err) {
			return false, "", fmt.Errorf("fetch %s/%s before divergence check: %w", remote, branch, err)
		}
		// origin landed and deleted the branch: it has no tip to compare, so
		// this is the no-ref outcome, not a fetch failure.
		return false, "", fmt.Errorf("%s/%s has no ref: %w", remote, branch, err)
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
	originSummaries, err := g.FirstParentCommitSummaries(originBase, originSHA)
	if err != nil {
		return false, "", fmt.Errorf("commit summaries of origin range %s..%s: %w", originBase, originSHA, err)
	}
	originIDs := patchIDsOf(originPairs)
	localIDs := patchIDsOf(localPairs)

	missing := missingCommits(originPairs, localIDs)

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

	refused := len(missing) > 0 && !rangeIdentical

	// The rework check reads every commit in origin's range, not just the
	// first-parent chain: a seat-authored merge can carry another author's
	// commit on its second parent, and the lease push would drop it from the
	// branch. Only a divergence that would otherwise be refused needs the walk.
	var originRange []git.CommitSummary
	if refused && rework.rejectedHead != "" {
		originRange, err = g.FullRangeCommitSummaries(originBase, originSHA)
		if err != nil {
			return false, "", fmt.Errorf("full commit summaries of origin range %s..%s: %w", originBase, originSHA, err)
		}
	}
	reworkAllowed := refused && rework.allows(originSHA, originRange)
	if reworkAllowed {
		carries, carriesErr := reworkLocalCarriesCommits(g, originSHA, localSHA)
		if carriesErr != nil {
			return false, "", carriesErr
		}
		reworkAllowed = carries
	}
	switch {
	case reworkAllowed:
		diagnosis = fmt.Sprintf("rework of the rejected submission, content changed (origin=%s is the head the rejection named, its %d commit(s) are all %s's; local=%s carries the rework)",
			ShortSHA(originSHA), len(originRange), rework.seatAuthor, ShortSHA(localSHA))
	case refused:
		return false, divergenceRefusal(originSHA, localSHA, missing, originSummaries), nil
	case len(originIDs) == 0:
		diagnosis = fmt.Sprintf("no origin work at risk: origin/%s (%s) carries no commits of its own against %s (origin=%s local=%s)",
			branch, ShortSHA(originSHA), target, ShortSHA(originSHA), ShortSHA(localSHA))
	case rangeIdentical || len(originIDs) == len(localIDs):
		diagnosis = fmt.Sprintf("diverged by rebase, content identical (origin=%s local=%s, origin %d commit(s) vs local %d, same change-set)",
			ShortSHA(originSHA), ShortSHA(localSHA), len(originIDs), len(localIDs))
	default:
		diagnosis = fmt.Sprintf("diverged by rework-redispatch, no origin work lost (origin=%s local=%s: origin's %d commit(s) are all present in local's %d — %d new local commit(s))",
			ShortSHA(originSHA), ShortSHA(localSHA), len(originIDs), len(localIDs), len(localIDs)-len(originIDs))
	}

	if leaseErr := g.PushForceWithLease(remote, refspec, branch, originSHA); leaseErr != nil {
		return false, diagnosis, fmt.Errorf("leased force-push failed: %w", leaseErr)
	}
	return true, diagnosis, nil
}

// isMissingRemoteRef reports whether a git fetch failed because origin has no
// ref by that name: git's "couldn't find remote ref" for a deleted branch.
func isMissingRemoteRef(err error) bool {
	var ge *git.GitError
	return errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref")
}

// reworkLocalCarriesCommits reports whether local HEAD holds at least one commit
// origin lacks. A stale worktree at or behind origin's tip carries no rework, so
// leasing over origin with it would discard the rejected submission for less.
// A merge-base that comes back empty reports false: the rework allowance fails closed.
func reworkLocalCarriesCommits(g divergedPushGit, originSHA, localSHA string) (bool, error) {
	if land.HeadsEqual(originSHA, localSHA) {
		return false, nil
	}
	base, err := g.MergeBase(originSHA, localSHA)
	if err != nil {
		return false, fmt.Errorf("merge-base(origin tip, local HEAD): %w", err)
	}
	return base != "" && !land.HeadsEqual(base, localSHA), nil
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

// missingCommits returns the origin commits whose patch-id is absent from have.
// It is a multiset difference, so a change applied twice on origin needs two
// matches locally, and each result keeps its commit for the refusal to name.
func missingCommits(want []git.PatchIDCommit, have []string) []git.PatchIDCommit {
	unmatched := make(map[string]int, len(have))
	for _, id := range have {
		unmatched[id]++
	}
	var missing []git.PatchIDCommit
	for _, p := range want {
		if unmatched[p.PatchID] > 0 {
			unmatched[p.PatchID]--
			continue
		}
		missing = append(missing, p)
	}
	return missing
}

// divergenceRefusal is the message for origin holding changes local does not.
// A patch-id hashes a diff, not a commit, so each id is named with the origin
// commit it came from; a reader would otherwise look for a commit that does
// not exist (gt-dq6p8).
func divergenceRefusal(originSHA, localSHA string, missing []git.PatchIDCommit, origin []git.CommitSummary) string {
	subjects := make(map[string]git.CommitSummary, len(origin))
	for _, c := range origin {
		subjects[c.SHA] = c
	}
	var b strings.Builder
	fmt.Fprintf(&b, "origin=%s local=%s — origin has %d commit(s) whose changes are absent locally — real divergence, not just a rebase; nothing pushed so that work is not clobbered\n",
		ShortSHA(originSHA), ShortSHA(localSHA), len(missing))
	b.WriteString("the ids below are patch-ids (git patch-id --stable), not commit shas; each names the origin commit it came from:")
	for _, p := range missing {
		c, ok := subjects[p.Commit]
		if !ok {
			// The patch-id walk and the summary walk disagreed about this commit.
			// Say so, rather than printing a mapping with no subject.
			fmt.Fprintf(&b, "\n  patch-id %s = commit %s (no subject: the commit is not in origin's summary listing)", p.PatchID, ShortSHA(p.Commit))
			continue
		}
		fmt.Fprintf(&b, "\n  patch-id %s = commit %s %s", p.PatchID, ShortSHA(p.Commit), c.Subject)
	}
	return b.String()
}

func ShortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
