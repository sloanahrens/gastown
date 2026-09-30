package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestBareRepoExistsCheck_Name(t *testing.T) {
	t.Parallel()
	check := NewBareRepoExistsCheck()
	if check.Name() != "bare-repo-exists" {
		t.Errorf("expected name 'bare-repo-exists', got %q", check.Name())
	}
	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}
}

func TestBareRepoExistsCheck_NoRig(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: t.TempDir(), RigName: ""}, gf)

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no rig specified, got %v", result.Status)
	}
}

func TestBareRepoExistsCheck_BareRepoExists(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create a real bare repo so the structural health check passes.
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, "https://github.com/example/repo.git")
	setupWorktreeRef(t, rigDir, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when .repo.git exists, got %v: %s", result.Status, result.Message)
	}
}

func TestBareRepoExistsCheck_NoBareRepoNoWorktrees(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create refinery/rig with a .git directory (not a worktree)
	refineryRig := filepath.Join(rigDir, "refinery", "rig", ".git")
	if err := os.MkdirAll(refineryRig, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no worktrees depend on .repo.git, got %v", result.Status)
	}
}

func TestBareRepoExistsCheck_MissingBareRepo(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create refinery/rig with a .git file pointing to missing .repo.git
	refineryRig := filepath.Join(rigDir, "refinery", "rig")
	if err := os.MkdirAll(refineryRig, 0755); err != nil {
		t.Fatal(err)
	}

	gitContent := "gitdir: " + filepath.Join(rigDir, ".repo.git", "worktrees", "rig") + "\n"
	if err := os.WriteFile(filepath.Join(refineryRig, ".git"), []byte(gitContent), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Errorf("expected StatusError when .repo.git is missing, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "missing .repo.git") {
		t.Errorf("expected message about missing .repo.git, got %q", result.Message)
	}
	if len(result.Details) < 2 {
		t.Errorf("expected at least 2 details (bare repo path + worktree), got %d", len(result.Details))
	}
}

func TestBareRepoExistsCheck_MultipleWorktreesMissing(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	bareRepoTarget := filepath.Join(rigDir, ".repo.git")

	// Create refinery/rig worktree
	refineryRig := filepath.Join(rigDir, "refinery", "rig")
	if err := os.MkdirAll(refineryRig, 0755); err != nil {
		t.Fatal(err)
	}
	gitContent := "gitdir: " + filepath.Join(bareRepoTarget, "worktrees", "rig") + "\n"
	if err := os.WriteFile(filepath.Join(refineryRig, ".git"), []byte(gitContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create polecat worktree
	polecatDir := filepath.Join(rigDir, "polecats", "worker1", rigName)
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatal(err)
	}
	polecatGit := "gitdir: " + filepath.Join(bareRepoTarget, "worktrees", "worker1") + "\n"
	if err := os.WriteFile(filepath.Join(polecatDir, ".git"), []byte(polecatGit), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Errorf("expected StatusError, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "2 worktree") {
		t.Errorf("expected message about 2 worktrees, got %q", result.Message)
	}
}

func TestBareRepoExistsCheck_RelativeGitdir(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create refinery/rig with a relative .git reference
	refineryRig := filepath.Join(rigDir, "refinery", "rig")
	if err := os.MkdirAll(refineryRig, 0755); err != nil {
		t.Fatal(err)
	}

	// Relative path from refinery/rig/ to .repo.git/worktrees/rig
	gitContent := "gitdir: ../../.repo.git/worktrees/rig\n"
	if err := os.WriteFile(filepath.Join(refineryRig, ".git"), []byte(gitContent), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Errorf("expected StatusError for broken relative gitdir, got %v", result.Status)
	}
}

func TestBareRepoExistsCheck_NonRepoGitWorktree(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create refinery/rig with a .git file pointing to something other than .repo.git
	refineryRig := filepath.Join(rigDir, "refinery", "rig")
	if err := os.MkdirAll(refineryRig, 0755); err != nil {
		t.Fatal(err)
	}

	gitContent := "gitdir: /some/other/path/worktrees/rig\n"
	if err := os.WriteFile(filepath.Join(refineryRig, ".git"), []byte(gitContent), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	// Worktree doesn't reference .repo.git, so this should pass
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when worktrees don't reference .repo.git, got %v", result.Status)
	}
}

// initBareRepoWithRemote makes rigDir/.repo.git a bare repository in gf with
// an origin remote and the refspec git remote add configures. On disk it is
// laid out as git lays one out (HEAD, refs/, objects/), since the checks
// inspect those files without git. Returns the bare repo path.
func initBareRepoWithRemote(t *testing.T, gf *gitfake.Fake, rigDir, fetchURL string) string {
	t.Helper()
	bareRepo := filepath.Join(rigDir, ".repo.git")
	gf.InitBare(t, bareRepo)
	gf.AddRemote(t, bareRepo, "origin", fetchURL)
	if err := bareGit(gf, bareRepo).ConfigSet("remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"refs/heads", "refs/tags", "objects"} {
		if err := os.MkdirAll(filepath.Join(bareRepo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bareRepo
}

// bareGit opens the bare repository at dir in gf, as the checks do.
func bareGit(gf *gitfake.Fake, dir string) Repo { return gf.OpenWithDir(dir, "").(Repo) }

// storeObjects writes loose object files into the bare repository at dir:
// the data that removing a repository would destroy.
func storeObjects(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"3b/18e512dba79e4c8300dd08aeb37f8e728b8dad", "e6/9de29bb2d1d6434b8b29ae775ad8c2e48c5391"} {
		path := filepath.Join(dir, "objects", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

// setupWorktreeRef creates a refinery/rig directory with a .git file pointing to .repo.git.
func setupWorktreeRef(t *testing.T, rigDir, bareRepo string) {
	t.Helper()
	refineryRig := filepath.Join(rigDir, "refinery", "rig")
	if err := os.MkdirAll(refineryRig, 0755); err != nil {
		t.Fatal(err)
	}
	worktreeDir := filepath.Join(bareRepo, "worktrees", "rig")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatal(err)
	}
	gitContent := "gitdir: " + filepath.Join(bareRepo, "worktrees", "rig") + "\n"
	if err := os.WriteFile(filepath.Join(refineryRig, ".git"), []byte(gitContent), 0644); err != nil {
		t.Fatal(err)
	}
}

// writeConfigJSON writes a config.json with optional push_url.
func writeConfigJSON(t *testing.T, rigDir, gitURL, pushURL string) {
	t.Helper()
	cfg := map[string]string{"git_url": gitURL}
	if pushURL != "" {
		cfg["push_url"] = pushURL
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBareRepoExistsCheck_PushURLMismatch(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	fetchURL := "https://github.com/example/repo.git"
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, fetchURL)

	// Set a push URL on the bare repo that differs from config
	if err := bareGit(gf, bareRepo).ConfigurePushURL("origin", "https://github.com/user/wrong-fork.git"); err != nil {
		t.Fatalf("set-url --push failed: %v", err)
	}

	// Config says the push URL should be something else
	writeConfigJSON(t, rigDir, fetchURL, "https://github.com/user/correct-fork.git")
	setupWorktreeRef(t, rigDir, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for push URL mismatch, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "push URL") {
		t.Errorf("expected message about push URL, got %q", result.Message)
	}
}

func TestBareRepoExistsCheck_LegacyConfigIgnoresPushURL(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	fetchURL := "https://github.com/example/repo.git"
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, fetchURL)

	// Set a push URL on the bare repo (may be from a pre-push_url-feature setup)
	if err := bareGit(gf, bareRepo).ConfigurePushURL("origin", "https://github.com/user/old-fork.git"); err != nil {
		t.Fatalf("set-url --push failed: %v", err)
	}

	// Config has NO push_url — legacy config that predates the push_url feature.
	// Doctor should NOT flag a mismatch; the existing push URL may be intentional.
	writeConfigJSON(t, rigDir, fetchURL, "")
	setupWorktreeRef(t, rigDir, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for legacy config (no push_url field), got %v: %s", result.Status, result.Message)
	}
}

func TestBareRepoExistsCheck_PushURLMatchesConfig(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	fetchURL := "https://github.com/example/repo.git"
	pushURL := "https://github.com/user/fork.git"
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, fetchURL)

	// Set push URL matching config
	if err := bareGit(gf, bareRepo).ConfigurePushURL("origin", pushURL); err != nil {
		t.Fatalf("set-url --push failed: %v", err)
	}

	writeConfigJSON(t, rigDir, fetchURL, pushURL)
	setupWorktreeRef(t, rigDir, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when push URL matches config, got %v: %s", result.Status, result.Message)
	}
}

func TestBareRepoExistsCheck_FixPushURLMismatch(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	fetchURL := "https://github.com/example/repo.git"
	correctPushURL := "https://github.com/user/correct-fork.git"
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, fetchURL)

	// Set wrong push URL
	if err := bareGit(gf, bareRepo).ConfigurePushURL("origin", "https://github.com/user/wrong-fork.git"); err != nil {
		t.Fatalf("set-url --push failed: %v", err)
	}

	writeConfigJSON(t, rigDir, fetchURL, correctPushURL)
	setupWorktreeRef(t, rigDir, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	// Run should detect mismatch
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", result.Status, result.Message)
	}

	// Fix should correct it
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Re-run should show OK
	check2 := NewBareRepoExistsCheck()
	result2 := check2.Run(ctx)
	if result2.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %s", result2.Status, result2.Message)
	}
}

// corruptBareRepo simulates the recurring partial-shell corruption mode:
// .repo.git is reduced to objects/ + worktrees/ (no HEAD, refs, config, hooks, info).
func corruptBareRepo(t *testing.T, bareRepo string) {
	t.Helper()
	for _, name := range []string{"HEAD", "config", "refs", "hooks", "info", "packed-refs"} {
		_ = os.RemoveAll(filepath.Join(bareRepo, name))
	}
}

func TestBareRepoExistsCheck_CorruptBareRepoDetected(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	bareRepo := initBareRepoWithRemote(t, gf, rigDir, "https://github.com/example/repo.git")
	setupWorktreeRef(t, rigDir, bareRepo)
	corruptBareRepo(t, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError for corrupt bare repo, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(strings.ToLower(result.Message), "corrupt") &&
		!strings.Contains(strings.ToLower(result.Message), "unusable") {
		t.Errorf("expected message to mention corruption/unusable, got %q", result.Message)
	}
	if result.FixHint == "" {
		t.Error("expected non-empty FixHint for corrupt bare repo")
	}
}

func TestBareRepoExistsCheck_FixCorruptBareRepoReclones(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// An upstream with a commit on main, so the clone has a default branch.
	upstream := fakeRemote(t, gf, filepath.Join(tmpDir, "upstream.git"), map[string]string{"README": "hi"})

	// Set up the rig with a corrupt .repo.git pointing at our upstream. No
	// worktree references it: a referenced corrupt repo is refused, see
	// TestBareRepoExistsCheck_FixRefusesCorruptRepoWithRegisteredWorktree.
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, upstream)
	writeConfigJSON(t, rigDir, upstream, "")
	corruptBareRepo(t, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError for corrupt repo, got %v: %s", result.Status, result.Message)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// The bare repo must be re-cloned and pass the structural health check.
	if err := bareRepoHealth(ctx, bareRepo); err != nil {
		t.Fatalf("bare repo still unhealthy after Fix: %v", err)
	}
	g := bareGit(gf, bareRepo)
	if ok, _ := g.RefExists("refs/heads/main"); !ok {
		t.Error("re-cloned repo has no main")
	}
	if refspec, _ := g.ConfigGet("remote.origin.fetch"); refspec != "+refs/heads/*:refs/remotes/origin/*" {
		t.Errorf("re-cloned refspec = %q", refspec)
	}
	// Re-running the check should now return OK.
	check2 := NewBareRepoExistsCheck()
	if result2 := check2.Run(ctx); result2.Status != StatusOK {
		t.Errorf("expected StatusOK after Fix re-cloned, got %v: %s", result2.Status, result2.Message)
	}
}

func TestBareRepoRefspecCheck_CorruptBareRepoErrors(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	bareRepo := initBareRepoWithRemote(t, gf, rigDir, "https://github.com/example/repo.git")
	corruptBareRepo(t, bareRepo)

	check := NewBareRepoRefspecCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)
	if result.Status != StatusError {
		t.Fatalf("expected StatusError on corrupt bare repo, got %v: %s", result.Status, result.Message)
	}
}

func TestBareRepoRefspecCheck_FixRefusesCorrupt(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	bareRepo := initBareRepoWithRemote(t, gf, rigDir, "https://github.com/example/repo.git")
	corruptBareRepo(t, bareRepo)

	check := NewBareRepoRefspecCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	if err := check.Fix(ctx); err == nil {
		t.Fatal("expected Fix to refuse writing config to a corrupt bare repo, got nil error")
	}
	// Critically: Fix must NOT have created .repo.git/config (the original bug).
	if _, err := os.Stat(filepath.Join(bareRepo, "config")); err == nil {
		t.Error("Fix wrote .repo.git/config on a corrupt bare repo — this is the bug it must prevent")
	}
}

func TestBareRepoExistsCheck_FixSkipsRepairedBetweenRunAndFix(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	bareRepo := initBareRepoWithRemote(t, gf, rigDir, "https://github.com/example/repo.git")
	setupWorktreeRef(t, rigDir, bareRepo)
	corruptBareRepo(t, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)
	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}

	// Operator manually repairs the repo between Run and Fix (TOCTOU window).
	// Re-init the bare repo in place.
	if err := os.RemoveAll(bareRepo); err != nil {
		t.Fatal(err)
	}
	gf.InitBare(t, bareRepo)
	// Capture the new HEAD inode/mtime to verify Fix doesn't touch it.
	infoBefore, err := os.Stat(filepath.Join(bareRepo, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	infoAfter, err := os.Stat(filepath.Join(bareRepo, "HEAD"))
	if err != nil {
		t.Fatalf("HEAD missing after Fix — Fix should have skipped a healthy repo: %v", err)
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Errorf("Fix re-created the bare repo even though it was healthy at Fix time")
	}
}

func TestBareRepoHealth_RejectsNonBareRepo(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	work := filepath.Join(t.TempDir(), "work")
	gf.InitRepo(t, work)
	// Point bareRepoHealth at the .git directory of a non-bare repo.
	if err := bareRepoHealth(withGit(&CheckContext{}, gf), filepath.Join(work, ".git")); err == nil {
		t.Error("expected bareRepoHealth to reject non-bare .git directory")
	}
}

func TestBareRepoRefspecCheck_HealthyRepoStillFixes(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	bareRepo := initBareRepoWithRemote(t, gf, rigDir, "https://github.com/example/repo.git")
	// Strip the refspec so Fix has work to do.
	if err := bareGit(gf, bareRepo).ConfigSet("remote.origin.fetch", ""); err != nil {
		t.Fatalf("unset refspec failed: %v", err)
	}

	check := NewBareRepoRefspecCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	if result := check.Run(ctx); result.Status != StatusError {
		t.Fatalf("expected StatusError when refspec missing, got %v: %s", result.Status, result.Message)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed on healthy repo: %v", err)
	}
	if result := check.Run(ctx); result.Status != StatusOK {
		t.Errorf("expected StatusOK after Fix, got %v: %s", result.Status, result.Message)
	}
}

func TestBareRepoExistsCheck_FixPreservesLegacyPushURL(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	fetchURL := "https://github.com/example/repo.git"
	legacyPushURL := "https://github.com/user/old-fork.git"
	bareRepo := initBareRepoWithRemote(t, gf, rigDir, fetchURL)

	// Set a push URL (from a pre-push_url-feature setup)
	if err := bareGit(gf, bareRepo).ConfigurePushURL("origin", legacyPushURL); err != nil {
		t.Fatalf("set-url --push failed: %v", err)
	}

	// Config has no push_url — legacy config
	writeConfigJSON(t, rigDir, fetchURL, "")
	setupWorktreeRef(t, rigDir, bareRepo)

	check := NewBareRepoExistsCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	// Run should NOT flag a mismatch for legacy configs
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK for legacy config, got %v: %s", result.Status, result.Message)
	}

	// Verify the push URL is preserved (not cleared)
	actualPush, err := bareGit(gf, bareRepo).GetPushURL("origin")
	if err != nil {
		t.Fatalf("GetPushURL failed: %v", err)
	}
	if actualPush != legacyPushURL {
		t.Errorf("expected push URL to be preserved as %q, got %q", legacyPushURL, actualPush)
	}
}
