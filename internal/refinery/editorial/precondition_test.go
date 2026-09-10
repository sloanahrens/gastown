package editorial

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// commitFile writes content to path in dir, stages it, and commits with
// message, mirroring internal/git's own test helper since that one is
// unexported and this is a different package.
func commitFile(t *testing.T, dir, path, content, message string) string {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	g := git.NewGit(dir)
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

// reviewedFixture builds a repo with a base commit and a reviewed head
// commit on top of it, writes an approve note on the head with the given
// patch-id, and returns the git client, base, and head shas.
func reviewedFixture(t *testing.T, patchID, verdict, omVersion string) (g *git.Git, base, head string) {
	t.Helper()
	dir := initTestRepo(t)
	g = git.NewGit(dir)
	var err error
	base, err = g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	head = commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	if patchID == "" {
		patchID, err = g.PatchID(base, head)
		if err != nil {
			t.Fatalf("PatchID: %v", err)
		}
	}
	n := Note{
		OMVersion:  omVersion,
		Rig:        "gastown",
		MR:         "gt-wisp-x",
		Worker:     "marble",
		BaseSHA:    base,
		HeadSHA:    head,
		PatchID:    patchID,
		Score:      0.8,
		Verdict:    verdict,
		Attempt:    1,
		ReviewedAt: time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC),
	}
	if err := WriteNote(g, n); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
	return g, base, head
}

func requiredCfg() config.EditorialConfig {
	return config.EditorialConfig{Required: true}
}

func TestCheckPrecondition_ApproveMatchingPatchID_OK(t *testing.T) {
	g, base, head := reviewedFixture(t, "", "approve", "1.4.0")
	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head}}

	notes, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr != nil {
		t.Fatalf("CheckPrecondition: unexpected error %+v", cerr)
	}
	if len(notes) != 1 || notes[0].HeadSHA != head {
		t.Fatalf("CheckPrecondition: got notes %+v", notes)
	}
}

func TestCheckPrecondition_NoNote_Missing(t *testing.T) {
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	base, _ := g.Rev("HEAD")
	head := commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head}}
	_, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr == nil || cerr.Reason != "missing" {
		t.Fatalf("CheckPrecondition: got %+v, want reason=missing", cerr)
	}
	if cerr.Class != Precondition {
		t.Fatalf("CheckPrecondition: got class %v, want Precondition", cerr.Class)
	}
}

func TestCheckPrecondition_EmptyReviewedHead_Missing(t *testing.T) {
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	base, _ := g.Rev("HEAD")
	head := commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: "", Base: base, Head: head}}
	_, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr == nil || cerr.Reason != "missing" {
		t.Fatalf("CheckPrecondition: got %+v, want reason=missing", cerr)
	}
}

func TestCheckPrecondition_RangeEdited_PatchIDMismatch(t *testing.T) {
	g, base, head := reviewedFixture(t, "", "approve", "1.4.0")
	dir := g.WorkDir()
	// Edit the range after review: append a second commit changing the diff.
	newHead := commitFile(t, dir, "feature.txt", "hello world\n", "edit after review")

	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: newHead}}
	_, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr == nil || cerr.Reason != "patch_id_mismatch" {
		t.Fatalf("CheckPrecondition: got %+v, want reason=patch_id_mismatch", cerr)
	}
}

func TestCheckPrecondition_VersionBelowMin(t *testing.T) {
	g, base, head := reviewedFixture(t, "", "approve", "0.9.0")
	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head}}

	cfg := requiredCfg()
	cfg.MinVersion = "1.0.0"
	_, cerr := CheckPrecondition(g, cfg, mrs)
	if cerr == nil || cerr.Reason != "version_below_min" {
		t.Fatalf("CheckPrecondition: got %+v, want reason=version_below_min", cerr)
	}
}

func TestCheckPrecondition_RequestChanges_VerdictNotApprove(t *testing.T) {
	g, base, head := reviewedFixture(t, "", "request_changes", "1.4.0")
	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head}}

	_, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr == nil || cerr.Reason != "verdict_not_approve" {
		t.Fatalf("CheckPrecondition: got %+v, want reason=verdict_not_approve", cerr)
	}
}

func TestCheckPrecondition_NotRequired_Skipped(t *testing.T) {
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	base, _ := g.Rev("HEAD")
	head := commitFile(t, dir, "feature.txt", "hello\n", "add feature")

	// No note at all — would fail as "missing" if the check ran.
	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: "", Base: base, Head: head}}
	notes, cerr := CheckPrecondition(g, config.EditorialConfig{Required: false}, mrs)
	if cerr != nil {
		t.Fatalf("CheckPrecondition: unexpected error %+v", cerr)
	}
	if notes != nil {
		t.Fatalf("CheckPrecondition: got notes %+v, want nil", notes)
	}
}

func TestCopyNotesToLanded_CopiesWhenHeadsDiffer(t *testing.T) {
	g, base, reviewedHead := reviewedFixture(t, "", "approve", "1.4.0")
	dir := g.WorkDir()
	landedHead := commitFile(t, dir, "other.txt", "unrelated\n", "unrelated landed commit")

	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: reviewedHead, Base: base, Head: landedHead}}
	notes, cerr := CheckPrecondition(g, requiredCfg(), []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: reviewedHead, Base: base, Head: reviewedHead}})
	if cerr != nil {
		t.Fatalf("CheckPrecondition: unexpected error %+v", cerr)
	}

	if err := CopyNotesToLanded(g, mrs, notes); err != nil {
		t.Fatalf("CopyNotesToLanded: %v", err)
	}

	got, err := ReadNote(g, landedHead)
	if err != nil {
		t.Fatalf("ReadNote(landedHead): %v", err)
	}
	if got.PatchID != notes[0].PatchID {
		t.Fatalf("copied note mismatch: got %+v, want patch_id %s", got, notes[0].PatchID)
	}
}

func TestCopyNotesToLanded_NoOpWhenSameCommit(t *testing.T) {
	g, base, head := reviewedFixture(t, "", "approve", "1.4.0")
	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head}}
	notes, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr != nil {
		t.Fatalf("CheckPrecondition: unexpected error %+v", cerr)
	}
	if err := CopyNotesToLanded(g, mrs, notes); err != nil {
		t.Fatalf("CopyNotesToLanded: %v", err)
	}
}
