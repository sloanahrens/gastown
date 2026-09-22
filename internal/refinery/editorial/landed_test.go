package editorial

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/plugin"
)

// landedFixture builds a repo whose history can produce each landing shape
// ResolveLandedRange must reason about: a single-parent landing, a clean merge
// onto an advanced target, a clean merge whose target moved a line inside the
// branch's hunk context, a merge whose tree carries a file no parent made, and
// a branch that deletes a file the base held.
type landedFixture struct {
	repoDir   string
	g         *git.Git
	base      string // the common merge point every shape descends from
	branchTip string // the branch's own head: edits shared.txt, adds feature.txt, deletes doomed.txt
	ffLanded  string // single-parent landing of the branch
	target    string // a target commit ahead of base (adds target.txt only)
	merge     string // clean merge of the branch onto that target
	ctxMerge  string // clean merge of the branch onto a target that edits shared.txt line 13
	evilMerge string // merge whose tree adds a file neither parent has
}

func newLandedFixture(t *testing.T) *landedFixture {
	t.Helper()
	dir := initTestRepo(t)
	g := git.NewGit(dir)

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// The merge point: the file the branch and the target each edit a
	// different line of, plus a file only the branch removes.
	write("shared.txt", sharedLines("", ""))
	write("doomed.txt", "doomed\n")
	run("add", ".")
	run("commit", "-m", "base")
	base := mustRev(t, g, "HEAD")

	// Fake origin/main at base: the remote tip a landing is measured against.
	run("update-ref", "refs/remotes/origin/main", base)

	// A real origin remote so the post-approve note push has somewhere to go.
	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare origin: %v\n%s", err, out)
	}
	if _, err := g.AddRemote("origin", bareDir); err != nil {
		t.Fatalf("add remote origin: %v", err)
	}

	// Branch line: edit shared.txt line 10, add a file, delete another.
	run("checkout", "-b", "branch-line")
	write("shared.txt", sharedLines("branch-10", ""))
	write("feature.txt", "feature\n")
	run("rm", "doomed.txt")
	run("add", ".")
	run("commit", "-m", "feature")
	branchTip := mustRev(t, g, "HEAD")

	// Single-parent landing: the branch line reaches origin/main directly.
	ffLanded := branchTip

	// Clean merge: advance the target line with an unrelated file, then merge
	// the branch in. The merge's first parent is a target commit AHEAD of the
	// branch's base, which is the shape that produced phantom findings when
	// the base was taken to be ^1.
	run("checkout", "-b", "target-line")
	run("reset", "--hard", base)
	write("target.txt", "target\n")
	run("add", ".")
	run("commit", "-m", "target work")
	target := mustRev(t, g, "HEAD")
	run("merge", branchTip, "-m", "merge feature into target")
	merge := mustRev(t, g, "HEAD")

	// Clean merge whose target moved a line inside the branch's hunk context:
	// line 13 sits in the trailing context of the branch's line-10 hunk, so
	// the two merge without conflict while patch-id(base..branch) and
	// patch-id(target..merge) still differ.
	run("checkout", "-b", "ctx-line")
	run("reset", "--hard", base)
	write("shared.txt", sharedLines("", "target-13"))
	write("ctx.txt", "ctx\n")
	run("add", ".")
	run("commit", "-m", "target context work")
	run("merge", branchTip, "-m", "merge feature into context target")
	ctxMerge := mustRev(t, g, "HEAD")

	// Evil merge: a clean merge, then amended to carry a file neither parent
	// has. The tree is no longer the merge of the parents, so no range from
	// the graph describes what landed.
	run("checkout", "-b", "evil-line")
	run("reset", "--hard", base)
	write("evilonly.txt", "evil only\n")
	run("add", ".")
	run("commit", "-m", "evil-side work")
	evilTip := mustRev(t, g, "HEAD")
	run("checkout", "target-line")
	run("merge", evilTip, "-m", "merge evil-side work")
	write("sneaky.txt", "sneaky\n")
	run("add", ".")
	run("commit", "--amend", "--no-edit")
	evilMerge := mustRev(t, g, "HEAD")

	// An unpushed commit, for the not-landed refusal: origin/main never names
	// it and cannot contain it.
	run("checkout", "-b", "unpushed-branch")
	run("commit", "--allow-empty", "-m", "unpushed")
	run("checkout", "target-line")

	return &landedFixture{
		repoDir:   dir,
		g:         g,
		base:      base,
		branchTip: branchTip,
		ffLanded:  ffLanded,
		target:    target,
		merge:     merge,
		ctxMerge:  ctxMerge,
		evilMerge: evilMerge,
	}
}

// land points origin/main at commit, making that shape the one that landed.
// The shapes are built from one base but do not all descend from each other,
// so which shape counts as landed is per-test rather than a property of the
// fixture.
func (f *landedFixture) land(t *testing.T, commit string) {
	t.Helper()
	cmd := exec.Command("git", "update-ref", "refs/remotes/origin/main", commit)
	cmd.Dir = f.repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref refs/remotes/origin/main %s: %v\n%s", commit, err, out)
	}
}

// sharedLines renders the 20-line file both sides edit a different line of.
// The two edits are far enough apart to merge without conflict and close
// enough that each lands in the other's hunk context.
func sharedLines(edit10, edit13 string) string {
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		line := fmt.Sprintf("line-%02d", i)
		switch {
		case i == 10 && edit10 != "":
			line = edit10
		case i == 13 && edit13 != "":
			line = edit13
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func mustRev(t *testing.T, g *git.Git, rev string) string {
	t.Helper()
	s, err := g.Rev(rev)
	if err != nil {
		t.Fatalf("rev %s: %v", rev, err)
	}
	return s
}

// unpushedTip returns the tip of the fixture's never-pushed branch.
func (f *landedFixture) unpushedTip(t *testing.T) string {
	t.Helper()
	return mustRev(t, f.g, "unpushed-branch")
}

func TestResolveLandedRange_FastForward(t *testing.T) {
	f := newLandedFixture(t)
	f.land(t, f.ffLanded)
	r, err := ResolveLandedRange(f.g, f.ffLanded, "main")
	if err != nil {
		t.Fatalf("ResolveLandedRange: %v", err)
	}
	if r.Commit != f.ffLanded {
		t.Errorf("Commit = %s, want %s", r.Commit, f.ffLanded)
	}
	if r.Parent != f.base || r.Base != f.base || r.Head != f.ffLanded {
		t.Errorf("Parent/Base/Head = %s/%s/%s, want %s/%s/%s",
			r.Parent, r.Base, r.Head, f.base, f.base, f.ffLanded)
	}
	wantID, err := f.g.PatchID(f.base, f.ffLanded)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	if r.PatchID != wantID {
		t.Errorf("PatchID = %q, want %q", r.PatchID, wantID)
	}
}

func TestResolveLandedRange_Merge(t *testing.T) {
	f := newLandedFixture(t)
	f.land(t, f.merge)
	r, err := ResolveLandedRange(f.g, f.merge, "main")
	if err != nil {
		t.Fatalf("ResolveLandedRange: %v", err)
	}
	if r.Commit != f.merge {
		t.Errorf("Commit = %s, want merge %s", r.Commit, f.merge)
	}
	if r.Parent != f.target {
		t.Errorf("Parent = %s, want first parent (target) %s", r.Parent, f.target)
	}
	// The base is the merge point of the two parents — base here, NOT the
	// first parent (which is ahead of it). The head is the branch that
	// arrived: the side whose change is under review.
	if r.Base != f.base {
		t.Errorf("Base = %s, want merge point %s (never the first parent %s)", r.Base, f.base, f.target)
	}
	if r.Head != f.branchTip {
		t.Errorf("Head = %s, want branch tip %s", r.Head, f.branchTip)
	}
	if r.Head == r.Base {
		t.Error("Head == Base: the range would be empty")
	}
	// The note is keyed on patch-id(commit^1..commit), the value the coverage
	// check recomputes from the landed commit.
	if want := f.landedPatchID(t, f.merge); r.PatchID != want {
		t.Errorf("PatchID = %q, want the landed diff's %q", r.PatchID, want)
	}
}

// TestResolveLandedRange_ContextChangedMergeAccepted pins the case that made a
// patch-id comparison the wrong test. The target moved line 13, which is
// context of the branch's line-10 hunk rather than a change of its own, so the
// merge is clean while patch-id(base..branch) and patch-id(target..merge)
// differ. Requiring those two ids to be equal refused this landing outright.
func TestResolveLandedRange_ContextChangedMergeAccepted(t *testing.T) {
	f := newLandedFixture(t)
	f.land(t, f.ctxMerge)
	branchID, err := f.g.PatchID(f.base, f.branchTip)
	if err != nil {
		t.Fatalf("PatchID(base, branch): %v", err)
	}
	landedID := f.landedPatchID(t, f.ctxMerge)
	if branchID == landedID {
		t.Fatal("fixture does not reproduce the case: the two patch-ids are equal, so a patch-id comparison would have accepted this landing too")
	}

	r, err := ResolveLandedRange(f.g, f.ctxMerge, "main")
	if err != nil {
		t.Fatalf("ResolveLandedRange refused a conflict-free merge: %v", err)
	}
	if r.Base != f.base {
		t.Errorf("Base = %s, want the merge point %s", r.Base, f.base)
	}
	if r.Head != f.branchTip {
		t.Errorf("Head = %s, want the branch tip %s", r.Head, f.branchTip)
	}
	if r.PatchID != landedID {
		t.Errorf("PatchID = %q, want the landed diff's %q", r.PatchID, landedID)
	}
}

func TestResolveLandedRange_EvilMergeRefused(t *testing.T) {
	f := newLandedFixture(t)
	f.land(t, f.evilMerge)
	_, err := ResolveLandedRange(f.g, f.evilMerge, "main")
	if err == nil {
		t.Fatal("ResolveLandedRange: want a refusal for a merge whose tree carries a file no parent made")
	}
	// The refusal must not diagnose this as a missing deletion: the evil merge
	// adds a file, it drops nothing, so a deletion-shaped explanation would be
	// false.
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("refusal = %q, want it to say 'refusing'", err.Error())
	}
	if !strings.Contains(err.Error(), "is not the merge of its parents") {
		t.Errorf("refusal = %q, want it to say the landing is not the merge of its parents", err.Error())
	}
}

func TestResolveLandedRange_NotLandedRefused(t *testing.T) {
	f := newLandedFixture(t)
	f.land(t, f.merge)
	unpushed := f.unpushedTip(t)
	_, err := ResolveLandedRange(f.g, unpushed, "main")
	if err == nil {
		t.Fatal("ResolveLandedRange: want a refusal for a commit origin/main cannot contain")
	}
	// This must be the reachability refusal, not a later one: an unpushed
	// commit is also a perfectly ordinary commit, so any other refusal would
	// mean the reachability check did not run.
	if !strings.Contains(err.Error(), "not reachable from") {
		t.Errorf("refusal = %q, want the not-reachable refusal", err.Error())
	}
}

func TestResolveLandedRange_RootRefused(t *testing.T) {
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	// A root commit, made reachable from origin/main so that the root check —
	// not the reachability check — is what fires.
	root := mustRev(t, g, "HEAD")
	cmd := exec.Command("git", "update-ref", "refs/remotes/origin/main", root)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	_, err := ResolveLandedRange(g, root, "main")
	if err == nil {
		t.Fatal("ResolveLandedRange: want a refusal for a root commit")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("refusal = %q, want it to name the root commit", err.Error())
	}
}

// TestResolveLandedRange_UnknownShaRefused covers a sha that resolves to
// nothing at all — the case a refusal recognizer keyed on message text would
// have missed, letting a zero-valued range reach the gate.
func TestResolveLandedRange_UnknownShaRefused(t *testing.T) {
	f := newLandedFixture(t)
	f.land(t, f.merge)
	if _, err := ResolveLandedRange(f.g, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "main"); err == nil {
		t.Fatal("ResolveLandedRange: want a refusal for an unresolvable sha")
	}
}

// TestTryLandedDeletions_ReportsEachDirection covers the refusal's path
// listing directly: it is a pure comparison of the two trees' deletions, so
// feeding it a range whose deletions differ from the landing's is enough to
// pin both directions without staging a merge that resolves a conflict.
func TestTryLandedDeletions_ReportsEachDirection(t *testing.T) {
	f := newLandedFixture(t)
	f.land(t, f.merge)
	// The branch removes doomed.txt; a landing measured against the target
	// commit (which still holds it) removes nothing.
	d, ok := tryLandedDeletions(f.g, LandedRange{
		Base:   f.base,
		Head:   f.branchTip,
		Parent: f.target,
		Commit: f.target,
	})
	if !ok {
		t.Fatal("tryLandedDeletions: want a computed comparison")
	}
	if len(d.rangeOnly) != 1 || d.rangeOnly[0] != "doomed.txt" {
		t.Errorf("rangeOnly = %v, want [doomed.txt]", d.rangeOnly)
	}
	if len(d.landedOnly) != 0 {
		t.Errorf("landedOnly = %v, want empty (the landing removes nothing)", d.landedOnly)
	}

	// And the mirror: a landing that removes a path the branch's own diff
	// keeps.
	rev, ok := tryLandedDeletions(f.g, LandedRange{
		Base:   f.base,
		Head:   f.target,
		Parent: f.base,
		Commit: f.branchTip,
	})
	if !ok {
		t.Fatal("tryLandedDeletions: want a computed comparison")
	}
	if len(rev.landedOnly) != 1 || rev.landedOnly[0] != "doomed.txt" {
		t.Errorf("landedOnly = %v, want [doomed.txt]", rev.landedOnly)
	}
	if len(rev.rangeOnly) != 0 {
		t.Errorf("rangeOnly = %v, want empty", rev.rangeOnly)
	}
}

func (f *landedFixture) landedPatchID(t *testing.T, commit string) string {
	t.Helper()
	parents, err := f.g.Parents(commit)
	if err != nil {
		t.Fatalf("parents: %v", err)
	}
	id, err := f.g.PatchID(parents[0], commit)
	if err != nil {
		t.Fatalf("patch-id of landed diff: %v", err)
	}
	return id
}

// TestRun_LandedReview_StampNoteOnLandedCommit pins the landed path's three
// load-bearing differences from the MR path: the note is stamped on the landed
// commit (not a rehearsal head or branch tip), it is keyed on the landed
// diff's patch-id (the value the coverage check recomputes), and no
// editorial_reviewed_head is written to any MR bead.
func TestRun_LandedReview_StampNoteOnLandedCommit(t *testing.T) {
	fakeBDForReview(t)
	f := newLandedFixture(t)
	f.land(t, f.merge)
	landed, err := ResolveLandedRange(f.g, f.merge, "main")
	if err != nil {
		t.Fatalf("ResolveLandedRange: %v", err)
	}

	rigDir := t.TempDir()
	if err := writeTestManifest(t, rigDir); err != nil {
		t.Fatal(err)
	}
	store := newReviewStore() // no MR bead: a landed review has none
	deps := Deps{
		Git:      f.g,
		Beads:    beads.NewWithStore(f.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.87, Verdict: "approve"})
			return "", 0, nil
		},
	}
	req := ReviewRequest{
		RigDir:  rigDir,
		RepoDir: f.repoDir,
		MRID:    "gt-mr-landed",
		Rig:     "gastown",
		Target:  "main",
		Landed:  &landed,
		Attempt: 1,
		Config:  config.EditorialConfig{Required: true},
	}

	result := Run(context.Background(), req, deps)
	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
	if result.Note == nil {
		t.Fatal("expected a note")
	}
	if !result.Note.RetroReview {
		t.Error("RetroReview = false, want true on a landed review")
	}
	// The note must sit on the landed commit, so the coverage check finds it.
	if result.Note.HeadSHA != f.merge {
		t.Errorf("Note.HeadSHA = %s, want the landed commit %s", result.Note.HeadSHA, f.merge)
	}
	if result.Note.ReviewHead != f.branchTip {
		t.Errorf("Note.ReviewHead = %q, want the reviewed head %s", result.Note.ReviewHead, f.branchTip)
	}
	// The key must be the landed diff's patch-id. The coverage check computes
	// patch-id(commit^1..commit) and compares; anything else — in particular
	// patch-id of the branch's own range — leaves the note unmatchable and the
	// commit still uncovered.
	if want := f.landedPatchID(t, f.merge); result.Note.PatchID != want {
		t.Errorf("Note.PatchID = %q, want the landed diff's %q", result.Note.PatchID, want)
	}

	gotNote, err := ReadNote(f.g, f.merge)
	if err != nil {
		t.Fatalf("ReadNote on landed commit: %v", err)
	}
	// RetroReview must be recorded; Backfill must NOT be — this is a
	// first-hand verdict, not a re-keyed copy.
	if !gotNote.RetroReview {
		t.Error("git note RetroReview = false, want true")
	}
	if gotNote.Backfill {
		t.Error("git note Backfill = true, want false (first-hand verdict)")
	}
	// No editorial_reviewed_head write happened: the store holds no MR bead,
	// and a landed review must not mint or update one.
	if len(store.issues) != 0 {
		t.Errorf("store has %d issue(s); a landed review must not write MR beads", len(store.issues))
	}
}

// writeTestManifest writes a valid harness manifest pointing at a stub om
// binary, mirroring newReviewFixture's rig setup.
func writeTestManifest(t *testing.T, rigDir string) error {
	t.Helper()
	binPath := filepath.Join(rigDir, "om-stub-binary")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho om\n"), 0755); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte("#!/bin/sh\necho om\n"))
	var m Manifest
	m.OMBinary.Path = binPath
	m.OMBinary.SHA256 = hex.EncodeToString(sum[:])
	m.OMBinary.Version = "1.4.0"
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rigDir, manifestFileName), data, 0644)
}

// TestFileFollowups_NoMRIDSkipsTheComment covers the retro-review path in
// fileFollowups: a landed review names no MR bead, so there is nothing to
// comment on. The follow-up beads are still filed — they are the record — and
// the run must not fail trying to comment on a bead that does not exist.
func TestFileFollowups_NoMRIDSkipsTheComment(t *testing.T) {
	// A bd that cannot comment at all: any attempt to comment fails, so a run
	// that returns cleanly proves none was attempted.
	binDir := t.TempDir()
	script := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  comments) echo 'comments: no such issue' >&2; exit 1 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := beads.NewWithStore(t.TempDir(), newReviewStore())
	majors := []Finding{{Title: "a major finding", Path: "internal/x.go", Line: 12}}

	ids, err := fileFollowups(b, "", 0.87, majors)
	if err != nil {
		t.Fatalf("fileFollowups with no MR id: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("ids = %v, want the one filed follow-up", ids)
	}

	// With an MR id the same uncommentable bead makes the failure visible,
	// which is what gives the clean run above its meaning.
	if _, err := fileFollowups(b, "gt-mr-gone", 0.87, majors); err == nil {
		t.Error("fileFollowups with an MR id swallowed a comment failure")
	}
}
