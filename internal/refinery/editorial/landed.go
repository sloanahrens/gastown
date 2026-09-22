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
	// PatchID is patch-id(Parent..Commit). ResolveLandedRange only returns a
	// range whose own diff produces it, so the note carries the value every
	// later reader recomputes.
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
// for files nobody touched (mayor, 2026-09-22: one such run reported a
// CRITICAL removal of a guard whose file exists).
//
// The range's diff must also reproduce patch-id(Commit^1..Commit): they are
// two views of one change, and a landed merge whose tree is not the merge of
// its parents — a hand-resolved conflict that dropped the other side, an
// evil merge — is not reviewable this way. Rather than review a diff that is
// not what landed, this refuses and reports which side deletes what.
func ResolveLandedRange(g *git.Git, landedArg, target string) (LandedRange, error) {
	landed, err := g.Rev(strings.TrimSpace(landedArg) + "^{commit}")
	if err != nil {
		return LandedRange{}, fmt.Errorf("review --landed: resolve %s: %w", strings.TrimSpace(landedArg), err)
	}
	landed = strings.TrimSpace(landed)

	// Only a landed commit has a landed diff. A note on a commit that never
	// reached the target proofs nothing about it (same rule as rekey-note).
	targetRef := "origin/" + target
	reachable, err := g.IsAncestor(landed, targetRef)
	if err != nil {
		return LandedRange{}, fmt.Errorf("review --landed: cannot tell whether %s landed on %s: %w (fetch the rig clone and retry)", landed, targetRef, err)
	}
	if !reachable {
		return LandedRange{}, fmt.Errorf("review --landed: refusing: %s is not reachable from %s — only a commit that landed has a landed diff to review; fetch and retry, or name the branch it landed on", landed, targetRef)
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
		return LandedRange{}, fmt.Errorf("review --landed: patch-id of the landed diff %s..%s: %w", parent, landed, err)
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

	// The change side is the parent whose own diff against the merge point
	// reproduces what landed. Normally that is the second parent — the head
	// the merge brought in — but a merge made the other way round (target
	// merged into the branch, then landed) puts it in the first parent, and
	// the patch-id is what distinguishes the two without guessing.
	attempts := make([]landedAttempt, 0, 2)
	for _, head := range []string{second, parent} {
		if head == base {
			continue // that parent is the merge point itself; its diff is empty
		}
		rangePatchID, err := g.PatchID(base, head)
		if err != nil {
			return LandedRange{}, fmt.Errorf("review --landed: patch-id of %s..%s: %w", base, head, err)
		}
		if rangePatchID == patchID {
			landedRange.Head = head
			return landedRange, nil
		}
		attempts = append(attempts, landedAttempt{head: head, patchID: rangePatchID})
	}

	return LandedRange{}, landedRangeRefusal(g, landedRange, attempts)
}

// landedAttempt is one candidate change side whose diff did not reproduce the
// landed patch-id, kept for the refusal message.
type landedAttempt struct {
	head    string
	patchID string
}

// landedRangeRefusal explains why no merge point and change side reproduces
// the landed diff, using the deletions each side makes. The paths are the
// actionable part: a phantom deletion names a file the landing kept, and an
// unseen one names a file the landing removed that the range would not show.
func landedRangeRefusal(g *git.Git, r LandedRange, attempts []landedAttempt) error {
	var b strings.Builder
	fmt.Fprintf(&b, "review --landed: refusing: no parent diff of %s reproduces the change that landed (patch-id %s)\n", shortCommit(r.Commit), r.PatchID)
	fmt.Fprintf(&b, "  landed diff  %s..%s  patch-id %s\n", shortCommit(r.Parent), shortCommit(r.Commit), r.PatchID)
	for _, a := range attempts {
		fmt.Fprintf(&b, "  candidate    %s..%s  patch-id %s\n", shortCommit(r.Base), shortCommit(a.head), a.patchID)
	}

	if d, ok := tryLandedDeletions(g, r); ok {
		if len(d.rangeOnly) > 0 {
			fmt.Fprintf(&b, "\nThe candidate diff removes %s the landed commit does not:\n%s",
				countPaths(len(d.rangeOnly)), landedPathLines(d.rangeOnly))
		}
		if len(d.landedOnly) > 0 {
			fmt.Fprintf(&b, "\nThe landed commit removes %s the candidate diff does not:\n%s",
				countPaths(len(d.landedOnly)), landedPathLines(d.landedOnly))
		}
	}

	b.WriteString("\nThe merge's tree is not the merge of its parents, so its diff cannot be reconstructed from the graph. " +
		"Inspect it directly (git show --stat <commit>) and review that diff by hand if it needs a verdict.")
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
