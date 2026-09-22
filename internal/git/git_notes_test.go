package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// TestPatchIDs_PerCommitAndStableAcrossRebase pins what PatchIDs adds over
// PatchID: one id per commit, each unchanged by a rebase, so a caller can tell
// which individual changes two branches share.
func TestPatchIDs_PerCommitAndStableAcrossRebase(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	a := commitFile(t, dir, "a.txt", "a\n", "add a")
	b := commitFile(t, dir, "b.txt", "b\n", "add b")

	before, err := g.PatchIDs(base, b)
	if err != nil {
		t.Fatalf("PatchIDs before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("PatchIDs returned %d ids for a 2-commit range: %v", len(before), before)
	}

	// Replay the same two commits onto a moved base: every sha changes, every
	// patch-id must not.
	runGit(t, dir, "checkout", "-b", "upstream", base)
	newBase := commitFile(t, dir, "upstream.txt", "upstream progress\n", "unrelated upstream commit")
	runGit(t, dir, "checkout", "-b", "rebased", newBase)
	runGit(t, dir, "cherry-pick", a, b)
	newHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD after cherry-pick: %v", err)
	}

	after, err := g.PatchIDs(newBase, newHead)
	if err != nil {
		t.Fatalf("PatchIDs after: %v", err)
	}
	if sorted := sortedCopy(after); !equalStrings(sorted, sortedCopy(before)) {
		t.Fatalf("per-commit patch-ids changed across a rebase:\nbefore=%v\nafter=%v", before, after)
	}
}

// TestPatchIDs_SupersetAfterAddingCommit is the shape the rework-redispatch
// recovery depends on: the shared commits keep their patch-ids when a new
// commit is added on top, so "origin's changes are all present locally" is
// answerable by set containment.
func TestPatchIDs_SupersetAfterAddingCommit(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	commitFile(t, dir, "a.txt", "a\n", "add a")
	originHead := commitFile(t, dir, "b.txt", "b\n", "add b")
	originIDs, err := g.PatchIDs(base, originHead)
	if err != nil {
		t.Fatalf("PatchIDs origin: %v", err)
	}

	localHead := commitFile(t, dir, "fix.txt", "rework fix\n", "rework: address review")
	localIDs, err := g.PatchIDs(base, localHead)
	if err != nil {
		t.Fatalf("PatchIDs local: %v", err)
	}

	if len(localIDs) != len(originIDs)+1 {
		t.Fatalf("local range has %d ids, want %d", len(localIDs), len(originIDs)+1)
	}
	remaining := map[string]int{}
	for _, id := range localIDs {
		remaining[id]++
	}
	for _, id := range originIDs {
		if remaining[id] == 0 {
			t.Fatalf("origin patch-id %s is not carried by the local range: origin=%v local=%v", id, originIDs, localIDs)
		}
		remaining[id]--
	}

	// The whole-range ids must differ, which is exactly why the per-commit
	// comparison is what decides this case.
	originRange, err := g.PatchID(base, originHead)
	if err != nil {
		t.Fatalf("PatchID origin range: %v", err)
	}
	localRange, err := g.PatchID(base, localHead)
	if err != nil {
		t.Fatalf("PatchID local range: %v", err)
	}
	if originRange == localRange {
		t.Fatal("expected whole-range patch-ids to differ once a commit is added")
	}
}

// TestPatchIDs_EmptyRange: no commits of its own is a state callers reason
// about, so it must read as an empty list rather than the error PatchID raises
// when there is no diff to hash.
func TestPatchIDs_EmptyRange(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	ids, err := g.PatchIDs(head, head)
	if err != nil {
		t.Fatalf("PatchIDs on an empty range: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("PatchIDs on an empty range = %v, want empty", ids)
	}
}

// TestFirstParentPatchIDs_MergeCommitCarriesBranchPatchID pins the property
// the refinery's landed-commit lookup depends on (gt-9t0p): a landing merge
// commit's own patch-id, measured against its first parent, is the whole
// merged branch's cumulative patch-id — the value a reviewed range's note is
// keyed to. Without that, the commit that landed an MR whose sha a rebase
// rewrote could not be found at all.
func TestFirstParentPatchIDs_MergeCommitCarriesBranchPatchID(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	mainBranch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	runGit(t, dir, "checkout", "-b", "feature", base)
	featureHead := commitFile(t, dir, "feature.txt", "feature\n", "feat: add feature")
	runGit(t, dir, "checkout", mainBranch)
	other := commitFile(t, dir, "other.txt", "other\n", "other: unrelated landing")
	runGit(t, dir, "merge", "--no-ff", "feature", "-m", "Merge feature into main")
	merge, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev merge: %v", err)
	}

	branchPatchID, err := g.PatchID(base, featureHead)
	if err != nil {
		t.Fatalf("PatchID branch range: %v", err)
	}

	pairs, err := g.FirstParentPatchIDs(base, "HEAD")
	if err != nil {
		t.Fatalf("FirstParentPatchIDs: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("FirstParentPatchIDs over a moved-base merge = %d entries (%v), want 2 (the merge and the landing before it)", len(pairs), pairs)
	}
	if pairs[0].Commit != merge {
		t.Fatalf("newest entry is %s, want the merge commit %s", pairs[0].Commit, merge)
	}
	if pairs[0].PatchID != branchPatchID {
		t.Fatalf("merge commit patch-id = %s, want the merged branch's cumulative patch-id %s", pairs[0].PatchID, branchPatchID)
	}
	if pairs[1].Commit != other {
		t.Fatalf("oldest entry is %s, want the landing before it %s", pairs[1].Commit, other)
	}
	for _, p := range pairs {
		if p.Commit == featureHead {
			t.Fatal("the branch commit is not on the first-parent chain and must not be returned")
		}
	}

	// The same patch replayed as a cherry-pick has a different sha and the
	// same patch-id, which is what makes the lookup survive a rewritten sha.
	runGit(t, dir, "checkout", "-b", "moved", base)
	movedBase := commitFile(t, dir, "upstream.txt", "upstream progress\n", "unrelated upstream commit")
	runGit(t, dir, "checkout", "-b", "picked", movedBase)
	runGit(t, dir, "cherry-pick", featureHead)
	picked, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev picked: %v", err)
	}
	if picked == featureHead {
		t.Fatal("cherry-pick should have rewritten the commit sha")
	}
	pickedPairs, err := g.FirstParentPatchIDs(movedBase, picked)
	if err != nil {
		t.Fatalf("FirstParentPatchIDs picked: %v", err)
	}
	if len(pickedPairs) != 1 || pickedPairs[0].Commit != picked || pickedPairs[0].PatchID != branchPatchID {
		t.Fatalf("cherry-picked commit = %v, want one entry {%s %s}", pickedPairs, branchPatchID, picked)
	}
}

// TestFirstParentPatchIDs_EmptyRange: like PatchIDs, "this range has no
// commits of its own" is a state callers reason about, so it reads as an
// empty list rather than the error PatchID raises with no diff to hash.
func TestFirstParentPatchIDs_EmptyRange(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	pairs, err := g.FirstParentPatchIDs(head, head)
	if err != nil {
		t.Fatalf("FirstParentPatchIDs on an empty range: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("FirstParentPatchIDs on an empty range = %v, want empty", pairs)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
