package git

import (
	"fmt"
	"path"
	"sort"
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
//
// A fourth shape is a deletion, and it arrives from the other side of the
// same question. A candidate that retires a package deletes every file in it,
// and one that carries a file into another package deletes it from the path
// the observation is anchored on; both leave the candidate's tree without a
// path a target commit wrote, which is also what a stale checkout's tree looks
// like. Reading the deletion as that inversion refused two branches that were
// doing what their beads said — retiring internal/townlog, and moving
// internal/cmd's slot tests into internal/slot — and both needed a hand
// override (gt-mdyds). So a path the candidate no longer has is read against
// the candidate's own tree instead: content that survives at a path the
// candidate added is a move (git's rename rule, and git's rename threshold),
// and a package the candidate no longer holds at all is a retirement.

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
	// candidate tip, at a new position or in a sibling file (gt-x748o), or the
	// path the target commit wrote has been carried to a path the candidate
	// added (gt-mdyds). Either way the behavior those lines carry survives the
	// submission. These are reported for the operator's benefit and are not a
	// reason to refuse.
	Relocated []RevertedMerge
}

// RevertReader is the git surface DetectRevertedMerges reads: *Git, or a
// consumer's fake of it.
type RevertReader interface {
	MergeBase(a, b string) (string, error)
	TreeFileBlobs(rev string) (map[string]string, error)
	CommitFileChanges(rev string, limit int) ([]CommitFileChange, error)
	BlobContent(sha string) (string, error)
	BlobDiffLines(oldBlob, newBlob string) (added, removed map[string]int, err error)
}

var _ RevertReader = (*Git)(nil)

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
//
// A path headTreeRef no longer has is classified against its tree rather than
// its package: content carried to a path headTreeRef added is a relocation
// (gt-mdyds), and a package headTreeRef holds no file of is a retirement, which
// is reported as neither — the candidate is not reverting the package, it is
// deleting it, which is a thing its own bead has to say and not something a
// stale tree can be distinguished from here.
func DetectRevertedMerges(g RevertReader, target, headTreeRef string) (RevertReport, error) {
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
	content := newPackageContent(g, baseBlobs, targetBlobs, headBlobs)

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

		// headTreeRef does not have this path at all. Read that against
		// headTreeRef's own tree before the package the path came from, which
		// is what separates a deletion from a stale tree that never saw the
		// commit (gt-mdyds).
		if head == "" {
			renamed, err := content.changeRenamed(ch.Path, base)
			if err != nil {
				return RevertReport{}, fmt.Errorf("checking whether %s moves %s: %w", headTreeRef, ch.Path, err)
			}
			if renamed {
				report.Relocated = addPath(report.Relocated, relocatedAt, ch.Commit, ch.Path)
				continue
			}
			if content.packageRetired(ch.Path) {
				continue
			}
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
func changeIsInvertedBy(g RevertReader, preImage, postImage, base, head string) (bool, error) {
	branchAdded, branchRemoved, err := g.BlobDiffLines(base, head)
	if err != nil {
		return false, fmt.Errorf("diffing %s..%s: %w", base, head, err)
	}
	changeAdded, changeRemoved, err := g.BlobDiffLines(preImage, postImage)
	if err != nil {
		return false, fmt.Errorf("diffing %s..%s: %w", preImage, postImage, err)
	}
	// Net each diff before comparing. A line that a diff both removes and adds
	// was moved, not removed; counting the removal alone makes a move look like
	// deletion, and for a commit that only added lines (nothing to restore, so
	// the second containment below is vacuously true) that made any relocation
	// of the added lines read as a revert (gt-tlw9u).
	branchAdded, branchRemoved = netLines(branchAdded, branchRemoved)
	changeAdded, changeRemoved = netLines(changeAdded, changeRemoved)
	if len(changeAdded)+len(changeRemoved) == 0 {
		return false, nil
	}
	return multisetContains(branchRemoved, changeAdded) && multisetContains(branchAdded, changeRemoved), nil
}

// netLines cancels lines a diff both adds and removes, returning what each side
// has left over. A line moved within a file appears once on each side and nets
// to nothing.
func netLines(added, removed map[string]int) (netAdded, netRemoved map[string]int) {
	netAdded, netRemoved = make(map[string]int), make(map[string]int)
	for line, n := range added {
		if d := n - removed[line]; d > 0 {
			netAdded[line] = d
		}
	}
	for line, n := range removed {
		if d := n - added[line]; d > 0 {
			netRemoved[line] = d
		}
	}
	return netAdded, netRemoved
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
// mean the change survived. Only a whole file carries that meaning across the
// boundary, and changeRenamed is where it does.
type packageContent struct {
	g         RevertReader
	blobs     map[string]string          // path -> blob sha, from the tree under search
	additions []string                   // paths the tree under search has and the target did not, sorted
	counts    map[string]int             // blob sha -> how many lines it holds, read once
	lines     map[string]map[string]bool // directory -> set of its lines, as changeMoved compares them
}

func newPackageContent(g RevertReader, baseBlobs, targetBlobs, headBlobs map[string]string) *packageContent {
	return &packageContent{
		g:         g,
		blobs:     headBlobs,
		additions: candidateAdditions(baseBlobs, targetBlobs, headBlobs),
		counts:    map[string]int{},
		lines:     map[string]map[string]bool{},
	}
}

// candidateAdditions returns the paths the tree under search holds that neither
// the merge base nor the target holds: the files the candidate's own work
// created, and so the only paths a file it moved can have landed on. A path the
// target holds is not where a move landed — the candidate did not put it there.
func candidateAdditions(baseBlobs, targetBlobs, headBlobs map[string]string) []string {
	var paths []string
	for filePath := range headBlobs {
		if _, inBase := baseBlobs[filePath]; inBase {
			continue
		}
		if _, inTarget := targetBlobs[filePath]; inTarget {
			continue
		}
		paths = append(paths, filePath)
	}
	sort.Strings(paths)
	return paths
}

// packageRetired reports whether the tree under search holds no file in
// filePath's directory: the package is gone from the candidate, rather than one
// file missing from a package that survives it.
func (p *packageContent) packageRetired(filePath string) bool {
	dir := path.Dir(filePath)
	for candidate := range p.blobs {
		if path.Dir(candidate) == dir {
			return false
		}
	}
	return true
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

// changeRenamed reports whether the candidate moved the file at filePath to a
// path its own work added, by the rule git's rename detection applies: at least
// half of the larger blob's lines are common to both (`-M` detects a rename at
// 50%). The file is what moves, and it is not required to arrive verbatim — the
// package clause and the references around it are rewritten where it lands —
// which is why the line containment changeMoved asks for cannot answer this,
// and why a whole file is what may cross a package boundary: a block of code
// leaving a package whose file survives is the revert gt-x748o keeps refusing,
// and this is git's rename, whatever directory it lands in (gt-mdyds).
//
// blob is the content the path held in the tree the candidate deleted it from.
func (p *packageContent) changeRenamed(filePath, blob string) (bool, error) {
	source, err := p.lineCount(blob)
	if err != nil || source == 0 {
		return false, err
	}
	for _, dest := range p.additions {
		if dest == filePath {
			continue
		}
		destBlob := p.blobs[dest]
		landed, err := p.lineCount(destBlob)
		if err != nil {
			return false, err
		}
		// The move keeps at least half of the larger blob's lines, so a pair
		// whose smaller side is under half the larger cannot be one: the diff
		// below is the only part worth asking git for, and this answers most
		// pairs without it. It also drops an empty destination, which no
		// rename produces.
		if 2*min(source, landed) < max(source, landed) {
			continue
		}
		moved, err := blobsRenamed(p.g, blob, destBlob, source, landed)
		if err != nil {
			return false, err
		}
		if moved {
			return true, nil
		}
	}
	return false, nil
}

// blobsRenamed reports whether two blobs are the two ends of one file's move,
// given the number of lines each holds. The lines common to both are the ones
// the diff does not report as added or removed; each line has to be common, so
// a line the diff reports on both sides was rewritten rather than kept.
func blobsRenamed(g RevertReader, oldBlob, newBlob string, oldLines, newLines int) (bool, error) {
	added, removed, err := g.BlobDiffLines(oldBlob, newBlob)
	if err != nil {
		return false, fmt.Errorf("diffing %s..%s: %w", oldBlob, newBlob, err)
	}
	common := min(oldLines-countLines(removed), newLines-countLines(added))
	return 2*common >= max(oldLines, newLines), nil
}

// lineCount returns how many lines a blob holds, reading each blob once for the
// whole scan.
func (p *packageContent) lineCount(blob string) (int, error) {
	if n, read := p.counts[blob]; read {
		return n, nil
	}
	content, err := p.g.BlobContent(blob)
	if err != nil {
		return 0, fmt.Errorf("reading blob %s: %w", blob, err)
	}
	n := 0
	if content != "" {
		n = strings.Count(content, "\n") + 1
	}
	p.counts[blob] = n
	return n, nil
}

// countLines returns how many lines a line multiset counts, so two diff sides
// can be subtracted from two blob sizes.
func countLines(lines map[string]int) int {
	n := 0
	for _, count := range lines {
		n += count
	}
	return n
}

// addedLines returns the multiset of lines the change from preImage to
// postImage added. BlobDiffLines answers "no lines" for a side the path is
// absent on — the honest answer for the containment test it serves, and a
// useless one here, where a file the commit CREATED added every line it holds.
func addedLines(g RevertReader, preImage, postImage string) (map[string]int, error) {
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
