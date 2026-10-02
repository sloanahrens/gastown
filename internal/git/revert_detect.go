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
//
// A fifth shape crosses the package boundary, and none of the readings above
// sees it. The thin cobra slice that hands its engine to internal/<leaf>/
// deletes functions from a file that keeps existing — so the package the path
// belongs to no longer holds them — and their code arrives in a file written
// from several, not in a whole file carried across (the fourth shape's
// question). Crossing the boundary also rewrites the qualifiers around the
// code: params.BeadID lands as opts.BeadID, a local type arrives exported. So
// a removed hunk is read as moved when one file the candidate's own work wrote
// holds its lines with those qualifiers compared away (gt-bbk1f). That reading
// is the loosest of the five, so it also asks for volume: a removed hunk of a
// line or two is what a one-line fix looks like, and such a line turns up in
// any unrelated file, so only a hunk of minMovedCodeLines substantial lines is
// excused by it (gt-s5eou).
//
// A sixth shape inverts the premise: not a candidate undoing a change but one
// SUPERSEDING it. A branch that rewrites the merged reads into one combined call
// deletes the lines the change added while the behavior they carried lives on in
// the line beside them, and where the change only added lines nothing weighed
// against the deletion — the other containment is vacuous for such a change — so
// the branch was refused for doing what its bead asked (gt-gas9g). A change with
// nothing to restore now has to have been removed in full.

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
	// of code the target commit added still exists at the candidate tip, at a
	// new position or in a sibling file (gt-x748o), in a file the candidate's
	// own work wrote anywhere in the tree with the qualifiers the move rewrote
	// compared away (gt-bbk1f), or the path the target commit wrote has been
	// carried to a path the candidate added (gt-mdyds). Either way the behavior
	// those lines carry survives the submission. These are reported for the
	// operator's benefit and are not a reason to refuse.
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
// Each detection is then classified against the rest of the tree: an
// observation whose added code survives elsewhere is a relocation, and every
// observation is one or the other (gt-x748o, gt-bbk1f). The search starts with
// the path's own package — a Go package is one directory — and, when the code
// is not there, asks every file the candidate's own work wrote, wherever it
// is, comparing lines with the qualifiers a move rewrites taken out.
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
	content := newCandidateContent(g, baseBlobs, targetBlobs, headBlobs)

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
// removed, is not reverting it. A commit that only added lines has nothing to
// restore, which makes the second direction vacuous; the first one then has to
// hold as equality, or a branch that deletes the lines while writing a
// replacement of its own would read as undoing them (gt-gas9g).
//
// Blank lines take no part in either direction. BlobDiffLines keys a blank line
// as "" like any other, so a commit that only strips blank lines reads as
// changeRemoved = {"": n} — which any branch whose diff adds n blank lines
// contains, while changeAdded empty makes the other containment vacuous. That
// pair refused a branch for undoing a commit that changed no content
// (gt-cqzm3). Filtering every side keeps both containments about content.
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
	branchAdded, branchRemoved = nonBlankLines(branchAdded), nonBlankLines(branchRemoved)
	changeAdded, changeRemoved = nonBlankLines(changeAdded), nonBlankLines(changeRemoved)
	// An empty side is contained by everything and contains everything, so it
	// would satisfy half of the conjunction for free: only a comparison with
	// content on both sides is a reading at all.
	if len(changeAdded)+len(changeRemoved) == 0 || len(branchAdded)+len(branchRemoved) == 0 {
		return false, nil
	}
	// A change with nothing to restore leaves the second containment free: an
	// empty multiset is contained by anything, so however much the branch writes
	// over the lines it deletes, deleting them reads as an inversion. That
	// refused a branch whose edit SUPERSEDED the change instead of undoing it —
	// the PreloadIssues call deleted beside the single combined call that now
	// does its work, which is what the bead asked for (gt-gas9g). With nothing
	// to restore, the removal must be the whole of the addition: a revert takes
	// back everything the commit put there, while a superseding edit keeps the
	// lines its replacement also needs — the warning, the closing brace — and
	// rewrites the ones it replaces.
	if len(changeRemoved) == 0 {
		return multisetContains(branchRemoved, changeAdded) && multisetContains(changeAdded, branchRemoved), nil
	}
	return multisetContains(branchRemoved, changeAdded) && multisetContains(branchAdded, changeRemoved), nil
}

// nonBlankLines drops whitespace-only lines from one side of a diff. squashLine
// reduces a line to its fields, so a line of spaces leaves with the "" a blank
// line arrives as.
func nonBlankLines(lines map[string]int) map[string]int {
	kept := make(map[string]int, len(lines))
	for line, count := range lines {
		if squashLine(line) != "" {
			kept[line] = count
		}
	}
	return kept
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

// candidateContent reads the tree under search in the two shapes the
// relocation readings need, each indexed once for the whole scan: the lines
// each directory of the tree holds (a move within a package lands in one of
// its files, whatever file that is), and — for the move that leaves the
// package altogether — the lines of each file the candidate's own work wrote.
//
// A Go package is one directory; a subdirectory is a different package, so the
// per-directory reading stops at the boundary by construction. Only a whole
// file carries that meaning across it as a file, and changeRenamed is where it
// does; the code inside a file that crossed is read by changeMoved's second
// reading instead.
type candidateContent struct {
	g         RevertReader
	blobs     map[string]string          // path -> blob sha, from the tree under search
	base      map[string]string          // path -> blob sha, from the merge base
	additions []string                   // paths the tree under search has and the target did not, sorted
	counts    map[string]int             // blob sha -> how many lines it holds, read once
	lines     map[string]map[string]bool // directory -> set of its lines, as changeMoved compares them
	moved     map[string]map[string]bool // path -> set of its lines with qualifiers stripped
	written   []string                   // paths the candidate's own work wrote, sorted; nil until read
}

func newCandidateContent(g RevertReader, baseBlobs, targetBlobs, headBlobs map[string]string) *candidateContent {
	return &candidateContent{
		g:         g,
		blobs:     headBlobs,
		base:      baseBlobs,
		additions: candidateAdditions(baseBlobs, targetBlobs, headBlobs),
		counts:    map[string]int{},
		lines:     map[string]map[string]bool{},
		moved:     map[string]map[string]bool{},
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
func (p *candidateContent) packageRetired(filePath string) bool {
	dir := path.Dir(filePath)
	for candidate := range p.blobs {
		if path.Dir(candidate) == dir {
			return false
		}
	}
	return true
}

// changeMoved reports whether every line of code the change from preImage to
// postImage added at filePath still exists at the tree under search — content
// that moved rather than content that was deleted.
//
// Two readings, the second the first widened. A move within the path's own
// package lands in a file that package holds, so the directory is asked first
// (gt-x748o). A move OUT of the package — the thin cobra slice handing its
// engine to a leaf package, internal/cmd/<x>.go into internal/<leaf>/ — leaves
// nothing there, and is asked of every file the candidate's own work wrote,
// anywhere in the tree (gt-bbk1f).
//
// The two are compared differently, because what a move preserves differs.
// Within a package the lines arrive as they were (at most re-indented, which
// squashLine already forgives), so a bag of them gathered from the package's
// files answers. Across a package boundary the move rewrites the qualifiers
// around the code — params.BeadID leaves as opts.BeadID — so the lines are
// compared with those gone (moveLine), and must be found together in ONE file:
// a line that survived on its own somewhere in the tree is not the block that
// arrived somewhere else. Only a change of minMovedCodeLines substantial lines
// can be read that way at all.
//
// Comment lines are not evidence of anything: moving code re-wraps and
// rewrites the prose around it, so a move cannot be required to reproduce them
// (the gt-x748o block arrived re-commented). Excluding them cuts both ways,
// and the second edge is the deliberate one: a change whose added lines are
// all comments has no code to find and is never excused, and a move that also
// rewrote its code keeps its classification as a revert.
func (p *candidateContent) changeMoved(filePath, preImage, postImage string) (bool, error) {
	added, err := addedLines(p.g, preImage, postImage)
	if err != nil {
		return false, err
	}
	var code []codeLine
	substantial := 0
	for line := range added {
		squashed := squashLine(line)
		if squashed == "" || isComment(squashed) {
			continue
		}
		code = append(code, codeLine{squashed: squashed, moved: moveLine(line)})
		if substantialLine(line) {
			substantial++
		}
	}
	if len(code) == 0 {
		return false, nil
	}
	inPackage, err := p.survivesInPackage(filePath, code)
	if err != nil || inPackage {
		return inPackage, err
	}
	if substantial < minMovedCodeLines {
		return false, nil
	}
	return p.survivesInWrittenFile(filePath, code)
}

// codeLine is one added line of code in the two reductions the relocation
// readings compare it by: squashLine for a move within the package, moveLine
// for a move out of it. moveLine needs the line as written, so the line cannot
// travel squashed alone.
type codeLine struct {
	squashed string
	moved    string
}

// survivesInPackage reports whether every line in code is held by a file
// directly in filePath's directory at the tree under search.
func (p *candidateContent) survivesInPackage(filePath string, code []codeLine) (bool, error) {
	dir := path.Dir(filePath)
	for _, line := range code {
		found, err := p.hasLine(dir, line.squashed)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// survivesInWrittenFile reports whether one file the candidate's own work
// wrote elsewhere in the tree holds a copy of every line in code, each
// compared with the qualifiers a move rewrites taken out (moveLine).
func (p *candidateContent) survivesInWrittenFile(filePath string, code []codeLine) (bool, error) {
	for _, dest := range p.writtenPaths() {
		if dest == filePath {
			continue
		}
		lines, err := p.movedLines(dest)
		if err != nil {
			return false, err
		}
		holds := true
		for _, line := range code {
			if !lines[line.moved] {
				holds = false
				break
			}
		}
		if holds {
			return true, nil
		}
	}
	return false, nil
}

// writtenPaths returns the paths the candidate's own work wrote: the tree under
// search holds them with content the merge base does not have. A path the
// candidate left alone — including one the target moved on past a stale
// checkout without the candidate touching it — carries the content it started
// from, so nothing the candidate moved can have landed on it. That is also
// what keeps the deletion of a block boilerplate repeats from being answered by
// a file nobody wrote, where those lines have always been.
func (p *candidateContent) writtenPaths() []string {
	if p.written == nil {
		p.written = []string{}
		for filePath, blob := range p.blobs {
			if p.base[filePath] != blob {
				p.written = append(p.written, filePath)
			}
		}
		sort.Strings(p.written)
	}
	return p.written
}

// movedLines indexes the lines of one written file with moveLine, once for the
// whole scan.
func (p *candidateContent) movedLines(filePath string) (map[string]bool, error) {
	if lines, read := p.moved[filePath]; read {
		return lines, nil
	}
	content, err := p.g.BlobContent(p.blobs[filePath])
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filePath, err)
	}
	lines := make(map[string]bool)
	for _, line := range strings.Split(content, "\n") {
		if moved := moveLine(line); moved != "" {
			lines[moved] = true
		}
	}
	p.moved[filePath] = lines
	return lines, nil
}

// hasLine reports whether any file directly in dir holds a line that squashLine
// reduces to squashed.
func (p *candidateContent) hasLine(dir, squashed string) (bool, error) {
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
func (p *candidateContent) readDir(dir string) (map[string]bool, error) {
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
func (p *candidateContent) changeRenamed(filePath, blob string) (bool, error) {
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
func (p *candidateContent) lineCount(blob string) (int, error) {
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
			counts[line]++
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

// moveLine reduces a line to what a move OUT of its package preserves: the
// squashLine reduction, with every selector chain collapsed to the name it
// ends on. Crossing a package boundary rewrites what stands in front of the
// dot — a local variable or receiver renamed on the way (params.BeadID leaves
// as opts.BeadID), a package's own type arriving exported, a symbol the new
// package reaches for gaining that package's name, and the reverse for the one
// it left (gt-bbk1f).
//
// The chains collapse before the whitespace goes: squashed first, `defer
// mu.Unlock()` reads as the identifier defermu and its keyword is stripped as
// a qualifier, leaving a line equal to the one the fix removed (gt-s5eou).
func moveLine(line string) string {
	return squashLine(stripQualifiers(line))
}

// minMovedCodeLines is how many substantial lines a removed hunk must carry
// for a copy in another package to excuse it. One substantial line is a
// one-line fix and two can be a coincidence of an unrelated file; three
// matching in a single file is a block that was carried. Refusing a smaller
// real move costs an operator override, excusing a stale revert costs the
// merged fix (gt-s5eou).
const minMovedCodeLines = 3

// minLineNames is how many identifiers a line needs to be substantial. A
// closing brace, `return err` and a lone call such as `mu.Unlock()` have fewer,
// and are lines any file holds.
const minLineNames = 2

// goWords are the identifiers that carry no name of their own: the keywords
// and the predeclared constants.
var goWords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
	"nil": true, "true": true, "false": true, "iota": true,
}

// substantialLine reports whether line names at least minLineNames things once
// keywords, literals, a trailing comment and the qualifiers a move rewrites are
// set aside.
func substantialLine(line string) bool {
	stripped := stripQualifiers(line)
	names := 0
	for i := 0; i < len(stripped); {
		c := stripped[i]
		switch {
		case c == '"' || c == '`' || c == '\'':
			i = literalEnd(stripped, i)
		case c == '/' && i+1 < len(stripped) && stripped[i+1] == '/':
			return names >= minLineNames
		case c >= '0' && c <= '9':
			for i < len(stripped) && isIdentChar(stripped[i]) {
				i++
			}
		default:
			name := identifierAt(stripped, i)
			if name == "" {
				i++
				continue
			}
			i += len(name)
			if !goWords[name] {
				names++
			}
		}
	}
	return names >= minLineNames
}

// stripQualifiers rewrites every identifier chain (a.b.c) to the name it ends
// on, leaving string literals and runes alone: a path, a format string or a
// shell command carries meaning a chain of selectors does not, and rewriting
// text inside one would compare two different strings as equal. A number does
// not start an identifier, so a float is not a chain: 1.5 stays 1.5.
func stripQualifiers(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if c := line[i]; c == '"' || c == '`' || c == '\'' {
			end := literalEnd(line, i)
			b.WriteString(line[i:end])
			i = end
			continue
		}
		name := identifierAt(line, i)
		if name == "" {
			b.WriteByte(line[i])
			i++
			continue
		}
		last, end := name, i+len(name)
		for end < len(line) && line[end] == '.' {
			next := identifierAt(line, end+1)
			if next == "" {
				break
			}
			last, end = next, end+1+len(next)
		}
		b.WriteString(last)
		i = end
	}
	return b.String()
}

// identifierAt returns the identifier starting at i, or "" when i does not
// start one. Only ASCII: an identifier in another script does not start one
// here, so its bytes pass through untouched, which costs a match rather than
// inventing one.
func identifierAt(s string, i int) string {
	if i >= len(s) || !isIdentStart(s[i]) {
		return ""
	}
	j := i + 1
	for j < len(s) && isIdentChar(s[j]) {
		j++
	}
	return s[i:j]
}

// literalEnd returns the index just past the string literal or rune starting
// at i. A backquoted string has no escapes; the others take a backslash, which
// is what keeps the quote it escapes from ending the literal.
func literalEnd(s string, i int) int {
	quote := s[i]
	for j := i + 1; j < len(s); {
		switch {
		case s[j] == '\\' && quote != '`':
			j += 2
		case s[j] == quote:
			return j + 1
		default:
			j++
		}
	}
	return len(s)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
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
