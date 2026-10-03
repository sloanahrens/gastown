//go:build !integration

package git

import (
	"strings"
	"testing"
)

// fakeRevertTree is a RevertReader over three trees named "base", "main" and
// "tree": the merge base, the target and the candidate under test.
type fakeRevertTree struct {
	blobs   map[string]string            // blob sha -> content
	trees   map[string]map[string]string // tree name -> path -> blob sha
	changes []CommitFileChange
}

func (f fakeRevertTree) MergeBase(a, b string) (string, error) { return "base", nil }

func (f fakeRevertTree) TreeFileBlobs(rev string) (map[string]string, error) {
	return f.trees[rev], nil
}

func (f fakeRevertTree) CommitFileChanges(rev string, limit int) ([]CommitFileChange, error) {
	return f.changes, nil
}

func (f fakeRevertTree) BlobContent(sha string) (string, error) { return f.blobs[sha], nil }

// BlobDiffLines answers what a line diff would for the fixtures here: the lines
// one blob holds that the other does not, as multisets.
func (f fakeRevertTree) BlobDiffLines(oldBlob, newBlob string) (added, removed map[string]int, err error) {
	count := func(blob string) map[string]int {
		counts := map[string]int{}
		for _, line := range strings.Split(f.blobs[blob], "\n") {
			counts[line]++
		}
		return counts
	}
	oldLines, newLines := count(oldBlob), count(newBlob)
	return subtractLines(newLines, oldLines), subtractLines(oldLines, newLines), nil
}

func subtractLines(from, minus map[string]int) map[string]int {
	left := map[string]int{}
	for line, n := range from {
		if d := n - minus[line]; d > 0 {
			left[line] = d
		}
	}
	return left
}

// detectFixReverted runs DetectRevertedMerges over a target commit that turned
// internal/pkg/lock.go from before into fixed, and a candidate that undoes it —
// lock.go back at before — while writing other as a file in another package.
// The candidate was cut after the fix, so its merge base already holds it.
func detectFixReverted(t *testing.T, before, fixed, other string) RevertReport {
	t.Helper()
	const lock = "internal/pkg/lock.go"
	g := fakeRevertTree{
		blobs: map[string]string{"before": before, "fixed": fixed, "other": other},
		trees: map[string]map[string]string{
			"base": {lock: "fixed"},
			"main": {lock: "fixed"},
			"tree": {lock: "before", "internal/other/other.go": "other"},
		},
		changes: []CommitFileChange{{Commit: "fix", Path: lock, OldBlob: "before", NewBlob: "fixed"}},
	}
	report, err := DetectRevertedMerges(g, "main", "tree")
	if err != nil {
		t.Fatalf("DetectRevertedMerges: %v", err)
	}
	return report
}

// A one-line fix that a stale branch undoes stays a revert when the branch also
// wrote an unrelated file holding a look-alike of the line the fix added
// (gt-s5eou).
func TestDetectRevertedMergesRefusesAnUnlockFixUndoneBesideAnUnrelatedFile(t *testing.T) {
	t.Parallel()
	const before = `package pkg

func (s *store) put(k string) {
	s.mu.Lock()
	s.items[k] = true
	s.mu.Unlock()
}
`
	const fixed = `package pkg

func (s *store) put(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[k] = true
}
`
	const other = `package other

func release(x *guard) {
	x.Unlock()
}
`
	report := detectFixReverted(t, before, fixed, other)
	if len(report.Relocated) != 0 {
		t.Errorf("a one-line fix was read as relocated: %+v", report.Relocated)
	}
	if len(report.Reverted) != 1 {
		t.Fatalf("got %d reverted commits, want 1: %+v", len(report.Reverted), report.Reverted)
	}
}

// A hunk big enough to have been carried is excused only by a file that holds
// all of it: with the keyword gone from one line, the file holds another block.
func TestDetectRevertedMergesKeepsTheKeywordOnACarriedBlock(t *testing.T) {
	t.Parallel()
	const before = `package pkg

func run(path string) {
	state.mu.Lock()
	state.mu.Unlock()
}
`
	const fixed = `package pkg

func run(path string) {
	state.mu.Lock()
	cfg := loadConfig(path)
	state.ready = markReady(cfg)
	state.names = collectNames(cfg)
	defer state.mu.Unlock()
}
`
	const carried = `package other

func run(path string) {
	cfg := loadConfig(path)
	s.ready = markReady(cfg)
	s.names = collectNames(cfg)
	defer s.mu.Unlock()
}
`
	const keywordDropped = `package other

func run(path string) {
	cfg := loadConfig(path)
	s.ready = markReady(cfg)
	s.names = collectNames(cfg)
	s.mu.Unlock()
}
`
	if report := detectFixReverted(t, before, fixed, carried); len(report.Reverted) != 0 || len(report.Relocated) != 1 {
		t.Errorf("a block carried to another package: reverted %+v, relocated %+v; want a relocation", report.Reverted, report.Relocated)
	}
	if report := detectFixReverted(t, before, fixed, keywordDropped); len(report.Reverted) != 1 || len(report.Relocated) != 0 {
		t.Errorf("a block that lost its defer: reverted %+v, relocated %+v; want a revert", report.Reverted, report.Relocated)
	}
}

// Under minMovedCodeLines the cross-package reading excuses nothing, even when
// another package holds every line.
func TestDetectRevertedMergesNeedsMinMovedCodeLinesToExcuseAMove(t *testing.T) {
	t.Parallel()
	const before = `package pkg

func run(path string) {
	state.mu.Lock()
}
`
	const fixed = `package pkg

func run(path string) {
	state.mu.Lock()
	cfg := loadConfig(path)
	state.ready = markReady(cfg)
}
`
	const carried = `package other

func run(path string) {
	cfg := loadConfig(path)
	s.ready = markReady(cfg)
}
`
	report := detectFixReverted(t, before, fixed, carried)
	if len(report.Reverted) != 1 || len(report.Relocated) != 0 {
		t.Errorf("a two-line block: reverted %+v, relocated %+v; want a revert", report.Reverted, report.Relocated)
	}
}

// blankLines writes the blank lines a fixture holds, so the counts these tests
// turn on are read off the construction rather than eyed in a raw string.
func blankLines(n int) string { return strings.Repeat("\n", n) }

// A commit that only strips blank lines is not merged content a branch can
// undo. With a blank line keyed as "" the strip reads as changeRemoved = {"": 5},
// and a branch whose diff adds ten blank lines to the same test file contains
// it — so the branch was refused for reverting a commit that removed no
// content (gt-cqzm3).
func TestDetectRevertedMergesIgnoresBlankLinesAsContent(t *testing.T) {
	t.Parallel()
	const configTest = "internal/cmd/config_test.go"
	stripped := "package cmd\n" + blankLines(6) + "func TestFoo(t *testing.T) {\n}\n"
	strippedOut := "package cmd\n\nfunc TestFoo(t *testing.T) {\n}\n"
	branch := "package cmd\n" + blankLines(9) + "func TestFoo(t *testing.T) {\n}\n\n\nfunc TestBar(t *testing.T) {\n}\n"
	g := fakeRevertTree{
		blobs: map[string]string{"stripped": stripped, "strippedOut": strippedOut, "branch": branch},
		trees: map[string]map[string]string{
			"base": {configTest: "strippedOut"},
			"main": {configTest: "strippedOut"},
			"tree": {configTest: "branch"},
		},
		changes: []CommitFileChange{{Commit: "strip", Path: configTest, OldBlob: "stripped", NewBlob: "strippedOut"}},
	}
	report, err := DetectRevertedMerges(g, "main", "tree")
	if err != nil {
		t.Fatalf("DetectRevertedMerges: %v", err)
	}
	if len(report.Reverted) != 0 || len(report.Relocated) != 0 {
		t.Errorf("a branch that adds blank lines: reverted %+v, relocated %+v; want neither", report.Reverted, report.Relocated)
	}
}

// Dropping blank lines must not cost a real revert: the branch here is a stale
// tree that never saw the line the commit added, and the blank lines it carries
// are not what detects it.
func TestDetectRevertedMergesStillCatchesARealRevertBesideBlankLines(t *testing.T) {
	t.Parallel()
	const configTest = "internal/cmd/config_test.go"
	const before = "package cmd\n\nfunc TestFoo(t *testing.T) {\n}\n"
	const after = "package cmd\n\nfunc TestFoo(t *testing.T) {\n\tcheckGot(t, want, got)\n}\n"
	branch := "package cmd\n" + blankLines(4) + "func TestFoo(t *testing.T) {\n}\n"
	g := fakeRevertTree{
		blobs: map[string]string{"before": before, "after": after, "branch": branch},
		trees: map[string]map[string]string{
			"base": {configTest: "after"},
			"main": {configTest: "after"},
			"tree": {configTest: "branch"},
		},
		changes: []CommitFileChange{{Commit: "fix", Path: configTest, OldBlob: "before", NewBlob: "after"}},
	}
	report, err := DetectRevertedMerges(g, "main", "tree")
	if err != nil {
		t.Fatalf("DetectRevertedMerges: %v", err)
	}
	if len(report.Reverted) != 1 || len(report.Relocated) != 0 {
		t.Errorf("a stale branch: reverted %+v, relocated %+v; want the commit reverted", report.Reverted, report.Relocated)
	}
}

// The gt-59p7e shape in miniature: two preload reads folded into one combined
// call. preloadTwoReads is the file before main's first commit, preloadCollapsed
// after it merged the wisps reads, preloadWithIssues after the purely additive
// commit that added the issues read, and preloadCombined is a branch that
// supersedes both reads with one call.
const (
	supersededPath = "internal/cmd/polecat.go"

	preloadTwoReads = `package cmd

func buildRigSeats(b *bd.Client) {
	if err := b.ListLabeledWisps("gt:agent"); err != nil {
		warnf(err)
	}
	if err := b.ListLabeledWisps("gt:merge-request"); err != nil {
		warnf(err)
	}
}
`

	preloadCollapsed = `package cmd

func buildRigSeats(b *bd.Client) {
	// ONE wisps read for both label sets this listing wants.
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		warnf(err)
	}
}
`

	preloadWithIssues = `package cmd

func buildRigSeats(b *bd.Client) {
	// ONE wisps read for both label sets this listing wants.
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		warnf(err)
	}

	// The issues read, one more round trip.
	if err := b.PreloadIssues([]string{"gt:agent", "gt:merge-request"}, workStatuses); err != nil {
		warnf(err)
	}
}
`

	preloadCombined = `package cmd

func buildRigSeats(b *bd.Client) {
	// ONE read for both tables.
	if err := b.PreloadBeads([]string{"gt:agent", "gt:merge-request"}, workStatuses); err != nil {
		warnf(err)
	}
}
`

	// preloadEdited is the read dropped with no call written over it: the
	// branch's own work sits beside the hole it left.
	preloadEdited = `package cmd

func buildRigSeats(b *bd.Client) {
	// ONE wisps read for both label sets this listing wants.
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		warnf(err)
	}
	audit("seats")
}
`
)

// supersededPreloadTree builds those four blobs into an observation: main's tip
// is the additive issues-read commit, and the candidate writes candidateBlob.
func supersededPreloadTree(candidateBlob string) fakeRevertTree {
	return fakeRevertTree{
		blobs: map[string]string{
			"twoReads": preloadTwoReads, "collapsed": preloadCollapsed,
			"withIssues": preloadWithIssues, "combined": preloadCombined,
			"edited": preloadEdited,
		},
		trees: map[string]map[string]string{
			"base": {supersededPath: "withIssues"},
			"main": {supersededPath: "withIssues"},
			"tree": {supersededPath: candidateBlob},
		},
		changes: []CommitFileChange{
			{Commit: "collapse the wisps reads", Path: supersededPath, OldBlob: "twoReads", NewBlob: "collapsed"},
			{Commit: "add the issues read", Path: supersededPath, OldBlob: "collapsed", NewBlob: "withIssues"},
		},
	}
}

// A branch that SUPERSEDES a merged change is not undoing it. The read this
// branch deletes was added on its own, so the change has nothing to restore and
// nothing in the second containment weighed against the deletion — it was
// refused for doing what its bead asked, merging the two reads into one call
// (gt-gas9g).
func TestDetectRevertedMergesReadsASupersedingEditAsNoRevert(t *testing.T) {
	t.Parallel()
	report, err := DetectRevertedMerges(supersededPreloadTree("combined"), "main", "tree")
	if err != nil {
		t.Fatalf("DetectRevertedMerges: %v", err)
	}
	if len(report.Reverted) != 0 || len(report.Relocated) != 0 {
		t.Errorf("a branch that supersedes the added read with one combined call: reverted %+v, relocated %+v; want neither",
			report.Reverted, report.Relocated)
	}
}

// The other edge of that reading: the same tree dropping the added read and
// writing an edit of its own is still undoing the change. Nothing covers the
// deleted lines, so the removal is the whole of the addition and the
// observation stands (gt-gas9g).
func TestDetectRevertedMergesStillRefusesADeletedAdditionBesideAnEdit(t *testing.T) {
	t.Parallel()
	report, err := DetectRevertedMerges(supersededPreloadTree("edited"), "main", "tree")
	if err != nil {
		t.Fatalf("DetectRevertedMerges: %v", err)
	}
	if len(report.Reverted) != 1 || len(report.Relocated) != 0 {
		t.Errorf("a tree that drops the added read beside an edit of its own: reverted %+v, relocated %+v; want the commit reverted",
			report.Reverted, report.Relocated)
	}
}

// The gt-qw70y shape in miniature: main added one row to a docs table, and the
// branch reworded that row in place. The row is there, updated; nothing merged
// is taken back — but the branch removes exactly the line main added, so an
// addition with no removal beside it reads as undone.
const (
	tablePath = "docs/testing.md"

	tableBefore = "| target | command | runs |\n" +
		"| `make test` | `go test ./...` | never |\n"

	tableWithPresubmitRow = tableBefore +
		"| `make presubmit` | `make lint`, then `go build ./...` | never | the cheap first look (gt-ssyxd) |\n"

	tableRowReworded = tableBefore +
		"| `make presubmit` | `make lint`, then `go build ./...` | never | the cheap first look, before the push (gt-ssyxd) |\n"

	tableRowDropped = tableBefore +
		"| `make bench` | `go test -bench=. ./...` | weekly | nightly bench numbers (gt-bench) |\n"
)

// rewordedTableTree builds those blobs into an observation: main's tip is the
// table with the added row, and the candidate writes candidateBlob over it.
func rewordedTableTree(candidateBlob string) fakeRevertTree {
	return fakeRevertTree{
		blobs: map[string]string{
			"before": tableBefore, "withRow": tableWithPresubmitRow,
			"reworded": tableRowReworded, "dropped": tableRowDropped,
		},
		trees: map[string]map[string]string{
			"base": {tablePath: "withRow"},
			"main": {tablePath: "withRow"},
			"tree": {tablePath: candidateBlob},
		},
		changes: []CommitFileChange{
			{Commit: "add the presubmit row", Path: tablePath, OldBlob: "before", NewBlob: "withRow"},
		},
	}
}

// A branch that edits an added line in place is superseding it, not undoing
// it: the line the change added is gone from the branch's tree because the
// branch wrote its reworded copy in the same place (gt-qw70y).
func TestDetectRevertedMergesReadsARewordedAdditionAsNoRevert(t *testing.T) {
	t.Parallel()
	report, err := DetectRevertedMerges(rewordedTableTree("reworded"), "main", "tree")
	if err != nil {
		t.Fatalf("DetectRevertedMerges: %v", err)
	}
	if len(report.Reverted) != 0 || len(report.Relocated) != 0 {
		t.Errorf("a branch that rewords the added row in place: reverted %+v, relocated %+v; want neither",
			report.Reverted, report.Relocated)
	}
}

// The edge that reading must not cross: an addition the branch drops while
// writing a line of its own over the hole is undoing the change. The line it
// wrote shares only a table's scaffolding with the one it dropped.
func TestDetectRevertedMergesStillRefusesADroppedAdditionBesideItsOwnRow(t *testing.T) {
	t.Parallel()
	report, err := DetectRevertedMerges(rewordedTableTree("dropped"), "main", "tree")
	if err != nil {
		t.Fatalf("DetectRevertedMerges: %v", err)
	}
	if len(report.Reverted) != 1 || len(report.Relocated) != 0 {
		t.Errorf("a tree that drops the added row beside a row of its own: reverted %+v, relocated %+v; want the commit reverted",
			report.Reverted, report.Relocated)
	}
}
