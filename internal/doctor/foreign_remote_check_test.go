package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNewForeignRemoteCheck(t *testing.T) {
	t.Parallel()
	check := NewForeignRemoteCheck()

	if check.Name() != "foreign-remotes" {
		t.Errorf("expected name 'foreign-remotes', got %q", check.Name())
	}

	if check.Description() == "" {
		t.Error("expected non-empty description")
	}

	if !check.CanFix() {
		t.Error("expected CanFix() to return true")
	}
}

func TestForeignRemoteCheck_NoGitRepo(t *testing.T) {
	t.Parallel()
	tmpDir, err := os.MkdirTemp("", "foreign-remote-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewForeignRemoteCheck()
	result := check.Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped for non-git dir (could not list remotes), got %v", result.Status)
	}
}

func TestForeignRemoteCheck_OnlyOrigin(t *testing.T) {
	t.Parallel()
	tmpDir := initForeignRemoteTestRepo(t)
	defer os.RemoveAll(tmpDir)

	ctx := &CheckContext{TownRoot: tmpDir}
	check := NewForeignRemoteCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK with only origin, got %v: %s", result.Status, result.Message)
	}
}

func TestForeignRemoteCheck_DetectsForeignRemote(t *testing.T) {
	t.Parallel()
	townDir := initForeignRemoteTestRepo(t)
	defer os.RemoveAll(townDir)

	foreignDir := initSeparateTestRepo(t)
	defer os.RemoveAll(foreignDir)

	runGit(t, townDir, "remote", "add", "gastown", foreignDir)
	runGit(t, townDir, "fetch", "gastown")

	ctx := &CheckContext{TownRoot: townDir}
	check := NewForeignRemoteCheck()
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for foreign remote, got %v: %s", result.Status, result.Message)
	}

	if len(check.foreignRemotes) != 1 {
		t.Errorf("expected 1 foreign remote, got %d", len(check.foreignRemotes))
	}

	if check.foreignRemotes[0].name != "gastown" {
		t.Errorf("expected foreign remote name 'gastown', got %q", check.foreignRemotes[0].name)
	}
}

func TestForeignRemoteCheck_IgnoresRelatedRemote(t *testing.T) {
	t.Parallel()
	// Create town repo with origin, then clone origin to create a related repo
	townDir := initForeignRemoteTestRepo(t)
	defer os.RemoveAll(townDir)

	// Get origin URL so we can clone it
	originURL := runGit(t, townDir, "remote", "get-url", "origin")

	// Clone origin to create a related repo (shares commit ancestry)
	relatedDir, err := os.MkdirTemp("", "foreign-remote-related-*")
	if err != nil {
		t.Fatalf("failed to create related dir: %v", err)
	}
	defer os.RemoveAll(relatedDir)

	cmd := exec.Command("git", "clone", originURL, relatedDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone related: %v\n%s", err, out)
	}

	runGit(t, townDir, "remote", "add", "backup", relatedDir)
	runGit(t, townDir, "fetch", "backup")

	ctx := &CheckContext{TownRoot: townDir}
	check := NewForeignRemoteCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for related remote, got %v: %s", result.Status, result.Message)
	}
}

func TestForeignRemoteCheck_FixRemovesForeignRemotes(t *testing.T) {
	t.Parallel()
	townDir := initForeignRemoteTestRepo(t)
	defer os.RemoveAll(townDir)

	foreignDir := initSeparateTestRepo(t)
	defer os.RemoveAll(foreignDir)

	runGit(t, townDir, "remote", "add", "gastown", foreignDir)
	runGit(t, townDir, "fetch", "gastown")

	ctx := &CheckContext{TownRoot: townDir}
	check := NewForeignRemoteCheck()

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v", result.Status)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %s", result.Status, result.Message)
	}
}

// --- helpers (prefixed to avoid conflicts with branch_check_test.go) ---

// initForeignRemoteTestRepo returns a town repo on main whose origin is a bare
// repo it has pushed to. The pair is built once per test binary and copied.
func initForeignRemoteTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cachedGitTree(t, "foreign remote town", root, func(dir string) {
		town := filepath.Join(dir, "town")
		bare := filepath.Join(dir, "bare.git")
		if err := os.MkdirAll(town, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, town, "init", "-b", "main")
		if err := os.WriteFile(filepath.Join(town, "README.md"), []byte("# Town"), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
		runGit(t, town, "add", "README.md")
		runGit(t, town, "commit", "-m", "initial")
		runGit(t, dir, "init", "--bare", bare)
		runGit(t, town, "remote", "add", "origin", bare)
		runGit(t, town, "push", "origin", "main")
	})
	return filepath.Join(root, "town")
}

// initSeparateTestRepo returns a repo that shares no history with the town
// repo. It is built once per test binary and copied.
func initSeparateTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cachedGitTree(t, "foreign remote separate", dir, func(dir string) {
		runGit(t, dir, "init", "-b", "main")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Separate"), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
		runGit(t, dir, "add", "README.md")
		runGit(t, dir, "commit", "-m", "separate initial")
	})
	return dir
}
