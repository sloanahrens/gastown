package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestTownRootBranchCheck_NotAGitRepo(t *testing.T) {
	t.Parallel()
	// A non-git directory means the branch cannot be read — this is a
	// could-not-ask condition, not a verified "on main" state.
	result := NewTownRootBranchCheck().Run(withGit(&CheckContext{TownRoot: t.TempDir()}, gitfake.New()))

	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped for non-git dir, got %v: %s", result.Status, result.Message)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
	if len(result.Details) == 0 {
		t.Error("expected Details to carry the underlying error")
	}
}

// townRootOn is a town root clone checked out on branch (main needs nothing).
func townRootOn(t *testing.T, branch string) (*gitfake.Fake, *CheckContext) {
	t.Helper()
	f := gitfake.New()
	town := filepath.Join(t.TempDir(), "town")
	fakeClone(t, f, town)
	if branch != "main" {
		if err := f.OpenBranchRepo(town).CheckoutNewBranch(branch, "HEAD"); err != nil {
			t.Fatal(err)
		}
	}
	return f, withGit(&CheckContext{TownRoot: town}, f)
}

func TestTownRootBranchCheck_OnMain(t *testing.T) {
	t.Parallel()
	_, ctx := townRootOn(t, "main")
	if result := NewTownRootBranchCheck().Run(ctx); result.Status != StatusOK {
		t.Errorf("expected StatusOK on main branch, got %v: %s", result.Status, result.Message)
	}
}

func TestTownRootBranchCheck_WrongBranch(t *testing.T) {
	t.Parallel()
	f, ctx := townRootOn(t, "some-feature")
	check := NewTownRootBranchCheck()
	if result := check.Run(ctx); result.Status != StatusError || !strings.Contains(result.Message, "some-feature") {
		t.Fatalf("expected StatusError naming the branch, got %v: %s", result.Status, result.Message)
	}

	// Fix refuses while the town root has uncommitted changes, then switches.
	dirty := filepath.Join(ctx.TownRoot, "scratch.txt")
	if err := os.WriteFile(dirty, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := check.Fix(ctx); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("Fix with a dirty town root = %v, want a refusal", err)
	}
	if err := os.Remove(dirty); err != nil {
		t.Fatal(err)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if b, _ := f.OpenBranchRepo(ctx.TownRoot).CurrentBranch(); b != "main" {
		t.Errorf("after Fix the town root is on %q, want main", b)
	}
}

func TestTownRootBranchCheck_DetachedHead(t *testing.T) {
	t.Parallel()
	f, ctx := townRootOn(t, "main")
	if err := f.OpenBranchRepo(ctx.TownRoot).CheckoutDetachForce("HEAD"); err != nil {
		t.Fatal(err)
	}
	if result := NewTownRootBranchCheck().Run(ctx); result.Status != StatusWarning || !strings.Contains(result.Message, "detached") {
		t.Errorf("expected a detached-HEAD warning, got %v: %s", result.Status, result.Message)
	}
}
