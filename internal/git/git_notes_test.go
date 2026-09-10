package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestMergeBase_FindsCommonAncestor(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	head := commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	got, err := g.MergeBase(base, head)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if got != base {
		t.Fatalf("MergeBase(base, head) = %s, want %s", got, base)
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
