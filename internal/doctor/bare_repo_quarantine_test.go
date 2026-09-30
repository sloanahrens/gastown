package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// seedUpstream creates an upstream bare repo in gf with one commit on main,
// so a re-clone from config.json has something to clone.
func seedUpstream(t *testing.T, gf *gitfake.Fake, tmpDir string) string {
	t.Helper()
	return fakeRemote(t, gf, filepath.Join(tmpDir, "upstream.git"), nil)
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

// brokenGit is git that cannot run at all.
type brokenGit struct{ Repo }

var errGitBroken = errors.New("xcrun: error: invalid active developer path")

func (brokenGit) GitDir() (string, error)          { return "", errGitBroken }
func (brokenGit) IsBareRepository() (bool, error)  { return false, errGitBroken }
func (brokenGit) RemoteURL(string) (string, error) { return "", errGitBroken }
func (brokenGit) GetPushURL(string) (string, error) {
	return "", errGitBroken
}

// gt-fcxe9.1: when git itself cannot run (CLT update, fork exhaustion, a codesign
// kill), rev-parse fails on a perfectly good repo. That is UNKNOWN, not
// corrupt: Run reports it and Fix must leave .repo.git and its objects alone.
func TestBareRepoExistsCheck_GitFailureIsNotCorruption(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	upstream := seedUpstream(t, gf, tmpDir)
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, upstream)
	writeConfigJSON(t, rigDir, upstream, "")
	storeObjects(t, bareRepo)
	before := objectCount(t, bareRepo)
	if before == 0 {
		t.Fatal("fixture: bare repo has no objects")
	}

	// Every git invocation from here on fails the way a broken toolchain does.
	check := NewBareRepoExistsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	ctx.openGit = func(gitDir, workDir string) Repo { return brokenGit{gf.OpenWithDir(gitDir, workDir).(Repo)} }
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
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	upstream := seedUpstream(t, gf, tmpDir)
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, upstream)
	writeConfigJSON(t, rigDir, upstream, "")
	storeObjects(t, bareRepo)
	setupWorktreeRef(t, rigDir, bareRepo)
	corruptBareRepo(t, bareRepo)
	before := objectCount(t, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}
	err := check.Fix(ctx)
	if err == nil {
		t.Fatal("Fix removed a corrupt .repo.git that a worktree still references")
	}
	if !strings.Contains(err.Error(), filepath.Join("polecats", "nux")) {
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
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	upstream := seedUpstream(t, gf, tmpDir)
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, upstream)
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
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)
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
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	upstream := seedUpstream(t, gf, tmpDir)
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, upstream)
	writeConfigJSON(t, rigDir, upstream, "")
	storeObjects(t, bareRepo)
	corruptBareRepo(t, bareRepo)
	before := objectCount(t, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if err := bareRepoHealth(ctx, bareRepo); err != nil {
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
