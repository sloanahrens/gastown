package version

import (
	"path/filepath"
	"strings"
	"testing"
)

// formulaFile is the repository-relative path of a formula source file.
func formulaFile(name string) string {
	return filepath.Join(formulaSourceDir, name)
}

// sourceFile is a path outside the formula source dir, so tests can move the
// build branch forward without touching formula content.
const sourceFile = "internal/cmd/formula.go"

// TestCheckEmbeddedFormulaDrift_NamesFormulaFilesChangedSinceBuild verifies the
// check names exactly the formula files a build is missing, which is what tells
// an operator a "synced" line is not "current" (gt-dt7r).
func TestCheckEmbeddedFormulaDrift_NamesFormulaFilesChangedSinceBuild(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	binaryCommit := r.commit(formulaFile("mol-polecat-work.formula.toml"), "v1")

	// A formula fix and an unrelated source change both land after the build.
	r.commit(formulaFile("mol-refinery-patrol.formula.toml"), "v2")
	r.commit(sourceFile, "// unrelated")

	drift := g.checker(binaryCommit).checkFormulaDrift(repoDir)

	if !drift.Checked {
		t.Fatalf("Checked = false (%s), want a determined result", drift.Reason)
	}
	want := formulaFile("mol-refinery-patrol.formula.toml")
	if len(drift.Files) != 1 || drift.Files[0] != want {
		t.Errorf("Files = %v, want [%s]", drift.Files, want)
	}
	if drift.CommitsBehind != 2 {
		t.Errorf("CommitsBehind = %d, want the 2 commits added after the build", drift.CommitsBehind)
	}
	if drift.CompareRef != "main" || drift.BinaryCommit != binaryCommit {
		t.Errorf("CompareRef=%q BinaryCommit=%q, want main and the build commit", drift.CompareRef, drift.BinaryCommit)
	}
}

// TestCheckEmbeddedFormulaDrift_NoFormulaChangesIsCheckedAndEmpty verifies a
// build whose formula sources are current reports a determined empty result,
// distinct from an undetermined one.
func TestCheckEmbeddedFormulaDrift_NoFormulaChangesIsCheckedAndEmpty(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	built := r.commit(formulaFile("mol-polecat-work.formula.toml"), "v1")

	// A change outside the formula source dir must not register as drift.
	r.commit(sourceFile, "// unrelated")

	drift := g.checker(built).checkFormulaDrift(repoDir)

	if !drift.Checked {
		t.Fatalf("Checked = false (%s), want a determined result", drift.Reason)
	}
	if len(drift.Files) != 0 {
		t.Errorf("Files = %v, want none: no formula source changed", drift.Files)
	}
}

// TestCheckEmbeddedFormulaDrift_CurrentBuildIsChecked: a binary at the build
// ref tip has nothing pending and says so without diffing.
func TestCheckEmbeddedFormulaDrift_CurrentBuildIsChecked(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	tip := r.commit(formulaFile("mol-polecat-work.formula.toml"), "v1")

	drift := g.checker(tip).checkFormulaDrift(repoDir)

	if !drift.Checked || len(drift.Files) != 0 {
		t.Errorf("drift = %+v, want checked with no files", drift)
	}
}

// TestCheckEmbeddedFormulaDrift_DevBuildIsUnchecked verifies an unstamped build
// reports unknown rather than fresh — sync must not imply currency it cannot
// establish.
func TestCheckEmbeddedFormulaDrift_DevBuildIsUnchecked(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	r.commit(formulaFile("mol-polecat-work.formula.toml"), "v1")

	drift := g.checker("").checkFormulaDrift(repoDir)

	if drift.Checked {
		t.Error("Checked = true, want false for a build with no commit")
	}
	if !strings.Contains(drift.Reason, "dev build") {
		t.Errorf("Reason = %q, want the unstamped-build reason", drift.Reason)
	}
}

// TestCheckEmbeddedFormulaDrift_SkippedCheckIsUnchecked: a binary commit the
// checkout does not hold leaves the question open.
func TestCheckEmbeddedFormulaDrift_SkippedCheckIsUnchecked(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	r.commit(formulaFile("mol-polecat-work.formula.toml"), "v1")

	drift := g.checker("ffffffffffffffffffffffffffffffffffffffff").checkFormulaDrift(repoDir)

	if drift.Checked {
		t.Error("Checked = true, want false when the binary commit is not in the checkout")
	}
	if !strings.Contains(drift.Reason, "binary commit not found") {
		t.Errorf("Reason = %q, want the skip reason", drift.Reason)
	}
}

// TestCheckEmbeddedFormulaDrift_UnverifiableRefFailsClosed: an unreachable
// origin whose cached ref matches the binary looks perfectly current. Trusting
// it is the false reassurance gt-dt7r is about, so the check must report
// unknown instead.
func TestCheckEmbeddedFormulaDrift_UnverifiableRefFailsClosed(t *testing.T) {
	t.Parallel()
	g, r := newRepo(t)
	r.addRemote("origin", "/fake/does-not-exist")
	staleTip := r.commit(formulaFile("mol-polecat-work.formula.toml"), "v1")
	freshTip := r.commit(formulaFile("mol-polecat-work.formula.toml"), "v2")
	r.setRef("refs/heads/main", staleTip)
	r.setRef("refs/remotes/origin/main", freshTip)

	drift := g.checker(freshTip).checkFormulaDrift(repoDir)

	if drift.Checked {
		t.Errorf("Checked = true, want false: origin/main was never confirmed (files=%v)", drift.Files)
	}
	if drift.Reason == "" {
		t.Error("Reason is empty, want why the cached ref could not be trusted")
	}
}

// TestCheckEmbeddedFormulaDrift_BinaryNotBehindBuildRefIsNotDrift: a binary
// ahead of or diverged from origin/main is not behind it. Diffing the two
// would list the binary's own formula changes as missing, and tell the operator
// to rebuild for nothing.
func TestCheckEmbeddedFormulaDrift_BinaryNotBehindBuildRefIsNotDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// build adds commits after the shared origin/main base and returns the
		// commit the binary was built from.
		build func(r *fakeRepo) string
	}{
		{
			name: "binary ahead of origin/main",
			build: func(r *fakeRepo) string {
				r.checkoutNew("side")
				binary := r.commit(formulaFile("mol-refinery-patrol.formula.toml"), "v2")
				r.checkout("main")
				return binary
			},
		},
		{
			name: "binary diverged from origin/main",
			build: func(r *fakeRepo) string {
				r.checkoutNew("side")
				binary := r.commit(formulaFile("mol-refinery-patrol.formula.toml"), "v2")
				r.checkout("main")
				r.commit(formulaFile("mol-polecat-work.formula.toml"), "v3")
				r.push("origin", "main", "main")
				return binary
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, r := newRepo(t)
			g.repo("/fake/origin")
			r.addRemote("origin", "/fake/origin")
			r.commit(formulaFile("mol-polecat-work.formula.toml"), "v1")
			r.push("origin", "main", "main")
			binary := tc.build(r)

			drift := g.checker(binary).checkFormulaDrift(repoDir)

			if !drift.Checked {
				t.Fatalf("Checked = false (%s), want a determined result", drift.Reason)
			}
			if len(drift.Files) != 0 {
				t.Errorf("Files = %v, want none: the binary is not behind origin/main", drift.Files)
			}
		})
	}
}
