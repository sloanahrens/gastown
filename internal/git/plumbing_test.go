package git

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The canned outputs below are real git's, captured from a scratch
// repository with the exact argv each method sends.

const (
	readmeV1 = "8ae056963b8b4664c9059e30bc8b834151e03950"
	readmeV2 = "e1b06750e5d49f78e4201f2c08aadc58e7912192"
	alphaV1  = "4a58007052a65fbc2fc3f910f2855f45a4058e74"
	betaV1   = "65b2df87f7df3aeedef04be96703e55ac19c2cfb"
)

// rawLogTwoCommits is CommitFileChanges' git log for a commit that edits
// README.md and adds alpha.txt and beta.txt, on top of a root commit.
const rawLogTwoCommits = "C e7f26ec0b89deda338c0b84786b888b2057a0c0a 7d964c86c090c8ad6e41f72a7009f99d8b78c1cd\x00" +
	"\n:100644 100644 " + readmeV1 + " " + readmeV2 + " M\x00README.md\x00" +
	":000000 100644 0000000000000000000000000000000000000000 " + alphaV1 + " A\x00alpha.txt\x00" +
	":000000 100644 0000000000000000000000000000000000000000 " + betaV1 + " A\x00beta.txt\x00" +
	"C 7d964c86c090c8ad6e41f72a7009f99d8b78c1cd \x00" +
	"\n:000000 100644 0000000000000000000000000000000000000000 " + readmeV1 + " A\x00README.md\x00"

// TestCommitFileChanges_ReportsEveryFileOfEveryCommit pins the -z parsing of
// git log --raw: under -z the blank line between a commit's header and its
// raw block arrives as a leading "\n" on the FIRST meta token, and a parser
// that classifies tokens by their raw prefix drops the first file of every
// commit.
//
// It also pins the parentless rule: a root commit, or a shallow clone's graft
// boundary, prints its whole tree as creations, which describe content the
// commit never added (gt-zeuip), so it contributes nothing.
func TestCommitFileChanges_ReportsEveryFileOfEveryCommit(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"log --raw --no-abbrev --no-renames -z --format=C %H %P HEAD -n 10": ok(rawLogTwoCommits)})
	changes, err := newTestGit(t, s).CommitFileChanges("HEAD", 10)
	if err != nil {
		t.Fatalf("CommitFileChanges: %v", err)
	}
	const head = "e7f26ec0b89deda338c0b84786b888b2057a0c0a"
	want := []CommitFileChange{
		{Commit: head, Path: "README.md", OldBlob: readmeV1, NewBlob: readmeV2},
		{Commit: head, Path: "alpha.txt", NewBlob: alphaV1},
		{Commit: head, Path: "beta.txt", NewBlob: betaV1},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("CommitFileChanges = %+v, want %+v", changes, want)
	}
}

// Creation and deletion use "" for the absent side, never git's all-zero
// null object, which is not a blob any tree reports.
func TestCommitFileChanges_CreationAndDeletionUseEmptyNotNullBlob(t *testing.T) {
	t.Parallel()
	const out = "C c18603fd84fb3c7fdfef4d88fa2c0e2ebf734a32 46e5943efdbff29a940d87331d3458ada16f054e\x00" +
		"\n:100644 000000 ed041f1eab085badf8dd8d6f7beed690968f8f86 0000000000000000000000000000000000000000 D\x00gone.txt\x00"
	s := newScripted(map[string]reply{"log --raw --no-abbrev --no-renames -z --format=C %H %P HEAD -n 1": ok(out)})
	changes, err := newTestGit(t, s).CommitFileChanges("HEAD", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].NewBlob != "" || changes[0].OldBlob != "ed041f1eab085badf8dd8d6f7beed690968f8f86" {
		t.Fatalf("CommitFileChanges = %+v", changes)
	}
}

// A path that looks like a header or a meta token is still a path: it is only
// ever read from the position right after a meta token.
func TestParseRawLogChangesReadsPathsPositionally(t *testing.T) {
	t.Parallel()
	out := "C aaa bbb\x00\n:100644 100644 " + readmeV1 + " " + readmeV2 + " M\x00C trick\x00" +
		":100644 100644 " + readmeV1 + " " + readmeV2 + " M\x00:colon\x00" +
		":100644 100755 " + readmeV1 + " " + readmeV1 + " M\x00modeonly.sh\x00"
	got := parseRawLogChanges(out)
	if len(got) != 2 || got[0].Path != "C trick" || got[1].Path != ":colon" {
		t.Fatalf("parseRawLogChanges = %+v; want the two content changes, mode-only skipped", got)
	}
}

// TestBlobDiffLines: which lines one blob adds and removes relative to
// another. A removed line starting with "--" is body, not the file header.
func TestBlobDiffLines(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff --unified=0 --no-color " + readmeV1 + " " + readmeV2: ok("diff --git a/" + readmeV1 + " b/" + readmeV2 + "\n" +
			"index 8ae0569..e1b0675 100644\n--- a/" + readmeV1 + "\n+++ b/" + readmeV2 + "\n" +
			"@@ -1 +1,2 @@\n-# Test\n+# Changed\n+more\n@@ -5 +5,0 @@\n--- old rule\n"),
	})
	g := newTestGit(t, s)
	added, removed, err := g.BlobDiffLines(readmeV1, readmeV2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, map[string]int{"# Changed": 1, "more": 1}) || !reflect.DeepEqual(removed, map[string]int{"# Test": 1, "-- old rule": 1}) {
		t.Fatalf("added = %v, removed = %v", added, removed)
	}
	// An absent side is not a whole-file rewrite, and asks git nothing.
	noneAdded, noneRemoved, err := g.BlobDiffLines("", readmeV2)
	if err != nil || len(noneAdded) != 0 || len(noneRemoved) != 0 {
		t.Fatalf("absent side = %v %v %v, want nothing", noneAdded, noneRemoved, err)
	}
	if len(s.sent()) != 1 {
		t.Fatalf("calls = %q", s.sent())
	}
}

func TestTreeFileBlobs(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"ls-tree -r -z --full-tree HEAD": ok(
		"100644 blob " + readmeV2 + "\tREADME.md\x00" +
			"160000 commit 1111111111111111111111111111111111111111\tlibs/sub\x00" +
			"100644 blob " + alphaV1 + "\tdir/with space.txt\x00")})
	blobs, err := newTestGit(t, s).TreeFileBlobs("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"README.md": readmeV2, "dir/with space.txt": alphaV1}; !reflect.DeepEqual(blobs, want) {
		t.Fatalf("TreeFileBlobs = %v, want %v (gitlinks skipped)", blobs, want)
	}
}

func TestTreesIdentical(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"rev-parse main^{tree}":  ok("aaaa\n"),
		"rev-parse same^{tree}":  ok("aaaa\n"),
		"rev-parse other^{tree}": ok("bbbb\n"),
		"rev-parse gone^{tree}":  fail(128, "fatal: ambiguous argument 'gone^{tree}': unknown revision or path not in the working tree.\n"),
	})
	g := newTestGit(t, s)
	if same, err := g.TreesIdentical("main", "same"); err != nil || !same {
		t.Errorf("TreesIdentical(main, same) = %v, %v", same, err)
	}
	if same, err := g.TreesIdentical("main", "other"); err != nil || same {
		t.Errorf("TreesIdentical(main, other) = %v, %v", same, err)
	}
	if _, err := g.TreesIdentical("main", "gone"); err == nil || !strings.Contains(err.Error(), "resolve tree of gone") {
		t.Errorf("TreesIdentical(main, gone) err = %v", err)
	}
}

// TestCommitLineStatsInRange pins the two shapes git log --numstat emits: a
// commit with changes prints a blank line and its counts, an empty commit
// prints nothing, so the next hash follows its subject directly. Binary files
// ("-") count nothing.
func TestCommitLineStatsInRange(t *testing.T) {
	t.Parallel()
	const out = "668ddf17d5b8826c7fc2da715924e1c4d5339a7e\nfix: drop payload\n\n0\t3\tpayload.txt\n-\t-\tlogo.png\n" +
		"f96911d8d9b1aa717ba711a6f9e616f2bf3eedb9\nchore: no-op\n" +
		"99b271f01e302377df51c5ad66a00cc816ad41b6\nfeat: add payload\n\n3\t0\tpayload.txt\n"
	s := newScripted(map[string]reply{"log --no-merges --numstat --format=%H%n%s -n 10 main..work": ok(out)})
	stats, err := newTestGit(t, s).CommitLineStatsInRange("main..work", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []CommitLineStats{
		{Commit: "668ddf17d5b8826c7fc2da715924e1c4d5339a7e", Subject: "fix: drop payload", Removed: 3},
		{Commit: "f96911d8d9b1aa717ba711a6f9e616f2bf3eedb9", Subject: "chore: no-op"},
		{Commit: "99b271f01e302377df51c5ad66a00cc816ad41b6", Subject: "feat: add payload", Added: 3},
	}
	if !reflect.DeepEqual(stats, want) {
		t.Fatalf("CommitLineStatsInRange = %+v, want %+v", stats, want)
	}
}

func TestCommitMessagesSplitsRecords(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"log --format=%H%x1f%B%x1e base..head": ok(
		"bbb\x1ffeat: two\n\nbody line\n\x1e\naaa\x1ffix: one\n\x1e\n")})
	msgs, err := newTestGit(t, s).CommitMessages("base", "head")
	if err != nil {
		t.Fatal(err)
	}
	want := []CommitMessage{{SHA: "bbb", Message: "feat: two\n\nbody line"}, {SHA: "aaa", Message: "fix: one"}}
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("CommitMessages = %q, want %q", msgs, want)
	}
}

func TestParents(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"rev-list --parents -n 1 root":  ok("aaa\n"),
		"rev-list --parents -n 1 merge": ok("mmm ppp qqq\n"),
		"rev-list --parents -n 1 none":  ok(""),
	})
	g := newTestGit(t, s)
	if p, err := g.Parents("root"); err != nil || len(p) != 0 {
		t.Errorf("Parents(root) = %q, %v", p, err)
	}
	if p, err := g.Parents("merge"); err != nil || !reflect.DeepEqual(p, []string{"ppp", "qqq"}) {
		t.Errorf("Parents(merge) = %q, %v", p, err)
	}
	if _, err := g.Parents("none"); err == nil {
		t.Error("Parents of nothing must fail")
	}
}

// FirstParentContains is membership of the first-parent chain, not ancestry
// (gt-ljn8): the chain is what git prints; the test pins the exact-line match.
func TestFirstParentContains(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"rev-list --first-parent main": ok("mmm\nbase\n")})
	g := newTestGit(t, s)
	if in, err := g.FirstParentContains("base", "main"); err != nil || !in {
		t.Errorf("FirstParentContains(base) = %v, %v", in, err)
	}
	if in, err := g.FirstParentContains("bas", "main"); err != nil || in {
		t.Errorf("FirstParentContains(prefix) = %v, %v; want false", in, err)
	}
}

func TestNotesShowNoNoteIsErrNoNote(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"notes --ref om show abc": fail(1, "error: no note found for object abc.\n"),
		"notes --ref om show def": fail(128, "fatal: failed to resolve 'def' as a valid ref.\n"),
	})
	g := newTestGit(t, s)
	if _, err := g.NotesShow("om", "abc"); !errors.Is(err, ErrNoNote) {
		t.Fatalf("NotesShow = %v, want ErrNoNote", err)
	}
	if _, err := g.NotesShow("om", "def"); err == nil || errors.Is(err, ErrNoNote) {
		t.Fatalf("NotesShow of a bad ref = %v, want a real error", err)
	}
}

// NotesList reads every note the ref holds, keyed by annotated object, and
// skips one removed between the list and the show.
func TestNotesList(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"notes --ref om list":     ok("n1 aaa\nn2 bbb\nn3 ccc\n"),
		"notes --ref om show aaa": ok("root note\n"),
		"notes --ref om show bbb": fail(1, "error: no note found for object bbb.\n"),
		"notes --ref om show ccc": ok("orphan note\n"),
	})
	entries, err := newTestGit(t, s).NotesList("om")
	if err != nil {
		t.Fatal(err)
	}
	want := []NoteEntry{{Annotated: "aaa", Content: "root note"}, {Annotated: "ccc", Content: "orphan note"}}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("NotesList = %+v, want %+v", entries, want)
	}
}

// An empty range has no commits of its own: PatchIDs and FirstParentPatchIDs
// read it as an empty list, without asking patch-id to hash nothing.
func TestPatchIDsEmptyRange(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"log --no-merges -p --no-color h..h":    ok(""),
		"log --first-parent -p --no-color h..h": ok(""),
	})
	g := newTestGit(t, s)
	if ids, err := g.PatchIDs("h", "h"); err != nil || len(ids) != 0 {
		t.Errorf("PatchIDs = %v, %v", ids, err)
	}
	if pairs, err := g.FirstParentPatchIDs("h", "h"); err != nil || len(pairs) != 0 {
		t.Errorf("FirstParentPatchIDs = %v, %v", pairs, err)
	}
	s.noUnscripted(t)
}

// The log is piped into patch-id --stable as stdin, and each output line
// pairs a patch-id with its commit.
func TestFirstParentPatchIDsPipesLogIntoPatchID(t *testing.T) {
	t.Parallel()
	const log = "commit bbb\n\ndiff --git a/x b/x\n"
	s := newScripted(map[string]reply{
		"log --first-parent -p --no-color a..b": ok(log),
		"patch-id --stable":                     ok("p2 bbb\np1 aaa\n"),
	})
	pairs, err := newTestGit(t, s).FirstParentPatchIDs("a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if want := []PatchIDCommit{{PatchID: "p2", Commit: "bbb"}, {PatchID: "p1", Commit: "aaa"}}; !reflect.DeepEqual(pairs, want) {
		t.Fatalf("FirstParentPatchIDs = %+v", pairs)
	}
	if c, _ := s.sentCall("patch-id --stable"); c.stdin != strings.TrimSpace(log) {
		t.Fatalf("patch-id stdin = %q", c.stdin)
	}
}

func TestPatchIDNoOutputIsAnError(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"diff a..b":         ok(""),
		"patch-id --stable": ok(""),
	})
	if _, err := newTestGit(t, s).PatchID("a", "b"); err == nil || !strings.Contains(err.Error(), "no output for range a..b") {
		t.Fatalf("PatchID = %v", err)
	}
}

func TestCheckBranchContamination(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"rev-list --count HEAD..main": ok("5\n"),
		"rev-list --count main..HEAD": ok("1\n"),
	})
	c, err := newTestGit(t, s).CheckBranchContamination("main")
	if err != nil || c.Behind != 5 || c.Ahead != 1 {
		t.Fatalf("CheckBranchContamination = %+v, %v; want behind 5 ahead 1", c, err)
	}
}

// CommitAuthorSubject reads one commit's author and subject in a single call;
// the NUL keeps a subject containing spaces from bleeding into the author.
func TestCommitAuthorSubject(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"log -1 --format=%an%x00%s cccc": ok("Alice\x00push straight to main\n"),
	})
	g := newTestGit(t, s)

	author, subject, err := g.CommitAuthorSubject("cccc")
	if err != nil {
		t.Fatal(err)
	}
	if author != "Alice" || subject != "push straight to main" {
		t.Errorf("author, subject = %q, %q, want the two fields split on the NUL", author, subject)
	}
	s.noUnscripted(t)
}

// A commit the repo does not have is an error: the caller decides what to say
// about a tip it cannot describe.
func TestCommitAuthorSubjectUnknownCommit(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"log -1 --format=%an%x00%s dead": fail(128, "fatal: bad revision 'dead'"),
	})
	g := newTestGit(t, s)

	if _, _, err := g.CommitAuthorSubject("dead"); err == nil {
		t.Fatal("want an error for a commit the repo does not have")
	}
	s.noUnscripted(t)
}
