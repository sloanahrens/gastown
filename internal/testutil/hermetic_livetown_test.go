package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/workspace"
)

// liveTownFixture builds a "live town" directory and returns its root and a
// directory below it, standing in for a test binary's cwd inside a worktree.
func liveTownFixture(t *testing.T) (town, inner string) {
	t.Helper()
	town = makeFakeTown(t)
	inner = filepath.Join(town, "gastown", "internal", "cmd")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	return town, inner
}

// TestAssertLiveTownRefusedCatchesLeakingResolver pins the startup check
// itself: a resolver that ignores the guard must fail harness setup, not slip
// through because it is absent from liveTownResolvers.
func TestAssertLiveTownRefusedCatchesLeakingResolver(t *testing.T) {
	t.Parallel()
	town, inner := liveTownFixture(t)
	f := newFakeHarness(t, workspace.EnvForbiddenTownRoot+"="+town)
	f.forbidden = func(root string) bool {
		return root == town || strings.HasPrefix(root, town+string(filepath.Separator))
	}

	// Control: a resolver that refuses loudly passes the check.
	f.resolvers = []liveTownResolver{{"loud", func(string) string { panic(workspace.ErrForbiddenTownRoot) }}}
	if err := f.assertLiveTownRefused(inner); err != nil {
		t.Fatalf("loud refusal must satisfy the check: %v", err)
	}

	// A resolver that returns the live root fails it, naming the resolver.
	f.resolvers = []liveTownResolver{{"leaky", func(string) string { return town }}}
	err := f.assertLiveTownRefused(inner)
	if err == nil {
		t.Fatal("assertLiveTownRefused accepted a resolver that returned the live town")
	}
	if !strings.Contains(err.Error(), "leaky") || !strings.Contains(err.Error(), town) {
		t.Errorf("error must name the leaking resolver and the root it returned, got: %v", err)
	}

	// A resolver aimed outside the forbidden root is not a leak.
	elsewhere := t.TempDir()
	f.resolvers = []liveTownResolver{{"elsewhere", func(string) string { return elsewhere }}}
	if err := f.assertLiveTownRefused(inner); err != nil {
		t.Errorf("fixture town outside the forbidden root must pass: %v", err)
	}
}
