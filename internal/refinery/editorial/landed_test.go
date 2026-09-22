package editorial

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// ResolveLandedRange must reason about: a base commit, a branch landing
// (single parent), a branch merged onto the base (two parents), a target-
// ahead merge, and an evil merge whose tree is not the merge of its parents.
type landedFixture struct {
	repoDir   string
	g         *git.Git
	base      string
	branchTip string // branch's own head, on the branch line
	ffLanded  string // single-parent landing of the branch line
	merge     string // merge of the branch line onto the base
	target    string // a second commit landed on the target line after base
	evilMerge string // merge whose tree drops the other side
}

func newLandedFixture(t *testing.T) *landedFixture {
	t.Helper()
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	base := mustRev(t, g, "HEAD")

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Fake origin/main at base: the remote tip a landing is measured against.
	run("update-ref", "refs/remotes/origin/main", base)

	// A real origin remote (matching newReviewFixture) so the post-approve
	// note push has somewhere to go.
	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare origin: %v\n%s", err, out)
	}
	if _, err := g.AddRemote("origin", bareDir); err != nil {
		t.Fatalf("add remote origin: %v", err)
	}

	// Branch line: one feature commit off base.
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write("feature.txt", "feature\n")
	run("add", ".")
	run("commit", "-m", "feature")
	branchTip := mustRev(t, g, "HEAD")

	// Fast-forward landing: branch line reaches origin/main as a normal commit.
	ffLanded := branchTip
	run("update-ref", "refs/remotes/origin/main", branchTip)

	// Merge landing: advance the target line, then merge the branch in from a
	// detached base, so the merge's first parent is a target commit AHEAD of
	// the branch base.
	run("checkout", "-b", "main-line")
	run("reset", "--hard", base)
	write("target.txt", "target\n")
	run("add", ".")
	run("commit", "-m", "target work")
	target := mustRev(t, g, "HEAD")
	run("merge", branchTip, "-m", "merge feature into target")
	merge := mustRev(t, g, "HEAD")
	// The merge has landed: it is now the target's tip.
	run("update-ref", "refs/remotes/origin/main", merge)

	// Evil merge: same shape, but the merge tree drops the branch's file, so
	// no parent diff reproduces the landed diff.
	run("checkout", "-b", "evil-line")
	run("reset", "--hard", base)
	write("feature.txt", "feature v2\n")
	run("add", ".")
	run("commit", "-m", "feature v2")
	evilTip := mustRev(t, g, "HEAD")
	run("checkout", "main-line")
	run("merge", "-X", "theirs", evilTip, "-m", "evil merge")
	evilMerge := mustRev(t, g, "HEAD")
	run("rm", "--cached", "feature.txt")
	run("reset", "-q", "HEAD")

	return &landedFixture{
		repoDir:   dir,
		g:         g,
		base:      base,
		branchTip: branchTip,
		ffLanded:  ffLanded,
		merge:     merge,
		target:    target,
		evilMerge: evilMerge,
	}
}

func mustRev(t *testing.T, g *git.Git, rev string) string {
	t.Helper()
	s, err := g.Rev(rev)
	if err != nil {
		t.Fatalf("rev %s: %v", rev, err)
	}
	return s
}

func TestResolveLandedRange_FastForward(t *testing.T) {
	f := newLandedFixture(t)
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
	// first parent (which is ahead of it). The head is the side whose own
	// diff reproduces the landed diff: the branch tip.
	if r.Base != f.base {
		t.Errorf("Base = %s, want merge point %s (never the first parent %s)", r.Base, f.base, f.target)
	}
	if r.Head != f.branchTip {
		t.Errorf("Head = %s, want branch tip %s", r.Head, f.branchTip)
	}
	// The range's diff must reproduce the landed patch-id.
	if want := f.landedPatchID(t); r.PatchID != want {
		t.Errorf("PatchID = %q, want %q", r.PatchID, want)
	}
	if r.Head == r.Base {
		t.Error("Head == Base: the range would be empty")
	}
}

func (f *landedFixture) landedPatchID(t *testing.T) string {
	t.Helper()
	parents, err := f.g.Parents(f.merge)
	if err != nil {
		t.Fatalf("parents: %v", err)
	}
	id, err := f.g.PatchID(parents[0], f.merge)
	if err != nil {
		t.Fatalf("patch-id of landed diff: %v", err)
	}
	return id
}

func TestResolveLandedRange_EvilMergeRefused(t *testing.T) {
	f := newLandedFixture(t)
	_, err := ResolveLandedRange(f.g, f.evilMerge, "main")
	if err == nil {
		t.Fatal("ResolveLandedRange: want refusal for a merge whose tree is not the merge of its parents")
	}
	// The refusal names the disagreement rather than silently picking a side.
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("refusal = %q, want it to say 'refusing'", err.Error())
	}
}

func TestResolveLandedRange_NotLandedRefused(t *testing.T) {
	f := newLandedFixture(t)
	// The base commit is not reachable from origin/main (which is at the
	// branch tip after the fixture), so it has no landed diff.
	_, err := ResolveLandedRange(f.g, f.base, "main")
	if err == nil {
		t.Fatal("ResolveLandedRange: want refusal for a commit not reachable from the target")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("refusal = %q, want it to say 'refusing'", err.Error())
	}
}

func TestResolveLandedRange_RootRefused(t *testing.T) {
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	// The initial commit is a root; point origin/main at it so reachability
	// passes and the root check is what fires.
	root := mustRev(t, g, "HEAD")
	cmd := exec.Command("git", "update-ref", "refs/remotes/origin/main", root)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	_, err := ResolveLandedRange(g, root, "main")
	if err == nil {
		t.Fatal("ResolveLandedRange: want refusal for a root commit")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("refusal = %q, want it to name the root commit", err.Error())
	}
}

// TestRun_LandedReview_StampNoteOnLandedCommit pins the landed path's two
// load-bearing differences from the MR path: the note is stamped on the
// landed commit (not a rehearsal head or branch tip), and no
// editorial_reviewed_head is written to any MR bead.
func TestRun_LandedReview_StampNoteOnLandedCommit(t *testing.T) {
	fakeBDForReview(t)
	f := newLandedFixture(t)
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
		RigDir:         rigDir,
		RepoDir:        f.repoDir,
		MRID:           "gt-mr-landed",
		Rig:            "gastown",
		Target:         "main",
		Landed:         &landed,
		Attempt:        1,
		Config:         config.EditorialConfig{Required: true},
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