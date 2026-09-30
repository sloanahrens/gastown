package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestNewSparseCheckoutCheck(t *testing.T) {
	t.Parallel()
	check := NewSparseCheckoutCheck()

	if check.Name() != "sparse-checkout" {
		t.Errorf("expected name 'sparse-checkout', got %q", check.Name())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}
}

func TestSparseCheckoutCheck_NoRigSpecified(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: ""}, gf)

	result := check.Run(ctx)

	// No rig specified + no rigs found = StatusOK (nothing to check)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no rigs found, got %v", result.Status)
	}
}

func TestSparseCheckoutCheck_TownWideMode(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()

	// Create two rigs with config.json so discoverRigPaths finds them
	rig1Dir := filepath.Join(tmpDir, "rig1")
	rig2Dir := filepath.Join(tmpDir, "rig2")

	// rig1: mayor/rig with legacy sparse checkout
	mayorRig1 := filepath.Join(rig1Dir, "mayor", "rig")
	initGitRepo(t, gf, mayorRig1)
	configureLegacySparseCheckout(t, gf, mayorRig1)
	if err := os.WriteFile(filepath.Join(rig1Dir, "config.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// rig2: mayor/rig with legacy sparse checkout
	mayorRig2 := filepath.Join(rig2Dir, "mayor", "rig")
	initGitRepo(t, gf, mayorRig2)
	configureLegacySparseCheckout(t, gf, mayorRig2)
	if err := os.WriteFile(filepath.Join(rig2Dir, "config.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: ""}, gf) // no --rig flag

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning in town-wide mode, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "2 repo(s) have legacy") {
		t.Errorf("expected message about 2 repos, got %q", result.Message)
	}
	if len(result.Details) != 2 {
		t.Errorf("expected 2 details, got %d: %v", len(result.Details), result.Details)
	}
}

func TestSparseCheckoutCheck_NoGitRepos(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)

	// No git repos found = StatusOK (nothing to check)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no git repos, got %v", result.Status)
	}
}

// initGitRepo makes path a repo in gf holding one commit of README.md.
func initGitRepo(t *testing.T, gf *gitfake.Fake, path string) {
	t.Helper()
	gf.InitRepo(t, path)
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gf.CommitWorktree(t, path, "Initial commit")
}

// configureLegacySparseCheckout sets up legacy sparse checkout that should be removed.
func configureLegacySparseCheckout(t *testing.T, gf *gitfake.Fake, repoPath string) {
	t.Helper()
	if err := gf.OpenBranchRepo(repoPath).(Repo).ConfigSet("core.sparseCheckout", "true"); err != nil {
		t.Fatalf("enabling sparse checkout: %v", err)
	}
}

func TestSparseCheckoutCheck_NoSparseCheckout(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create mayor/rig as a git repo without sparse checkout
	mayorRig := filepath.Join(rigDir, "mayor", "rig")
	initGitRepo(t, gf, mayorRig)

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)

	// No sparse checkout = StatusOK (nothing to clean up)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no sparse checkout, got %v", result.Status)
	}
}

func TestSparseCheckoutCheck_LegacySparseCheckoutDetected(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create mayor/rig with legacy sparse checkout
	mayorRig := filepath.Join(rigDir, "mayor", "rig")
	initGitRepo(t, gf, mayorRig)
	configureLegacySparseCheckout(t, gf, mayorRig)

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for legacy sparse checkout, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "1 repo(s) have legacy") {
		t.Errorf("expected message about legacy sparse checkout, got %q", result.Message)
	}
	if len(result.Details) != 1 || !strings.Contains(filepath.ToSlash(result.Details[0]), "mayor/rig") {
		t.Errorf("expected details to contain mayor/rig, got %v", result.Details)
	}
}

func TestSparseCheckoutCheck_MultipleReposWithLegacySparseCheckout(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create multiple git repos with legacy sparse checkout
	mayorRig := filepath.Join(rigDir, "mayor", "rig")
	initGitRepo(t, gf, mayorRig)
	configureLegacySparseCheckout(t, gf, mayorRig)

	crewAgent := filepath.Join(rigDir, "crew", "agent1")
	initGitRepo(t, gf, crewAgent)
	configureLegacySparseCheckout(t, gf, crewAgent)

	// Polecat worktrees use nested layout: polecats/<name>/<rigname>/
	polecat := filepath.Join(rigDir, "polecats", "pc1", "testrig")
	initGitRepo(t, gf, polecat)
	configureLegacySparseCheckout(t, gf, polecat)

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for legacy sparse checkout, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "3 repo(s) have legacy") {
		t.Errorf("expected message about 3 repos, got %q", result.Message)
	}
	if len(result.Details) != 3 {
		t.Errorf("expected 3 details, got %d", len(result.Details))
	}
}

func TestSparseCheckoutCheck_MixedRepos(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create mayor/rig with legacy sparse checkout
	mayorRig := filepath.Join(rigDir, "mayor", "rig")
	initGitRepo(t, gf, mayorRig)
	configureLegacySparseCheckout(t, gf, mayorRig)

	// Create crew/agent1 WITHOUT sparse checkout (clean)
	crewAgent := filepath.Join(rigDir, "crew", "agent1")
	initGitRepo(t, gf, crewAgent)

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for legacy sparse checkout, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "1 repo(s) have legacy") {
		t.Errorf("expected message about 1 legacy repo, got %q", result.Message)
	}
	if len(result.Details) != 1 || !strings.Contains(filepath.ToSlash(result.Details[0]), "mayor/rig") {
		t.Errorf("expected details to contain only mayor/rig, got %v", result.Details)
	}
}

func TestSparseCheckoutCheck_PolecatNestedWorktree(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Polecat worktrees use nested layout: polecats/<name>/<rigname>/
	polecatWorktree := filepath.Join(rigDir, "polecats", "pc1", rigName)
	initGitRepo(t, gf, polecatWorktree)
	configureLegacySparseCheckout(t, gf, polecatWorktree)

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for polecat nested worktree, got %v", result.Status)
	}
	if !strings.Contains(result.Message, "1 repo(s) have legacy") {
		t.Errorf("expected message about 1 legacy repo, got %q", result.Message)
	}
	if len(result.Details) != 1 || !strings.Contains(filepath.ToSlash(result.Details[0]), "polecats/pc1/"+rigName) {
		t.Errorf("expected details to contain polecats/pc1/%s, got %v", rigName, result.Details)
	}
}

func TestSparseCheckoutCheck_PolecatLegacyFlatLayout(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Legacy flat layout: polecats/<name>/ is the worktree directly
	polecatFlat := filepath.Join(rigDir, "polecats", "pc1")
	initGitRepo(t, gf, polecatFlat)
	configureLegacySparseCheckout(t, gf, polecatFlat)

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for polecat flat layout, got %v", result.Status)
	}
	if len(result.Details) != 1 || !strings.Contains(filepath.ToSlash(result.Details[0]), "polecats/pc1") {
		t.Errorf("expected details to contain polecats/pc1, got %v", result.Details)
	}
}

func TestSparseCheckoutCheck_Fix(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create git repos with legacy sparse checkout
	mayorRig := filepath.Join(rigDir, "mayor", "rig")
	initGitRepo(t, gf, mayorRig)
	configureLegacySparseCheckout(t, gf, mayorRig)

	crewAgent := filepath.Join(rigDir, "crew", "agent1")
	initGitRepo(t, gf, crewAgent)
	configureLegacySparseCheckout(t, gf, crewAgent)

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	// Verify fix is needed
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning before fix, got %v", result.Status)
	}

	// Apply fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify sparse checkout is now disabled
	for _, repo := range []string{mayorRig, crewAgent} {
		if v, _ := gf.OpenBranchRepo(repo).(Repo).ConfigGet("core.sparseCheckout"); v == "true" {
			t.Errorf("expected sparse checkout to be disabled for %s", repo)
		}
	}

	// Verify check now passes
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v", result.Status)
	}
}

func TestSparseCheckoutCheck_FixNoOp(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create git repo without sparse checkout (already clean)
	mayorRig := filepath.Join(rigDir, "mayor", "rig")
	initGitRepo(t, gf, mayorRig)

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	// Run check to populate state
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v", result.Status)
	}

	// Fix should be a no-op (no affected repos)
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Still OK
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after no-op fix, got %v", result.Status)
	}
}

func TestSparseCheckoutCheck_NonGitDirSkipped(t *testing.T) {
	t.Parallel()
	gf := gitfake.New()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigDir := filepath.Join(tmpDir, rigName)

	// Create non-git directories (should be skipped)
	if err := os.MkdirAll(filepath.Join(rigDir, "mayor", "rig"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, "crew", "agent1"), 0755); err != nil {
		t.Fatal(err)
	}

	check := NewSparseCheckoutCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, gf)

	result := check.Run(ctx)

	// Non-git dirs are skipped, so StatusOK
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no git repos, got %v", result.Status)
	}
}
