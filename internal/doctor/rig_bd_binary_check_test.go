package doctor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writeFileAt creates path (and its parents) with the given contents and
// permissions, chmodding explicitly because WriteFile's mode is masked by the
// process umask.
func writeFileAt(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func TestNewRigBDBinaryCheck(t *testing.T) {
	t.Parallel()
	check := NewRigBDBinaryCheck()

	if check.Name() != "rig-bd-binary" {
		t.Errorf("Name() = %q, want %q", check.Name(), "rig-bd-binary")
	}
	if check.Category() != CategoryCleanup {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryCleanup)
	}
	if check.CanFix() {
		t.Error("CanFix() = true; the check never changes files, so it must not advertise a fix")
	}
}

// TestRigBDBinaryCheck_RunReportsFindingsWithRemedy pins the acceptance case:
// an executable bd inside a rig worktree is reported as a failure naming the
// path and the chmod remedy.
func TestRigBDBinaryCheck_RunReportsFindingsWithRemedy(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	crewBD := filepath.Join(town, "alpha", "crew", "sloan", "bd")
	polecatBD := filepath.Join(town, "alpha", "polecats", "ruby", "alpha", "bd")
	writeFileAt(t, crewBD, "binary\n", 0o755)
	writeFileAt(t, polecatBD, "binary\n", 0o700)

	check := NewRigBDBinaryCheck()
	result := check.Run(&CheckContext{TownRoot: town})

	if result.Status != StatusError {
		t.Fatalf("Status = %v (%s), want %v", result.Status, result.Message, StatusError)
	}
	if !strings.Contains(result.Message, "2 executable bd binary(ies)") {
		t.Errorf("Message = %q, want it to count 2 findings", result.Message)
	}

	joined := strings.Join(result.Details, "\n")
	for _, want := range []string{
		crewBD,
		polecatBD,
		"chmod 644 " + crewBD,
		"chmod 644 " + polecatBD,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("details missing %q:\n%s", want, joined)
		}
	}
}

// TestRigBDBinaryCheck_NonFindingsAreClean pins what must NOT be reported: a
// mode 644 file named bd, a directory named bd, an executable bd outside the
// bounded rig subdirectories, a symlink, and anything under .git, node_modules
// or beyond the depth bound.
func TestRigBDBinaryCheck_NonFindingsAreClean(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := filepath.Join(town, "alpha")

	// A non-executable build artifact is harmless.
	writeFileAt(t, filepath.Join(rig, "polecats", "nux", "alpha", "bd"), "binary\n", 0o644)
	// A directory named bd holds no executable itself.
	if err := os.MkdirAll(filepath.Join(rig, "crew", "jade", "bd"), 0o755); err != nil {
		t.Fatalf("mkdir bd dir: %v", err)
	}
	// Outside the four bounded subdirectories: the rig root, a scripts/ dir,
	// the town root, and a directory that is not a rig at all.
	writeFileAt(t, filepath.Join(rig, "bd"), "binary\n", 0o755)
	writeFileAt(t, filepath.Join(rig, "scripts", "bd"), "binary\n", 0o755)
	writeFileAt(t, filepath.Join(town, "bd"), "binary\n", 0o755)
	writeFileAt(t, filepath.Join(town, "notarig", "bd"), "binary\n", 0o755)
	// Skipped subtrees.
	writeFileAt(t, filepath.Join(rig, "crew", "sloan", ".git", "hooks", "bd"), "binary\n", 0o755)
	writeFileAt(t, filepath.Join(rig, "crew", "sloan", "node_modules", "x", "bd"), "binary\n", 0o755)
	// Past the depth bound (root a/b/c/d is depth 4).
	writeFileAt(t, filepath.Join(rig, "crew", "sloan", "a", "b", "c", "bd"), "binary\n", 0o755)
	// A symlink named bd is not a regular file and is never followed.
	target := filepath.Join(town, "outside-target")
	writeFileAt(t, target, "binary\n", 0o755)
	if err := os.MkdirAll(filepath.Join(rig, "crew", "orion"), 0o755); err != nil {
		t.Fatalf("mkdir orion: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(rig, "crew", "orion", "bd")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A symlinked crew entry is not a directory to descend into.
	if err := os.Symlink(filepath.Join(rig, "crew", "sloan"), filepath.Join(rig, "crew", "clone")); err != nil {
		t.Fatalf("symlink crew clone: %v", err)
	}

	check := NewRigBDBinaryCheck()
	result := check.Run(&CheckContext{TownRoot: town})

	if result.Status != StatusOK {
		t.Fatalf("Status = %v (%s); details:\n%s", result.Status, result.Message, strings.Join(result.Details, "\n"))
	}
}

// TestRigBDBinaryCheck_FindsExecutableInsideDirNamedBD pins that a directory
// named bd is descended into, not reported: a build that put the binary in
// ./bd/bd is still a stray executable.
func TestRigBDBinaryCheck_FindsExecutableInsideDirNamedBD(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	nested := filepath.Join(town, "alpha", "crew", "jade", "bd", "bd")
	writeFileAt(t, nested, "binary\n", 0o755)

	result := NewRigBDBinaryCheck().Run(&CheckContext{TownRoot: town})

	if result.Status != StatusError {
		t.Fatalf("Status = %v (%s), want findings under a dir named bd", result.Status, result.Message)
	}
	if joined := strings.Join(result.Details, "\n"); !strings.Contains(joined, nested) {
		t.Errorf("details missing %q:\n%s", nested, joined)
	}
}

// TestRigBDBinaryCheck_ScanRootsStayUnderRigs asserts every directory the
// check walks sits below a rig's crew/, polecats/, refinery/ or mayor/ — never
// the town root, a rig root, or an unrelated directory.
func TestRigBDBinaryCheck_ScanRootsStayUnderRigs(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	wantRoots := []string{
		filepath.Join(town, "alpha", "crew", "sloan"),
		filepath.Join(town, "alpha", "refinery", "one"),
		filepath.Join(town, "alpha", "mayor", "rig"),
		filepath.Join(town, "beta", "polecats", "ruby"),
	}
	for _, dir := range wantRoots {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// Not rigs/subdirs the check may touch.
	for _, dir := range []string{
		filepath.Join(town, "alpha", "crew", ".hidden"),
		filepath.Join(town, "alpha", "scripts", "tools"),
		filepath.Join(town, "notarig", "orphan"),
		filepath.Join(town, ".runtime"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// A non-directory child of crew/ is not a scan root.
	writeFileAt(t, filepath.Join(town, "alpha", "crew", "notes.txt"), "x\n", 0o644)

	roots := rigBDBinaryScanRoots(town)
	sort.Strings(roots)

	allowed := map[string]bool{"crew": true, "polecats": true, "refinery": true, "mayor": true}
	for _, root := range roots {
		rel, err := filepath.Rel(town, root)
		if err != nil {
			t.Fatalf("Rel(%s): %v", root, err)
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 2 {
			t.Errorf("scan root %q is at or above a rig directory (rel %q)", root, rel)
			continue
		}
		if !allowed[parts[1]] {
			t.Errorf("scan root %q is under rig subdir %q, not one of crew/polecats/refinery/mayor", root, parts[1])
		}
	}

	sort.Strings(wantRoots)
	if strings.Join(roots, "\n") != strings.Join(wantRoots, "\n") {
		t.Errorf("scan roots =\n%s\nwant\n%s", strings.Join(roots, "\n"), strings.Join(wantRoots, "\n"))
	}
}

// TestRigBDBinaryCheck_EmptyTownRootIsSkipped ensures a check that cannot tell
// where the rigs are reports skipped, never a clean pass.
func TestRigBDBinaryCheck_EmptyTownRootIsSkipped(t *testing.T) {
	t.Parallel()
	result := NewRigBDBinaryCheck().Run(&CheckContext{})
	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v (%s), want %v", result.Status, result.Message, StatusSkipped)
	}
}

// TestRigBDBinaryCheck_UnreadableTownRootIsSkipped ensures a town root that
// cannot be listed is skipped — an unreadable town must not read as clean.
func TestRigBDBinaryCheck_UnreadableTownRootIsSkipped(t *testing.T) {
	t.Parallel()
	result := NewRigBDBinaryCheck().Run(&CheckContext{TownRoot: filepath.Join(t.TempDir(), "missing")})
	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v (%s), want %v", result.Status, result.Message, StatusSkipped)
	}
}

// TestRigBDBinaryCheck_NoRigsIsClean covers a readable town with no rigs: no
// worktrees exist, so there is nothing to report.
func TestRigBDBinaryCheck_NoRigsIsClean(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	result := NewRigBDBinaryCheck().Run(&CheckContext{TownRoot: town})
	if result.Status != StatusOK {
		t.Fatalf("Status = %v (%s), want %v", result.Status, result.Message, StatusOK)
	}
}
