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
