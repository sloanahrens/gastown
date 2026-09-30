package git

import (
	"fmt"
	"path"
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
//
// A third shape inverts the check's premise rather than its bookkeeping: a
// candidate that MOVES a merged change's code — into a helper, or a
// neighboring position — deletes those lines where the commit put them and
// adds re-indented, often re-commented copies elsewhere in the package. That
// is indistinguishable from a revert by line containment alone, and it
// refused a branch whose behavior was intact (gt-x748o). So an observation
// only counts as a revert when the removed code does not survive anywhere
// else in its package at the candidate tip; the ones whose code survives are
// reported as relocations (RevertReport.Relocated) rather than refused.

// revertScanCommits bounds how far back through target's history the check
// looks. A candidate can only revert commits merged after its checkout was
// cut, so the revert candidates are always recent; this window is the
// allowance for how long a single worktree may have sat stale, not a limit on
// history depth.
const revertScanCommits = 2000

// RevertedMerge is one commit on target and the paths where the candidate no
// longer carries the change where that commit put it. Whether that is a revert
// or a relocation is RevertReport's to say.
type RevertedMerge struct {
	Commit string   // the target commit being undone, full sha
	Paths  []string // every path of that commit the candidate undoes
}

// RevertReport is what a candidate tree does to the changes target already
// merged, split by whether the work they carry survives the submission.
type RevertReport struct {
	// Reverted are changes the candidate undoes: lines the target commit added
	// are gone from the package the candidate carries. A branch holding any of
	// these must be refused — submitting it deletes merged work.
	Reverted []RevertedMerge

	// Relocated are changes the candidate moves instead of undoing: every line
	// of code the target commit added still exists in the package at the
	// candidate tip, at a new position or in a sibling file, so the behavior
	// those lines carry survives the submission (gt-x748o). These are reported
	// for the operator's benefit and are not a reason to refuse.
	Relocated []RevertedMerge
}

// DetectRevertedMerges reports the changes merged into target that
// headTreeRef undoes, and the changes it relocates within their own package.
// An empty report means headTreeRef's diff against target removes only content
// headTreeRef itself introduced.
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
// Each detection is then classified against the rest of the path's package: an
// observation whose added code survives there is a relocation, and every
// observation is one or the other (gt-x748o).
func DetectRevertedMerges(g *Git, target, headTreeRef string) (RevertReport, error) {
	mergeBase, err := g.MergeBase(target, "HEAD")
	if err != nil {
		return RevertReport{}, fmt.Errorf("resolving merge base of HEAD and %s: %w", target, err)
	}
	baseBlobs, err := g.TreeFileBlobs(mergeBase)
	if err != nil {
		return RevertReport{}, fmt.Errorf("reading tree of %s: %w", mergeBase, err)
	}
	targetBlobs, err := g.TreeFileBlobs(target)
	if err != nil {
		return RevertReport{}, fmt.Errorf("reading tree of %s: %w", target, err)
	}
	headBlobs, err := g.TreeFileBlobs(headTreeRef)
	if err != nil {
		return RevertReport{}, fmt.Errorf("reading tree of %s: %w", headTreeRef, err)
	}
	changes, err := g.CommitFileChanges(target, revertScanCommits)
	if err != nil {
		return RevertReport{}, fmt.Errorf("reading history of %s: %w", target, err)
	}

	var report RevertReport
	revertedAt := make(map[string]int)  // commit -> index in report.Reverted
	relocatedAt := make(map[string]int) // commit -> index in report.Relocated
	content := newPackageContent(g, headBlobs)

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
				return RevertReport{}, err
			}
		}
		if !reverts {
			continue
		}

		relocated, err := content.changeMoved(ch.Path, preImage, postImage)
		if err != nil {
			return RevertReport{}, fmt.Errorf("checking whether %s relocates the change to %s: %w", headTreeRef, ch.Path, err)
		}
		if relocated {
			report.Relocated = addPath(report.Relocated, relocatedAt, ch.Commit, ch.Path)
			continue
		}
		report.Reverted = addPath(report.Reverted, revertedAt, ch.Commit, ch.Path)
	}
	return report, nil
}

// addPath records path under commit in bucket, merging into the entry the same
// commit already has.
func addPath(bucket []RevertedMerge, at map[string]int, commit, path string) []RevertedMerge {
	if i, seen := at[commit]; seen {
		bucket[i].Paths = append(bucket[i].Paths, path)
		return bucket
	}
	at[commit] = len(bucket)
	return append(bucket, RevertedMerge{Commit: commit, Paths: []string{path}})
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

// packageContent reads, per directory, the lines the tree under search holds
// in that directory's files. A relocation can land in any file of the package
// — the code leaving the path it was observed on is the move — so the search
// covers the directory rather than that one path. A Go package is one
// directory; a subdirectory is a different package, and code there does not
// mean the change survived.
type packageContent struct {
	g     *Git
	blobs map[string]string          // path -> blob sha, from the tree under search
	lines map[string]map[string]bool // directory -> set of its lines, as changeMoved compares them
}

func newPackageContent(g *Git, blobs map[string]string) *packageContent {
	return &packageContent{g: g, blobs: blobs, lines: map[string]map[string]bool{}}
}

// changeMoved reports whether every line of code the change from preImage to
// postImage added at filePath still exists in filePath's package at the tree
// under search — content that moved rather than content that was deleted.
//
// Comment lines are not evidence of anything: moving code re-wraps and
// rewrites the prose around it, so a move cannot be required to reproduce them
// (the gt-x748o block arrived re-commented). Excluding them cuts both ways,
// and the second edge is the deliberate one: a change whose added lines are
// all comments has no code to find and is never excused, and a move that also
// rewrote its code keeps its classification as a revert.
func (p *packageContent) changeMoved(filePath, preImage, postImage string) (bool, error) {
	added, err := addedLines(p.g, preImage, postImage)
	if err != nil {
		return false, err
	}
	dir := path.Dir(filePath)
	code := 0
	for line := range added {
		squashed := squashLine(line)
		if squashed == "" || isComment(squashed) {
			continue
		}
		code++
		found, err := p.hasLine(dir, squashed)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
	}
	return code > 0, nil
}

// hasLine reports whether any file directly in dir holds a line that squashLine
// reduces to squashed.
func (p *packageContent) hasLine(dir, squashed string) (bool, error) {
	lines, ok := p.lines[dir]
	if !ok {
		var err error
		if lines, err = p.readDir(dir); err != nil {
			return false, err
		}
		p.lines[dir] = lines
	}
	return lines[squashed], nil
}

// readDir indexes the lines of every file directly in dir at the tree under
// search, once per directory for the whole scan.
func (p *packageContent) readDir(dir string) (map[string]bool, error) {
	lines := make(map[string]bool)
	for filePath, blob := range p.blobs {
		if path.Dir(filePath) != dir {
			continue
		}
		content, err := p.g.BlobContent(blob)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", filePath, err)
		}
		for _, line := range strings.Split(content, "\n") {
			if squashed := squashLine(line); squashed != "" {
				lines[squashed] = true
			}
		}
	}
	return lines, nil
}

// addedLines returns the multiset of lines the change from preImage to
// postImage added. BlobDiffLines answers "no lines" for a side the path is
// absent on — the honest answer for the containment test it serves, and a
// useless one here, where a file the commit CREATED added every line it holds.
func addedLines(g *Git, preImage, postImage string) (map[string]int, error) {
	if postImage == "" {
		return map[string]int{}, nil
	}
	if preImage == "" {
		content, err := g.BlobContent(postImage)
		if err != nil {
			return nil, fmt.Errorf("reading blob %s: %w", postImage, err)
		}
		counts := make(map[string]int)
		for _, line := range strings.Split(content, "\n") {
			counts[squashLine(line)]++
		}
		return counts, nil
	}
	added, _, err := g.BlobDiffLines(preImage, postImage)
	if err != nil {
		return nil, fmt.Errorf("diffing %s..%s: %w", preImage, postImage, err)
	}
	return added, nil
}

// squashLine reduces a line to what a move preserves: its characters in order,
// with every run of whitespace removed. A move rewrites whitespace wholesale —
// re-indenting into a different nesting level, and gofmt realigning a struct
// literal whose sibling keys changed — so a surviving line is not required to
// keep its original spacing.
func squashLine(line string) string {
	return strings.Join(strings.Fields(line), "")
}

// commentMarkers are the prefixes that start a comment line in the languages
// this repository's files are written in, checked against a squashed line. A
// comment carries no behavior, so its survival says nothing about whether the
// change it was part of still lives.
var commentMarkers = []string{"//", "/*", "*/", "#", "--"}

func isComment(squashed string) bool {
	for _, marker := range commentMarkers {
		if strings.HasPrefix(squashed, marker) {
			return true
		}
	}
	return false
}
