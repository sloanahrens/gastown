package git

import (
	"fmt"
)

// This file holds git-content-based revert detection: it reports whether a
// candidate tree undoes changes that target's own history already merged,
// even though ordinary commit-ancestry checks see nothing wrong — the
// candidate's branch contains every one of those commits, but a stale
// checkout can still carry pre-merge content for paths it never meant to
// touch.
//
// Two incidents surfaced this, on opposite ends of a commit's lifetime. In
// the first, a polecat ran `git add -A; git reset --soft origin/main; git
// commit` over a checkout that was hours old: `reset --soft` moves HEAD to
// the fresh tip while leaving the index and working tree exactly as the old
// checkout had them, so the resulting commit records (old tree) - (new tip) —
// a revert of everything merged in between, under a message describing
// unrelated work (gt-63sz). In the second, a shared-worktree reuse left a
// polecat's working tree holding stale pre-merge content for six files it
// never touched, and the checkpoint_dog daemon staged and nearly committed
// that content under a generic "WIP: checkpoint (auto)" subject before the
// commit — no ancestry check would have seen it there either (gt-2bp8).
//
// Nothing about ancestry can see either shape: `git merge-base target HEAD`
// is target itself or an ancestor of it, and the branch is exactly as many
// commits ahead as its author intended. Only per-path CONTENT shows it, so
// DetectRevertedMerges reconstructs, for each path, the blobs on all four
// sides of the question (the merge base, the target commit's parent, the
// target tip, and the candidate tree) and asks whether the candidate undoes a
// live change.

// revertScanCommits bounds how far back through target's history the check
// looks. A candidate can only revert commits merged after its checkout was
// cut, so the revert candidates are always recent; this window is the
// allowance for how long a single worktree may have sat stale, not a limit on
// history depth.
const revertScanCommits = 2000

// gt-zeuip (dangling refs reachable only through a merge parent can
// false-positive a no-op path deletion) is a known, separate gap in this
// scan. Restricting CommitFileChanges to --first-parent would close it but
// also hides every commit merged with --no-ff — this town's own merge
// strategy — since a merge commit itself contributes no diff; excluding a
// path merely because targetBlobs equals baseBlobs for it is unsound too,
// since that equality is trivially true whenever the candidate's merge base
// already sits at target's own tip (the common, healthy case gt-63sz's own
// regression test pins). Left open rather than risk either regression.

// RevertedMerge is one commit on target whose change the candidate undoes,
// with the paths on which it was observed.
type RevertedMerge struct {
	Commit string   // the target commit being undone, full sha
	Paths  []string // every path of that commit the candidate undoes
}

// DetectRevertedMerges reports the changes merged into target that
// headTreeRef undoes. An empty result means headTreeRef's diff against target
// removes only content headTreeRef itself introduced.
//
// candidateCommit is the commit the candidate's content descends from — used
// only to resolve the merge base, since the merge base of an as-yet-uncommitted
// change is the merge base of the commit it will be committed onto. Pass
// "HEAD" for a check that runs against the caller's own checkout (gt done, the
// checkpoint_dog daemon); pass the candidate's actual commit SHA when the
// caller's checkout is on some other branch entirely, as the refinery's
// pre-merge gate is while target is staged.
//
// headTreeRef names the tree being checked for content, which is normally the
// same value as candidateCommit — pass a bare tree object (e.g. from
// `git write-tree`) instead when the content in question is still staged in
// the index rather than committed to candidateCommit.
//
// Two shapes are detected, both anchored on the same fact — that headTreeRef
// carries a live target content state that target has moved past:
//
//   - exact: headTreeRef's blob for a path equals the blob the commit's
//     parent had. Covers whole-file reverts, including a path headTreeRef
//     deletes that the target commit created.
//   - contained: headTreeRef's line-level diff against the merge base inverts
//     the commit's own line-level diff. Catches the case the exact test
//     cannot see at all — a path that was both edited and reverted (the fix
//     and the revert share one blob, so no blob comparison can separate
//     them).
//
// Both are gated on headTreeRef actually changing the path relative to the
// merge base. Without that gate a candidate that simply does not mention a
// path would be reported for every historical change to it, when in fact
// merging such a candidate leaves target's copy alone.
func DetectRevertedMerges(g *Git, target, candidateCommit, headTreeRef string) ([]RevertedMerge, error) {
	mergeBase, err := g.MergeBase(target, candidateCommit)
	if err != nil {
		return nil, fmt.Errorf("resolving merge base of HEAD and %s: %w", target, err)
	}
	baseBlobs, err := g.TreeFileBlobs(mergeBase)
	if err != nil {
		return nil, fmt.Errorf("reading tree of %s: %w", mergeBase, err)
	}
	targetBlobs, err := g.TreeFileBlobs(target)
	if err != nil {
		return nil, fmt.Errorf("reading tree of %s: %w", target, err)
	}
	headBlobs, err := g.TreeFileBlobs(headTreeRef)
	if err != nil {
		return nil, fmt.Errorf("reading tree of %s: %w", headTreeRef, err)
	}
	changes, err := g.CommitFileChanges(target, revertScanCommits)
	if err != nil {
		return nil, fmt.Errorf("reading history of %s: %w", target, err)
	}

	var found []RevertedMerge
	byCommit := make(map[string]int) // commit -> index in found
	for _, ch := range changes {
		// preImage is the content the commit started from ("" if it created the
		// path); postImage is what it changed the path to ("" if it deleted it).
		preImage, postImage := ch.OldBlob, ch.NewBlob
		base, head := baseBlobs[ch.Path], headBlobs[ch.Path]

		// headTreeRef must change this path relative to the merge base, or
		// merging it leaves target's copy as it is and there is nothing to
		// report. ("" == "" covers a path absent from both.)
		if head == base {
			continue
		}
		// target is back at the pre-image state, so the commit's change is no
		// longer live and cannot be reverted.
		if targetBlobs[ch.Path] == preImage {
			continue
		}

		reverts := head == preImage
		if !reverts && preImage != "" && postImage != "" && head != "" && base != "" {
			reverts, err = changeIsInvertedBy(g, preImage, postImage, base, head)
			if err != nil {
				return nil, err
			}
		}
		if !reverts {
			continue
		}
		if i, seen := byCommit[ch.Commit]; seen {
			found[i].Paths = append(found[i].Paths, ch.Path)
			continue
		}
		byCommit[ch.Commit] = len(found)
		found = append(found, RevertedMerge{Commit: ch.Commit, Paths: []string{ch.Path}})
	}
	return found, nil
}

// changeIsInvertedBy reports whether the change from preImage to postImage is
// present, inverted, in the change from base to head — i.e. whether the
// candidate removes what the commit added and restores what it removed.
//
// Containment is required in both directions: a candidate that merely deletes
// a file the commit touched, or that happens to add a line the commit
// removed, is not reverting it.
func changeIsInvertedBy(g *Git, preImage, postImage, base, head string) (bool, error) {
	branchAdded, branchRemoved, err := g.BlobDiffLines(base, head)
	if err != nil {
		return false, fmt.Errorf("diffing %s..%s: %w", base, head, err)
	}
	changeAdded, changeRemoved, err := g.BlobDiffLines(preImage, postImage)
	if err != nil {
		return false, fmt.Errorf("diffing %s..%s: %w", preImage, postImage, err)
	}
	if len(changeAdded)+len(changeRemoved) == 0 {
		return false, nil
	}
	return multisetContains(branchRemoved, changeAdded) && multisetContains(branchAdded, changeRemoved), nil
}

// multisetContains reports whether every line counted in want appears in have
// at least as many times. Set containment would let one surviving copy of a
// line stand in for two removed ones.
func multisetContains(have, want map[string]int) bool {
	for line, count := range want {
		if have[line] < count {
			return false
		}
	}
	return true
}
