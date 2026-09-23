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

// driftFixture builds a repo where target advances from a shared base commit
// while an unrelated MR branch also advances from that same base, so the
// two are independent commits rather than an ancestor chain — mirroring how
// target moves during a merge-queue gate while an MR's branch sits still.
// targetChangesFile is the path the simulated target advance touches; the MR
// branch always touches "feature.txt". Returns the git client, the shared
// base (both target's tip at review time and the MR's merge-base), the MR's
// head, and target's post-advance tip.
func driftFixture(t *testing.T, targetChangesFile string) (g *git.Git, base, head, newTargetTip string) {
	t.Helper()
	dir := initTestRepo(t)
	g = git.NewGit(dir)
	var err error
	base, err = g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	head = commitFile(t, dir, "feature.txt", "feature v1\n", "add feature")

	if err := g.CheckoutDetach(base); err != nil {
		t.Fatalf("checkout detach %s: %v", base, err)
	}
	newTargetTip = commitFile(t, dir, targetChangesFile, "advance\n", "target advanced")
	return g, base, head, newTargetTip
}

// driftNote writes an approve note for base..head, recording reviewedTip as
// the target tip at review time.
func driftNote(t *testing.T, g *git.Git, base, head, reviewedTip string) {
	t.Helper()
	patchID, err := g.PatchID(base, head)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	n := Note{
		OMVersion:         "1.4.0",
		Rig:               "gastown",
		MR:                "gt-wisp-x",
		Worker:            "marble",
		BaseSHA:           base,
		ReviewedTargetTip: reviewedTip,
		HeadSHA:           head,
		PatchID:           patchID,
		Score:             0.8,
		Verdict:           "approve",
		Attempt:           1,
		ReviewedAt:        time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC),
	}
	if err := WriteNote(g, n); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
}

func TestCheckPrecondition_TargetDriftMaterial_OverlappingFiles_Refused(t *testing.T) {
	g, base, head, newTargetTip := driftFixture(t, "feature.txt")
	driftNote(t, g, base, head, base)

	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head, TargetTip: newTargetTip}}
	_, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr == nil || cerr.Reason != ReasonTargetDriftMaterial {
		t.Fatalf("CheckPrecondition: got %+v, want reason=%s", cerr, ReasonTargetDriftMaterial)
	}
}

func TestCheckPrecondition_TargetDriftDisjointFiles_OK(t *testing.T) {
	g, base, head, newTargetTip := driftFixture(t, "unrelated.txt")
	driftNote(t, g, base, head, base)

	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head, TargetTip: newTargetTip}}
	notes, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr != nil {
		t.Fatalf("CheckPrecondition: unexpected error %+v", cerr)
	}
	if len(notes) != 1 {
		t.Fatalf("CheckPrecondition: got notes %+v", notes)
	}
}

func TestCheckPrecondition_TargetNotMoved_DriftCheckSkipped(t *testing.T) {
	g, base, head, _ := driftFixture(t, "feature.txt")
	// Target has not moved since review: reviewedTip and the MR's push-time
	// TargetTip are both base.
	driftNote(t, g, base, head, base)

	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head, TargetTip: base}}
	notes, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr != nil {
		t.Fatalf("CheckPrecondition: unexpected error %+v", cerr)
	}
	if len(notes) != 1 {
		t.Fatalf("CheckPrecondition: got notes %+v", notes)
	}
}

func TestCheckPrecondition_NoteMissingReviewedTargetTip_DriftCheckSkipped(t *testing.T) {
	// A note written before ReviewedTargetTip existed (empty) must not
	// refuse the push even though target has materially moved — the field
	// is unknown, not zero, and is skipped rather than guessed.
	g, base, head, newTargetTip := driftFixture(t, "feature.txt")
	driftNote(t, g, base, head, "")

	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head, TargetTip: newTargetTip}}
	notes, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr != nil {
		t.Fatalf("CheckPrecondition: unexpected error %+v", cerr)
	}
	if len(notes) != 1 {
		t.Fatalf("CheckPrecondition: got notes %+v", notes)
	}
}

func TestCheckPrecondition_MRTargetTipUnset_DriftCheckSkipped(t *testing.T) {
	// A caller that never resolved TargetTip (e.g. an older call site) must
	// not have every push refused — unknown is skipped, not guessed.
	g, base, head, _ := driftFixture(t, "feature.txt")
	driftNote(t, g, base, head, base)

	mrs := []LandedMR{{MRID: "gt-wisp-x", ReviewedHead: head, Base: base, Head: head}}
	notes, cerr := CheckPrecondition(g, requiredCfg(), mrs)
	if cerr != nil {
		t.Fatalf("CheckPrecondition: unexpected error %+v", cerr)
	}
	if len(notes) != 1 {
		t.Fatalf("CheckPrecondition: got notes %+v", notes)
	}
}

func TestTargetDriftedMaterially_UnresolvableRange_Errors(t *testing.T) {
	g, base, head, newTargetTip := driftFixture(t, "feature.txt")
	if _, err := targetDriftedMaterially(g, base, newTargetTip, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", head); err == nil {
		t.Fatal("targetDriftedMaterially: expected error for unresolvable base, got nil")
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
