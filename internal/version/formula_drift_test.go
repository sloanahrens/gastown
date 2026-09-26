package version

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedFormulaSource writes a file under the embedded-formula source dir and
// returns the commit that carries it.
func seedFormulaSource(t *testing.T, dir, file, content string) string {
	t.Helper()
	full := filepath.Join(dir, formulaSourceDir, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	return gitCommit(t, dir, filepath.Join(formulaSourceDir, file), content)
}

// commitSourceChange commits a change outside the formula source dir, so tests
// can move the build branch forward without touching formula content.
func commitSourceChange(t *testing.T, dir, content string) string {
	t.Helper()
	full := filepath.Join(dir, "internal", "cmd", "formula.go")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	return gitCommit(t, dir, filepath.Join("internal", "cmd", "formula.go"), content)
}

// TestCheckEmbeddedFormulaDrift_NamesFormulaFilesChangedSinceBuild verifies the
// check names exactly the formula files a build is missing, which is what tells
// an operator a "synced" line is not "current" (gt-dt7r).
func TestCheckEmbeddedFormulaDrift_NamesFormulaFilesChangedSinceBuild(t *testing.T) {
	dir := newGitRepo(t)
	binaryCommit := seedFormulaSource(t, dir, "mol-polecat-work.formula.toml", "v1")
	gitRun(t, dir, "branch", "-M", "main")
	setBinaryCommit(t, binaryCommit)

	// A formula fix and an unrelated source change both land after the build.
	seedFormulaSource(t, dir, "mol-refinery-patrol.formula.toml", "v2")
	commitSourceChange(t, dir, "// unrelated")

	drift := CheckEmbeddedFormulaDrift(dir)

	if !drift.Checked {
		t.Fatalf("Checked = false (%s), want a determined result", drift.Reason)
	}
	want := filepath.Join(formulaSourceDir, "mol-refinery-patrol.formula.toml")
	if len(drift.Files) != 1 || drift.Files[0] != want {
		t.Errorf("Files = %v, want [%s]", drift.Files, want)
	}
	if drift.CommitsBehind == 0 {
		t.Error("CommitsBehind = 0, want the commits added after the build")
	}
}

// TestCheckEmbeddedFormulaDrift_NoFormulaChangesIsCheckedAndEmpty verifies a
// build whose formula sources are current reports a determined empty result,
// distinct from an undetermined one.
func TestCheckEmbeddedFormulaDrift_NoFormulaChangesIsCheckedAndEmpty(t *testing.T) {
	dir := newGitRepo(t)
	seedFormulaSource(t, dir, "mol-polecat-work.formula.toml", "v1")
	gitRun(t, dir, "branch", "-M", "main")
	setBinaryCommit(t, gitRun(t, dir, "rev-parse", "HEAD"))

	// A change outside the formula source dir must not register as drift.
	commitSourceChange(t, dir, "// unrelated")

	drift := CheckEmbeddedFormulaDrift(dir)

	if !drift.Checked {
		t.Fatalf("Checked = false (%s), want a determined result", drift.Reason)
	}
	if len(drift.Files) != 0 {
		t.Errorf("Files = %v, want none: no formula source changed", drift.Files)
	}
}

// TestCheckEmbeddedFormulaDrift_DevBuildIsUnchecked verifies an unstamped build
// reports unknown rather than fresh — sync must not imply currency it cannot
// establish.
func TestCheckEmbeddedFormulaDrift_DevBuildIsUnchecked(t *testing.T) {
	dir := newGitRepo(t)
	seedFormulaSource(t, dir, "mol-polecat-work.formula.toml", "v1")
	gitRun(t, dir, "branch", "-M", "main")
	setBinaryCommit(t, "")

	// The branch this test is about needs an empty commit. Go stamps
	// vcs.revision into some test binaries, and no environment variable reaches
	// that (an earlier version of this test cleared GIT_DIR, which cannot
	// affect build info); skip where it is present rather than pass through
	// the throwaway "binary commit not found" path and assert nothing.
	if got := resolveCommitHash(); got != "" {
		t.Skipf("test binary carries commit %s; the dev-build path needs none", got)
	}

	drift := CheckEmbeddedFormulaDrift(dir)

	if drift.Checked {
		t.Error("Checked = true, want false for a build with no commit")
	}
	if !strings.Contains(drift.Reason, "dev build") {
		t.Errorf("Reason = %q, want the unstamped-build reason", drift.Reason)
	}
}

// TestCheckEmbeddedFormulaDrift_UnverifiableRefFailsClosed: an unreachable
// origin whose cached ref matches the binary looks perfectly current. Trusting
// it is the false reassurance gt-dt7r is about, so the check must report
// unknown instead.
func TestCheckEmbeddedFormulaDrift_UnverifiableRefFailsClosed(t *testing.T) {
	dir := newGitRepo(t)
	gitRun(t, dir, "remote", "add", "origin", filepath.Join(t.TempDir(), "does-not-exist"))
	staleTip := seedFormulaSource(t, dir, "mol-polecat-work.formula.toml", "v1")
	freshTip := seedFormulaSource(t, dir, "mol-polecat-work.formula.toml", "v2")
	gitRun(t, dir, "branch", "-M", "main")
	gitRun(t, dir, "update-ref", "refs/heads/main", staleTip)
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", freshTip)
	setBinaryCommit(t, freshTip)

	drift := CheckEmbeddedFormulaDrift(dir)

	if drift.Checked {
		t.Errorf("Checked = true, want false: origin/main was never confirmed (files=%v)", drift.Files)
	}
	if drift.Reason == "" {
		t.Error("Reason is empty, want why the cached ref could not be trusted")
	}
}
