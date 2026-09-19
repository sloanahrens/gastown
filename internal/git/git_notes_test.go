package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commitFile writes content to path in dir, stages it, and commits with message.
func commitFile(t *testing.T, dir, path, content, message string) string {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	g := NewGit(dir)
	if err := g.Add(path); err != nil {
		t.Fatalf("add %s: %v", path, err)
	}
	if err := g.Commit(message); err != nil {
		t.Fatalf("commit %s: %v", message, err)
	}
	rev, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	return rev
}

func TestPatchID_StableAcrossNoOpRebase(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	head := commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	before, err := g.PatchID(base, head)
	if err != nil {
		t.Fatalf("PatchID before: %v", err)
	}
	if before == "" {
		t.Fatal("PatchID returned empty string")
	}

	// Simulate a real rebase: upstream moves forward with an unrelated commit,
	// sibling to head off the same original base (giving the new base a
	// different sha from the old one), then the feature commit is replayed
	// on top of it. The diff content is unchanged, so the patch-id must
	// match even though both the base and head shas differ from before.
	cmd := exec.Command("git", "checkout", "-b", "upstream", base)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout upstream: %v\n%s", err, out)
	}
	newBase := commitFile(t, dir, "unrelated.txt", "upstream progress\n", "unrelated upstream commit")

	cmd = exec.Command("git", "checkout", "-b", "rebased", newBase)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout rebased: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "cherry-pick", head)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cherry-pick: %v\n%s", err, out)
	}
	newHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD after cherry-pick: %v", err)
	}
	if newHead == head {
		t.Fatal("expected cherry-pick to produce a new commit sha")
	}

	after, err := g.PatchID(newBase, newHead)
	if err != nil {
		t.Fatalf("PatchID after: %v", err)
	}
	if after != before {
		t.Fatalf("PatchID changed across no-op rebase: before=%s after=%s", before, after)
	}
}

func TestPatchID_DiffersAfterContentEdit(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	head := commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	original, err := g.PatchID(base, head)
	if err != nil {
		t.Fatalf("PatchID original: %v", err)
	}

	edited := commitFile(t, dir, "feature.txt", "hello world\n", "edit feature")
	changed, err := g.PatchID(head, edited)
	if err != nil {
		t.Fatalf("PatchID changed: %v", err)
	}
	if changed == original {
		t.Fatalf("expected PatchID to differ after content edit, got same value %s", changed)
	}
}

func TestNotesAddShow_RoundTrip(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	if err := g.NotesAdd("om", head, `{"verdict":"approve"}`); err != nil {
		t.Fatalf("NotesAdd: %v", err)
	}

	got, err := g.NotesShow("om", head)
	if err != nil {
		t.Fatalf("NotesShow: %v", err)
	}
	if got != `{"verdict":"approve"}` {
		t.Fatalf("NotesShow round-trip mismatch: got %q", got)
	}
}

func TestNotesShow_NoNoteReturnsErrNoNote(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	_, err = g.NotesShow("om", head)
	if !errors.Is(err, ErrNoNote) {
		t.Fatalf("expected ErrNoNote, got %v", err)
	}
}

// currentBranchName reports the branch initTestRepo left checked out, so
// these tests do not depend on git's configured default branch name.
func currentBranchName(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse --abbrev-ref HEAD: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestNotesList_ReturnsNotesOnUnreachableCommits(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	root, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	branch := currentBranchName(t, dir)

	// A note keyed to a commit nothing references any more: the shape a
	// discarded rehearsal head leaves behind, and the reason the backfill
	// reads the notes ref instead of walking history.
	runGit(t, dir, "checkout", "-q", "-b", "rehearsal")
	orphan := commitFile(t, dir, "feature.txt", "hello\n", "rehearsal head")
	runGit(t, dir, "checkout", "-q", branch)
	runGit(t, dir, "branch", "-D", "rehearsal")

	if err := g.NotesAdd("om", orphan, "orphan note"); err != nil {
		t.Fatalf("NotesAdd orphan: %v", err)
	}
	if err := g.NotesAdd("om", root, "root note"); err != nil {
		t.Fatalf("NotesAdd root: %v", err)
	}

	entries, err := g.NotesList("om")
	if err != nil {
		t.Fatalf("NotesList: %v", err)
	}
	got := make(map[string]string, len(entries))
	for _, e := range entries {
		got[e.Annotated] = e.Content
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 notes, got %d: %+v", len(got), entries)
	}
	if got[orphan] != "orphan note" {
		t.Fatalf("unreachable commit's note missing: %+v", got)
	}
	if got[root] != "root note" {
		t.Fatalf("reachable commit's note missing: %+v", got)
	}
}

func TestNotesList_AbsentRefIsEmpty(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)

	entries, err := g.NotesList("om")
	if err != nil {
		t.Fatalf("NotesList on an absent ref: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no notes, got %+v", entries)
	}
}

func TestParents_OfRootAndMerge(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	root, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	branch := currentBranchName(t, dir)

	parents, err := g.Parents(root)
	if err != nil {
		t.Fatalf("Parents(root): %v", err)
	}
	if len(parents) != 0 {
		t.Fatalf("root commit has parents %v", parents)
	}

	runGit(t, dir, "checkout", "-q", "-b", "feature")
	featureHead := commitFile(t, dir, "feature.txt", "hello\n", "add feature")
	runGit(t, dir, "checkout", "-q", branch)
	runGit(t, dir, "merge", "--no-ff", "-m", "merge feature", "feature")
	merged, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev merge: %v", err)
	}

	parents, err = g.Parents(merged)
	if err != nil {
		t.Fatalf("Parents(merge): %v", err)
	}
	if len(parents) != 2 || parents[0] != root || parents[1] != featureHead {
		t.Fatalf("merge parents = %v, want [%s %s]", parents, root, featureHead)
	}
}

func TestNotesCopy(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	from, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	to := commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	if err := g.NotesAdd("om", from, "note content"); err != nil {
		t.Fatalf("NotesAdd: %v", err)
	}
	if err := g.NotesCopy("om", from, to); err != nil {
		t.Fatalf("NotesCopy: %v", err)
	}

	fromNote, err := g.NotesShow("om", from)
	if err != nil {
		t.Fatalf("NotesShow(from): %v", err)
	}
	toNote, err := g.NotesShow("om", to)
	if err != nil {
		t.Fatalf("NotesShow(to): %v", err)
	}
	if fromNote != toNote {
		t.Fatalf("NotesShow(to) != NotesShow(from): %q vs %q", toNote, fromNote)
	}
}
