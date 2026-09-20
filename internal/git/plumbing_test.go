package git

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCommitFileChanges_ReportsEveryFileOfEveryCommit pins the -z parsing of
// git log --raw. git separates a commit's header from its raw block with a
// blank line, and under -z that newline arrives as a leading "\n" on the FIRST
// meta token of every commit. A parser that classifies tokens by their raw
// prefix therefore drops the first file of every commit — silently, and for
// every commit in the scan, which is the exact failure an unseen-file guard
// cannot afford.
func TestCommitFileChanges_ReportsEveryFileOfEveryCommit(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)

	// One commit touching several paths, so the first entry of the raw block is
	// distinguishable from the rest.
	write(t, filepath.Join(dir, "alpha.txt"), "alpha\n")
	write(t, filepath.Join(dir, "beta.txt"), "beta\n")
	write(t, filepath.Join(dir, "README.md"), "# Test\nmore\n")
	if _, err := g.run("add", "-A"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := g.run("commit", "-m", "several files"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	changes, err := g.CommitFileChanges("HEAD", 10)
	if err != nil {
		t.Fatalf("CommitFileChanges: %v", err)
	}
	// Changes arrive newest-first, and the older "initial" commit also touches
	// README.md, so keep each path's newest entry.
	got := map[string]CommitFileChange{}
	for _, ch := range changes {
		if ch.Commit == "" {
			t.Fatalf("change with no commit: %+v", ch)
		}
		if _, seen := got[ch.Path]; !seen {
			got[ch.Path] = ch
		}
	}
	for _, path := range []string{"alpha.txt", "beta.txt", "README.md"} {
		ch, ok := got[path]
		if !ok {
			t.Fatalf("CommitFileChanges did not report %s (reported: %v)", path, keys(got))
		}
		if ch.NewBlob == "" {
			t.Errorf("%s: new=%q, want the committed content's blob", path, ch.NewBlob)
		}
	}
	// README.md existed before the commit, so its entry must carry both sides;
	// the two new files must report no pre-image at all.
	if ch := got["README.md"]; ch.OldBlob == "" {
		t.Errorf("README.md: old=%q, want the pre-change blob for a modification", ch.OldBlob)
	}
	for _, path := range []string{"alpha.txt", "beta.txt"} {
		if ch := got[path]; ch.OldBlob != "" {
			t.Errorf("%s: old=%q, want empty for a creation", path, ch.OldBlob)
		}
	}
}

// TestCommitFileChanges_CreationAndDeletionUseEmptyNotNullBlob pins the blob
// spelling: callers compare these values against blobs read from trees, and
// git's all-zero null object is not a blob any tree reports.
func TestCommitFileChanges_CreationAndDeletionUseEmptyNotNullBlob(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)

	write(t, filepath.Join(dir, "gone.txt"), "temporary\n")
	if _, err := g.run("add", "-A"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := g.run("commit", "-m", "add file"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := g.run("rm", "gone.txt"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, err := g.run("commit", "-m", "remove file"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	changes, err := g.CommitFileChanges("HEAD", 1)
	if err != nil {
		t.Fatalf("CommitFileChanges: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	if changes[0].NewBlob != "" {
		t.Errorf("deletion reported NewBlob=%q, want empty", changes[0].NewBlob)
	}
	if changes[0].OldBlob == "" || changes[0].OldBlob == nullBlob {
		t.Errorf("deletion reported OldBlob=%q, want the removed content's blob", changes[0].OldBlob)
	}
}

// TestBlobDiffLines exercises the containment primitive the revert check is
// built on: which lines one side adds and removes relative to the other.
func TestBlobDiffLines(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)

	before, err := g.run("rev-parse", "HEAD:README.md")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	write(t, filepath.Join(dir, "README.md"), "# Changed\n")
	if _, err := g.run("commit", "-am", "change readme"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	after, err := g.run("rev-parse", "HEAD:README.md")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}

	added, removed, err := g.BlobDiffLines(before, after)
	if err != nil {
		t.Fatalf("BlobDiffLines: %v", err)
	}
	if added["# Changed"] != 1 {
		t.Errorf("added = %v, want the new line", added)
	}
	if removed["# Test"] != 1 {
		t.Errorf("removed = %v, want the old line", removed)
	}

	// Both directions invert, which is what lets the check compare a branch's
	// diff against a commit's diff.
	backAdded, backRemoved, err := g.BlobDiffLines(after, before)
	if err != nil {
		t.Fatalf("BlobDiffLines reversed: %v", err)
	}
	if backAdded["# Test"] != 1 || backRemoved["# Changed"] != 1 {
		t.Errorf("reversed diff added=%v removed=%v, want the swap", backAdded, backRemoved)
	}

	// An absent side is not a whole-file rewrite: callers ask "is this change
	// contained in that one", and "absent" cannot answer it.
	noneAdded, noneRemoved, err := g.BlobDiffLines("", after)
	if err != nil {
		t.Fatalf("BlobDiffLines with absent side: %v", err)
	}
	if len(noneAdded) != 0 || len(noneRemoved) != 0 {
		t.Errorf("absent-side diff reported added=%v removed=%v, want nothing", noneAdded, noneRemoved)
	}
}

// TestTreeFileBlobs checks the path -> blob view both sides of every comparison
// are read from.
func TestTreeFileBlobs(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)

	blobs, err := g.TreeFileBlobs("HEAD")
	if err != nil {
		t.Fatalf("TreeFileBlobs: %v", err)
	}
	readme, ok := blobs["README.md"]
	if !ok {
		t.Fatalf("README.md missing from %v", keysOf(blobs))
	}
	want, err := g.run("rev-parse", "HEAD:README.md")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if readme != want {
		t.Errorf("README.md blob = %s, want %s", readme, want)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestTreesIdentical(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)

	if _, err := g.run("checkout", "-b", "same"); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	identical, err := g.TreesIdentical("main", "same")
	if err != nil {
		t.Fatalf("TreesIdentical: %v", err)
	}
	if !identical {
		t.Error("descendant branch with no commits reported as differing from its base")
	}

	write(t, filepath.Join(dir, "extra.txt"), "extra\n")
	if _, err := g.run("add", "."); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := g.run("commit", "-m", "add extra"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	identical, err = g.TreesIdentical("main", "same")
	if err != nil {
		t.Fatalf("TreesIdentical after commit: %v", err)
	}
	if identical {
		t.Error("branch that adds a file reported as having its base's tree")
	}
}

// TestCommitLineStatsInRange pins the two shapes git log --numstat emits inside
// a range: a commit with changes prints a blank line and its per-file counts,
// and a commit with none — an empty commit — prints nothing at all, so the next
// hash line follows the subject directly. A parser that assumes the blank line
// is present loses every commit after an empty one.
func TestCommitLineStatsInRange(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)

	if _, err := g.run("checkout", "-b", "work"); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	write(t, filepath.Join(dir, "payload.txt"), "one\ntwo\nthree\n")
	if _, err := g.run("add", "."); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := g.run("commit", "-m", "feat: add payload"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := g.run("commit", "--allow-empty", "-m", "chore: no-op"); err != nil {
		t.Fatalf("empty commit: %v", err)
	}
	if _, err := g.run("rm", "payload.txt"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, err := g.run("commit", "-m", "fix: drop payload"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	stats, err := g.CommitLineStatsInRange("main..work", 10)
	if err != nil {
		t.Fatalf("CommitLineStatsInRange: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("got %d commits, want 3: %+v", len(stats), stats)
	}
	if stats[0].Subject != "fix: drop payload" || stats[0].Added != 0 || stats[0].Removed != 3 {
		t.Errorf("newest = %+v, want a 3-line removal", stats[0])
	}
	if stats[1].Subject != "chore: no-op" || stats[1].Added != 0 || stats[1].Removed != 0 {
		t.Errorf("empty commit = %+v, want zero counts", stats[1])
	}
	if stats[2].Subject != "feat: add payload" || stats[2].Added != 3 || stats[2].Removed != 0 {
		t.Errorf("oldest = %+v, want a 3-line addition", stats[2])
	}

	limited, err := g.CommitLineStatsInRange("main..work", 1)
	if err != nil {
		t.Fatalf("CommitLineStatsInRange limited: %v", err)
	}
	if len(limited) != 1 || limited[0].Subject != "fix: drop payload" {
		t.Errorf("limit 1 = %+v, want only the newest commit", limited)
	}
}

func keys(m map[string]CommitFileChange) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
