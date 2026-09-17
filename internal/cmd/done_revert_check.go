package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
)

// This file guards gt done against a branch whose commit REVERTS work already
// merged into the target. That is not a hypothetical: two local-coder polecat
// MRs in one night (gt-wisp-hrau, gt-wisp-p7nl) each submitted an unrelated fix
// wrapped around a full revert of everything merged since their worktree was
// cut — 17 files +972/-1711 and 9 files +106/-472 (gt-63sz).
//
// The mechanism is a squash-onto-fresh-base habit, not staleness of the
// COMMIT graph: the polecat runs
//
//	git add -A; git reset --soft origin/main; git commit
//
// over a checkout that is hours old. `reset --soft` moves HEAD to the current
// remote tip while leaving the index and working tree exactly as the old
// checkout had them, so the following commit records (old tree) - (new tip) —
// a revert of every commit merged in between, hidden inside one commit on a
// perfectly fresh base.
//
// Nothing about ancestry can see this. `git merge-base origin/main HEAD` is
// origin/main itself, the branch is exactly one commit ahead, and the commit
// message describes the intended work. Only per-file CONTENT shows it: the
// branch carries the pre-merge blob of paths it never meant to touch. So the
// check below reconstructs, for each path, the blobs on all four sides of the
// question (the target commit's parent, the target commit, the target tip, and
// the branch tip) and asks whether the branch undoes a live change.

// revertScanCommits bounds how far back through the target's history the check
// looks. A branch can only revert commits merged after its checkout was cut, so
// the revert candidates are always recent; this window is the allowance for how
// long a single worktree may have sat stale, not a limit on history depth.
const revertScanCommits = 2000

// revertReportLimit caps how many reverted changes are listed before the message
// summarizes the rest. The count is always reported in full.
const revertReportLimit = 8

// revertPathsPerCommit caps the paths listed under a single reverted commit.
const revertPathsPerCommit = 4

// revertedMerge is one commit on the target branch whose change the branch
// undoes, with the paths on which it was observed.
type revertedMerge struct {
	Commit string   // the target-branch commit being undone, full sha
	Paths  []string // every path of that commit the branch undoes
}

// detectRevertedMerges reports the changes merged into target that g's branch
// tip (HEAD, in g's repository) undoes. An empty result means the branch's diff
// against target removes only content the branch itself introduced.
//
// Two shapes are detected, both anchored on the same fact — that the branch
// carries a live target content state that the target has moved past:
//
//   - exact: the branch's blob for a path equals the blob the commit's parent
//     had. Covers whole-file reverts, including a path the branch deletes that
//     the target commit created.
//   - contained: the branch's line-level diff against the merge base inverts the
//     commit's own line-level diff. Catches the case the exact test cannot see
//     at all — a path the polecat both edited and reverted (the fix and the
//     revert share one blob, so no blob comparison can separate them).
//
// Both are gated on the branch actually changing the path relative to the merge
// base. Without that gate a branch that simply does not mention a path would be
// reported for every historical change to it, when in fact merging such a branch
// leaves the target's copy alone.
func detectRevertedMerges(g *git.Git, target string) ([]revertedMerge, error) {
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
	headBlobs, err := g.TreeFileBlobs("HEAD")
	if err != nil {
		return nil, fmt.Errorf("reading tree of HEAD: %w", err)
	}
	changes, err := g.CommitFileChanges(target, revertScanCommits)
	if err != nil {
		return nil, fmt.Errorf("reading history of %s: %w", target, err)
	}

	var found []revertedMerge
	byCommit := make(map[string]int) // commit -> index in found
	for _, ch := range changes {
		// preImage is the content the commit started from ("" if it created the
		// path); postImage is what it changed the path to ("" if it deleted it).
		preImage, postImage := ch.OldBlob, ch.NewBlob
		base, head := baseBlobs[ch.Path], headBlobs[ch.Path]

		// The branch must change this path relative to the merge base, or the
		// merge leaves the target's copy as it is and there is nothing to
		// report. ("" == "" covers a path absent from both.)
		if head == base {
			continue
		}
		// The target is back at the pre-image state, so the commit's change is
		// no longer live and cannot be reverted.
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
		found = append(found, revertedMerge{Commit: ch.Commit, Paths: []string{ch.Path}})
	}
	return found, nil
}

// changeIsInvertedBy reports whether the change from preImage to postImage is
// present, inverted, in the change from base to head — i.e. whether the branch
// removes what the commit added and restores what it removed.
//
// Containment is required in both directions: a branch that merely deletes a
// file the commit touched, or that happens to add a line the commit removed,
// is not reverting it.
func changeIsInvertedBy(g *git.Git, preImage, postImage, base, head string) (bool, error) {
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

// multisetContains reports whether every line counted in want appears in have at
// least as many times. Set containment would let one surviving copy of a line
// stand in for two removed ones.
func multisetContains(have, want map[string]int) bool {
	for line, count := range want {
		if have[line] < count {
			return false
		}
	}
	return true
}

// reportRevertedMerges prints the branch's diff against target and refuses the
// submission when the branch undoes merged work.
//
// The stat is printed on every call, refusals included, because it is the one
// view that makes the failure self-evident to the polecat that caused it: a
// correct branch lists the polecat's own files, and the two branches in gt-63sz
// listed 9 and 17 files each, nearly none of them the author's.
func reportRevertedMerges(g *git.Git, target string) error {
	if stat, err := g.DiffStatThreeDot(target, "HEAD"); err != nil {
		style.PrintWarning("could not compute branch diff against %s: %v", target, err)
	} else if strings.TrimSpace(stat) != "" {
		fmt.Printf("  Branch diff vs %s:\n", target)
		for _, line := range strings.Split(strings.TrimSpace(stat), "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}

	found, err := detectRevertedMerges(g, target)
	if err != nil {
		// Refuse rather than submit: this check exists because a branch that
		// reverts merged work is silently accepted by everything downstream,
		// and a check that cannot run must not read as a check that passed.
		return fmt.Errorf("cannot verify branch against %s: %w\n"+
			"Refusing to submit rather than risk reverting merged work. "+
			"Run `git fetch origin && git rebase %s`, then re-run gt done.", target, err, target)
	}
	if len(found) == 0 {
		return nil
	}
	return revertedMergeRefusal(g, target, found)
}

// revertedMergeRefusal builds the refusal error for a branch that undoes merged
// work. The message deliberately does not name the flag that overrides it:
// agents read refusal text and self-bypass, so the text says what to do about
// the branch instead.
func revertedMergeRefusal(g *git.Git, target string, found []revertedMerge) error {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to submit: this branch undoes work already merged to %s\n\n", target)
	fmt.Fprintf(&b, "These commits on %s are undone by your branch:\n", target)
	for i, f := range found {
		if i == revertReportLimit {
			fmt.Fprintf(&b, "  ... and %d more\n", len(found)-revertReportLimit)
			break
		}
		subject, err := g.CommitSubject(f.Commit)
		if err != nil || subject == "" {
			subject = "(subject unavailable)"
		}
		fmt.Fprintf(&b, "  %s %s\n", shortSHA(f.Commit), subject)
		for j, path := range f.Paths {
			if j == revertPathsPerCommit {
				fmt.Fprintf(&b, "      ... and %d more paths\n", len(f.Paths)-revertPathsPerCommit)
				break
			}
			fmt.Fprintf(&b, "      undoes: %s\n", path)
		}
	}
	b.WriteString("\nYour working tree is older than the base your commit claims to sit on, " +
		"so the commit records (your tree) - (that base): a revert of every commit merged " +
		"in between, plus your own change. Submitting it would delete other people's merged work.\n\n")
	fmt.Fprintf(&b, "Integrate with:\n"+
		"  git fetch origin && git rebase %s\n\n", target)
	fmt.Fprintf(&b, "Then confirm the branch lists only YOUR files and re-run gt done:\n"+
		"  git diff --stat %s...HEAD", target)
	return fmt.Errorf("%s", b.String())
}
