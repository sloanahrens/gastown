package editorial

import (
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// landedReportLimit caps how many paths a refusal lists per direction before
// it summarizes the rest. The count is always reported in full.
const landedReportLimit = 8

// LandedRange is the review range for a commit that already landed on the
// target branch: what a retro-review reads, and the commit it stamps.
//
// It exists because the editorial gate writes its proof before the merge,
// keyed to a rehearsal head, and copies it onto the merge commit as the
// merge completes (CopyNotesToLanded). When that copy never happens — the
// merge queue discarded the head, the MR bead is reaped before anyone
// notices — the landed commit carries no note and the coverage check flags
// it, with no way to produce the missing verdict after the fact (gt-ljn8).
type LandedRange struct {
	// Commit is the commit that landed. The verdict is stamped on it: that
	// is the commit the coverage check reads a note from.
	Commit string
	// Parent is Commit's first parent, the side the coverage check measures
	// the landed diff against (patch-id(Parent..Commit)).
	Parent string
	// Base and Head bound the reviewed diff, as a pair-wise range. They are
	// not Parent and Commit: for a merge, Parent..Commit is the branch's
	// change as it arrived on the target, while Base..Head is the branch's own
	// history — the same change with its own hunk context, which is what om
	// should read.
	Base string
	Head string
	// PatchID is patch-id(Parent..Commit) — the landed diff, and the value the
	// coverage check recomputes from the commit the note sits on. The note is
	// keyed on this, never on patch-id(Base..Head): Base..Head is the branch's
	// own view of the change, and patch-id hashes context lines, so the two
	// differ whenever the target moved lines inside the branch's hunk context.
	PatchID string
}

// ResolveLandedRange derives the review range for a commit that already
// landed, from the commit graph alone — no MR bead, no rehearsal.
//
// The base is the merge point of the landed merge's two parents, never its
// first parent. A merge commit's ^1 is the target branch's tip at merge
// time, which for a branch cut from an older target is ahead of the branch's
// own base: diffing against it reports everything the target gained in the
// meantime, inverted, as deletions the branch never made — phantom findings
// for files nobody touched (gt-ljn8).
//
// The landing must also be a merge of its parents rather than a rewrite of
// them: a landed merge whose tree differs from the tree git computes for its
// two parents resolved a conflict or carries an edit no parent made, and its
// range cannot be reconstructed from the graph. Rather than review a diff that
// is not what landed, this refuses and reports which side deletes what.
func ResolveLandedRange(g *git.Git, landedArg, target string) (LandedRange, error) {
	landed, err := g.Rev(strings.TrimSpace(landedArg) + "^{commit}")
	if err != nil {
		return LandedRange{}, fmt.Errorf("review --landed: resolve %s: %w", strings.TrimSpace(landedArg), err)
	}
	landed = strings.TrimSpace(landed)

	// Only a landed commit has a landed diff, and the fact that matters is
	// first-parent membership rather than ancestry: the coverage check walks
	// first parents, so a commit inside a merged branch is reachable without
	// being a commit the check ever reads, and a note stamped on it covers
	// nothing (gt-ljn8).
	targetRef := "origin/" + target
	onChain, err := g.FirstParentContains(landed, targetRef)
	if err != nil {
		return LandedRange{}, fmt.Errorf("review --landed: cannot tell whether %s landed on %s: %w (fetch the rig clone and retry)", landed, targetRef, err)
	}
	if !onChain {
		return LandedRange{}, fmt.Errorf("review --landed: refusing: %s is not on %s's first-parent chain — the coverage check walks first-parent history, so only a commit that landed there has a landed diff whose note counts; fetch and retry, or name the branch it landed on", landed, targetRef)
	}

	parents, err := g.Parents(landed)
	if err != nil {
		return LandedRange{}, fmt.Errorf("review --landed: parents of %s: %w", landed, err)
	}
	if len(parents) == 0 {
		return LandedRange{}, fmt.Errorf("review --landed: refusing: %s is a root commit, so it has no diff to review", landed)
	}

	parent := parents[0]
	patchID, err := g.PatchID(parent, landed)
	if err != nil {
		return LandedRange{}, fmt.Errorf("review --landed: the landed diff %s..%s has no patch-id to key a verdict on: %w", shortCommit(parent), shortCommit(landed), err)
	}
	landedRange := LandedRange{Commit: landed, Parent: parent, Base: parent, Head: landed, PatchID: patchID}
	if len(parents) == 1 {
		// A squash or fast-forward landing: the commit's own diff is the
		// change under review, and it is the only side there is.
		return landedRange, nil
	}

	second := parents[1]
	base, err := g.MergeBase(parent, second)
	if err != nil {
		return LandedRange{}, fmt.Errorf("review --landed: merge base of %s and %s: %w", parent, second, err)
	}
	landedRange.Base = base
	// The change under review is the branch's own side of the merge. The merge
	// is made on the target, so ^1 is the target's tip at merge time and ^2 is
	// the branch that arrived; the explicit merge-base above, not ^1, is what
	// keeps the range free of everything the target gained meanwhile.
	landedRange.Head = second

	if err := requireMergeOfParents(g, landedRange); err != nil {
		return LandedRange{}, err
	}
	return landedRange, nil
}

// requireMergeOfParents refuses a landing whose own tree is not the tree a
// conflict-free merge of its parents would produce.
//
// The comparison is between trees, not patch-ids. patch-id hashes hunk
// context as well as changed lines, so an ordinary conflict-free merge — one
// where the target moved lines inside the branch's hunk context — hashes
// differently from the branch alone even though both describe the same change
// (gt-ljn8). Trees carry no such ambiguity: they are either the same tree or
// they are not.
func requireMergeOfParents(g *git.Git, r LandedRange) error {
	commitTree, err := g.Rev(r.Commit + "^{tree}")
	if err != nil {
		return fmt.Errorf("review --landed: tree of %s: %w", shortCommit(r.Commit), err)
	}
	mergedTree, mergeErr := g.MergedTree(r.Parent, r.Head)
	if mergeErr == nil && mergedTree == commitTree {
		return nil
	}
	return landedMergeRefusal(g, r, commitTree, mergedTree, mergeErr)
}

// landedMergeRefusal explains why the landing is not the merge of its parents,
// using the deletions each side makes. The paths are the actionable part: a
// phantom deletion names a file the landing kept, and an unseen one names a
// file the landing removed that the range would not show.
func landedMergeRefusal(g *git.Git, r LandedRange, commitTree, mergedTree string, mergeErr error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "review --landed: refusing: %s is not the merge of its parents %s and %s, so the range to review cannot be reconstructed from the commit graph\n",
		shortCommit(r.Commit), shortCommit(r.Parent), shortCommit(r.Head))
	if mergeErr != nil {
		fmt.Fprintf(&b, "  git merge-tree %s %s does not resolve: %v\n", shortCommit(r.Parent), shortCommit(r.Head), mergeErr)
	} else {
		fmt.Fprintf(&b, "  git merge-tree %s %s = tree %s, but %s's own tree is %s\n",
			shortCommit(r.Parent), shortCommit(r.Head), shortCommit(mergedTree), shortCommit(r.Commit), shortCommit(commitTree))
	}

	if d, ok := tryLandedDeletions(g, r); ok {
		if len(d.rangeOnly) > 0 {
			fmt.Fprintf(&b, "\nThe branch's diff removes %s the landed commit does not:\n%s",
				countPaths(len(d.rangeOnly)), landedPathLines(d.rangeOnly))
		}
		if len(d.landedOnly) > 0 {
			fmt.Fprintf(&b, "\nThe landed commit removes %s the branch's diff does not:\n%s",
				countPaths(len(d.landedOnly)), landedPathLines(d.landedOnly))
		}
	}

	b.WriteString("\nA conflict resolution or an edit no parent made is not reviewable from the graph. " +
		"Review the landing itself instead (git show --stat <commit>) and stamp a verdict by hand if it needs one.")
	return fmt.Errorf("%s", b.String())
}

// landedDeletions is the path-level disagreement between the candidate range
// and what landed: paths one side deletes and the other keeps.
type landedDeletions struct {
	rangeOnly  []string
	landedOnly []string
}

// tryLandedDeletions computes the deletion disagreement, or reports false
// when the trees could not be read — the refusal is worth more than a
// missing explanation, so a failed diagnostic never replaces it.
func tryLandedDeletions(g *git.Git, r LandedRange) (landedDeletions, bool) {
	rangeRemoved, err := deletedPaths(g, r.Base, r.Head)
	if err != nil {
		return landedDeletions{}, false
	}
	landedRemoved, err := deletedPaths(g, r.Parent, r.Commit)
	if err != nil {
		return landedDeletions{}, false
	}
	return landedDeletions{
		rangeOnly:  missingFrom(rangeRemoved, landedRemoved),
		landedOnly: missingFrom(landedRemoved, rangeRemoved),
	}, true
}

// deletedPaths returns the sorted paths a tree held and the other does not —
// the paths that diff(base, head) reports as deletions.
func deletedPaths(g *git.Git, base, head string) ([]string, error) {
	baseBlobs, err := g.TreeFileBlobs(base)
	if err != nil {
		return nil, fmt.Errorf("reading tree of %s: %w", base, err)
	}
	headBlobs, err := g.TreeFileBlobs(head)
	if err != nil {
		return nil, fmt.Errorf("reading tree of %s: %w", head, err)
	}
	var removed []string
	for path := range baseBlobs {
		if _, ok := headBlobs[path]; !ok {
			removed = append(removed, path)
		}
	}
	sort.Strings(removed)
	return removed, nil
}

// missingFrom returns the entries of have that absent lacks, in have's own
// order (both are sorted), so a refusal reads the same on a re-run.
func missingFrom(have, absent []string) []string {
	present := make(map[string]bool, len(absent))
	for _, path := range absent {
		present[path] = true
	}
	var out []string
	for _, path := range have {
		if !present[path] {
			out = append(out, path)
		}
	}
	return out
}

// countPaths renders a path count for a refusal line.
func countPaths(n int) string {
	if n == 1 {
		return "1 path"
	}
	return fmt.Sprintf("%d paths", n)
}

// landedPathLines indents up to landedReportLimit paths, summarizing the
// rest.
func landedPathLines(paths []string) string {
	var b strings.Builder
	for i, path := range paths {
		if i == landedReportLimit {
			fmt.Fprintf(&b, "    ... and %d more\n", len(paths)-landedReportLimit)
			break
		}
		fmt.Fprintf(&b, "    %s\n", path)
	}
	return b.String()
}
