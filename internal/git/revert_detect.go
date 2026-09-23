package git

import (
	"fmt"
	"strings"
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
// headTreeRef names the tree being checked for content, not which commit the
// candidate descends from — that is always g's actual HEAD, since the merge
// base of an as-yet-uncommitted change is the merge base of the commit it
// will be committed onto. Pass "HEAD" when the content in question is already
// committed there; pass a bare tree object (e.g. from `git write-tree`) to
// check content still staged in the index before committing it.
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
//
// The contained test has no positional or context anchor — it only compares
// line multisets — so a candidate that removes one instance of a line the
// commit added reads as a match whether or not that is what actually
// happened (gt-0wy03). Before reporting a contained match, one escape hatch
// asks whether the removal is really a relocation: see packageAddedLines for
// what counts as relocated — content gained in a path this branch itself
// changed, never a path it left untouched. Only checked for a contained
// match: an exact match already requires the whole path back at its literal
// pre-commit blob, leaving no partial shape for relocation to find.
//
// There is no escape hatch keyed off commit-message text — an author's own
// words must never bypass a landing check. The one sanctioned override is
// --allow-reverts, gated on an explicit mayor ruling (see
// requireRevertOverrideAuthorization in internal/cmd/done_revert_check.go).
// A real revert has no relocation and is still refused.
func DetectRevertedMerges(g *Git, target, headTreeRef string) ([]RevertedMerge, error) {
	mergeBase, err := g.MergeBase(target, "HEAD")
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

	// parentTreeCache and packageAddedCache memoize, respectively, a flagged
	// commit's own parent tree and the per-package added-lines multiset
	// derived from it: several flagged paths sharing a commit, or a package
	// with several flagged paths, would otherwise re-read or re-diff the same
	// trees once per contained match.
	parentTreeCache := map[string]map[string]string{}
	packageAddedCache := map[string]map[string]int{}
	relocated := func(commit, path string, want map[string]int) (bool, error) {
		pkg := packageDir(path)
		cacheKey := commit + "\x00" + pkg
		have, cached := packageAddedCache[cacheKey]
		if !cached {
			parentBlobs, ok := parentTreeCache[commit]
			if !ok {
				var err error
				parentBlobs, err = g.TreeFileBlobs(commit + "^")
				if err != nil {
					return false, fmt.Errorf("reading parent tree of %s: %w", commit, err)
				}
				parentTreeCache[commit] = parentBlobs
			}
			var err error
			have, err = packageAddedLines(g, parentBlobs, headBlobs, baseBlobs, pkg)
			if err != nil {
				return false, err
			}
			packageAddedCache[cacheKey] = have
		}
		return multisetContains(have, want), nil
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
		var changeAdded map[string]int
		if !reverts && preImage != "" && postImage != "" && head != "" && base != "" {
			reverts, changeAdded, err = changeIsInvertedBy(g, preImage, postImage, base, head)
			if err != nil {
				return nil, err
			}
		}
		if !reverts {
			continue
		}

		// The one escape hatch applies only to a contained match
		// (changeAdded set): the exact match above already requires the
		// whole path back at its literal pre-commit blob, so there is no
		// partial, still-present-elsewhere shape for relocation to find.
		if changeAdded != nil {
			ok, err := relocated(ch.Commit, ch.Path, changeAdded)
			if err != nil {
				return nil, err
			}
			if ok {
				continue
			}
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
// candidate removes what the commit added and restores what it removed. On a
// true result it also returns changeAdded, the commit's own added lines, for
// the caller's relocation check: this test has no positional or context
// anchor, so a candidate that deletes one instance of a line the commit added
// elsewhere too (a repeated boilerplate line, e.g. a scattered t.Parallel())
// reads the same as a candidate that actually undoes it (gt-0wy03).
//
// Containment is required in both directions: a candidate that merely deletes
// a file the commit touched, or that happens to add a line the commit
// removed, is not reverting it.
func changeIsInvertedBy(g *Git, preImage, postImage, base, head string) (bool, map[string]int, error) {
	branchAdded, branchRemoved, err := g.BlobDiffLines(base, head)
	if err != nil {
		return false, nil, fmt.Errorf("diffing %s..%s: %w", base, head, err)
	}
	changeAdded, changeRemoved, err := g.BlobDiffLines(preImage, postImage)
	if err != nil {
		return false, nil, fmt.Errorf("diffing %s..%s: %w", preImage, postImage, err)
	}
	if len(changeAdded)+len(changeRemoved) == 0 {
		return false, nil, nil
	}
	if !multisetContains(branchRemoved, changeAdded) || !multisetContains(branchAdded, changeRemoved) {
		return false, nil, nil
	}
	return true, changeAdded, nil
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

// packageDir returns the directory a repo path belongs to ("." for a
// top-level path), which this file uses as a stand-in for "package": Go, like
// most of this repo's other languages, keeps one package per directory.
func packageDir(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[:i]
	}
	return "."
}

// packageAddedLines returns the multiset of lines gained anywhere in pkg
// between beforeBlobs and afterBlobs — across every file in the directory,
// not just the one path a flagged commit touched — restricted to paths this
// BRANCH's own commits actually changed: branchBefore[p] must differ from
// afterBlobs[p]. branchBefore is the merge base's tree, so a path where it
// equals afterBlobs is a path the branch never touched, and any content
// gained there between beforeBlobs (the flagged commit's own parent) and
// afterBlobs can only be a LATER TARGET COMMIT's addition, merged in after
// the flagged one and before the merge base — never something this branch
// did (gt-0wy03 attempt 3: an untouched sibling file gaining a matching line
// from such a commit used to rescue a real revert of it).
//
// This still catches the two shapes relocation is meant to: a repeated line
// the flagged commit added in one spot while the candidate independently
// touched another spot with the same text (gt-0wy03's own t.Parallel()
// case), and a block of code moved whole into a new helper in the same file
// (gt-x748o) — both are paths the branch itself changed.
func packageAddedLines(g *Git, beforeBlobs, afterBlobs, branchBefore map[string]string, pkg string) (map[string]int, error) {
	paths := make(map[string]struct{}, len(afterBlobs))
	for p := range beforeBlobs {
		paths[p] = struct{}{}
	}
	for p := range afterBlobs {
		paths[p] = struct{}{}
	}
	added := map[string]int{}
	for p := range paths {
		if packageDir(p) != pkg {
			continue
		}
		if branchBefore[p] == afterBlobs[p] {
			continue
		}
		a, _, err := g.BlobDiffLines(beforeBlobs[p], afterBlobs[p])
		if err != nil {
			return nil, fmt.Errorf("diffing %s in package %s: %w", p, pkg, err)
		}
		for line, n := range a {
			added[line] += n
		}
	}
	return added, nil
}
