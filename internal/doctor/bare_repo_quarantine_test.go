package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// seedUpstream creates an on-disk upstream bare repo with one commit on main,
// so a re-clone from config.json has something to clone.
func seedUpstream(t *testing.T, tmpDir string) string {
	t.Helper()
	upstream := filepath.Join(tmpDir, "upstream.git")
	work := filepath.Join(tmpDir, "work")
	for _, args := range [][]string{
		{"init", "--bare", upstream},
		{"init", "-b", "main", work},
		{"-C", work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init"},
		{"-C", work, "remote", "add", "origin", upstream},
		{"-C", work, "push", "origin", "main"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return upstream
}

// objectCount counts loose object files under a bare repo's objects dir, the
// data a RemoveAll would destroy.
func objectCount(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(filepath.Join(dir, "objects"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// quarantined returns the .repo.git.corrupt-* directories beside rigDir's repo.
func quarantined(t *testing.T, rigDir string) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(rigDir, ".repo.git.corrupt-*"))
	return matches
}

// G4-02: when git itself cannot run (CLT update, fork exhaustion, a codesign
// kill), rev-parse fails on a perfectly good repo. That is UNKNOWN, not
// corrupt: Run reports it and Fix must leave .repo.git and its objects alone.
func TestBareRepoExistsCheck_GitFailureIsNotCorruption(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git shim is POSIX-only")
	}
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	upstream := seedUpstream(t, tmpDir)
	bareRepo := initBareRepoWithRemote(t, rigDir, upstream)
	writeConfigJSON(t, rigDir, upstream, "")
	if out, err := exec.Command("git", "-C", bareRepo, "fetch", "origin", "main:main").CombinedOutput(); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	before := objectCount(t, bareRepo)
	if before == 0 {
		t.Fatal("fixture: bare repo has no objects")
	}

	// Every git invocation from here on fails the way a broken shim does.
	shimDir := t.TempDir()
	shim := "#!/bin/sh\necho 'xcrun: error: invalid active developer path' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	check := NewBareRepoExistsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status == StatusOK {
		t.Fatalf("Run = OK although git could not verify the repo: %s", result.Message)
	}
	if strings.Contains(strings.ToLower(result.Message), "corrupt") {
		t.Errorf("Run called an unverifiable repo corrupt: %q", result.Message)
	}

	_ = check.Fix(ctx)
	if got := objectCount(t, bareRepo); got != before {
		t.Fatalf("Fix changed .repo.git objects on a git failure: %d -> %d", before, got)
	}
	if q := quarantined(t, rigDir); len(q) != 0 {
		t.Fatalf("Fix quarantined a repo it could not verify: %v", q)
	}
}

// A corrupt shell (HEAD, refs, config gone; objects and worktrees left) that
// a worktree still references may hold that worktree's only copy of unpushed
// commits. Fix must refuse, name the worktree, and leave the objects.
func TestBareRepoExistsCheck_FixRefusesCorruptRepoWithRegisteredWorktree(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	upstream := seedUpstream(t, tmpDir)
	bareRepo := initBareRepoWithRemote(t, rigDir, upstream)
	writeConfigJSON(t, rigDir, upstream, "")
	if out, err := exec.Command("git", "-C", bareRepo, "fetch", "origin", "main:main").CombinedOutput(); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	setupWorktreeRef(t, rigDir, bareRepo)
	corruptBareRepo(t, bareRepo)
	before := objectCount(t, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}
	err := check.Fix(ctx)
	if err == nil {
		t.Fatal("Fix removed a corrupt .repo.git that a worktree still references")
	}
	if !strings.Contains(err.Error(), filepath.Join("refinery", "rig")) {
		t.Errorf("refusal %q does not name the referencing worktree", err)
	}
	if got := objectCount(t, bareRepo); got != before || before == 0 {
		t.Fatalf(".repo.git objects changed: %d -> %d", before, got)
	}
}

// A corrupt shell that still carries a local branch ref not identical to its
// origin tracking ref may be the only copy of that branch. Fix must refuse.
func TestBareRepoExistsCheck_FixRefusesCorruptRepoWithUnpushedRef(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	upstream := seedUpstream(t, tmpDir)
	bareRepo := initBareRepoWithRemote(t, rigDir, upstream)
	writeConfigJSON(t, rigDir, upstream, "")
	corruptBareRepo(t, bareRepo)
	ref := filepath.Join(bareRepo, "refs", "heads", "polecat", "work")
	if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref, []byte("0123456789abcdef0123456789abcdef01234567\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := NewBareRepoExistsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}
	err := check.Fix(ctx)
	if err == nil {
		t.Fatal("Fix removed a corrupt .repo.git holding an unpushed branch ref")
	}
	if !strings.Contains(err.Error(), "polecat/work") {
		t.Errorf("refusal %q does not name the unpushed ref", err)
	}
	if _, statErr := os.Stat(ref); statErr != nil {
		t.Fatalf("the unpushed ref is gone: %v", statErr)
	}
}

// A corrupt shell nothing depends on is moved aside, never deleted, and the
// repo is re-cloned. The old objects survive in the quarantine directory.
func TestBareRepoExistsCheck_FixQuarantinesUnneededCorruptRepo(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	upstream := seedUpstream(t, tmpDir)
	bareRepo := initBareRepoWithRemote(t, rigDir, upstream)
	writeConfigJSON(t, rigDir, upstream, "")
	if out, err := exec.Command("git", "-C", bareRepo, "fetch", "origin", "main:main").CombinedOutput(); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	corruptBareRepo(t, bareRepo)
	before := objectCount(t, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if err := bareRepoHealth(bareRepo); err != nil {
		t.Fatalf("re-cloned repo unhealthy: %v", err)
	}
	q := quarantined(t, rigDir)
	if len(q) != 1 {
		t.Fatalf("want one quarantine directory, got %v", q)
	}
	if got := objectCount(t, q[0]); got != before || before == 0 {
		t.Fatalf("quarantine holds %d objects, want %d", got, before)
	}
}
