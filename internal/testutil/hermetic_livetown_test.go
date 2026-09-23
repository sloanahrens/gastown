package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
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

// TestHermeticHarnessRefusesLiveTownInProcess is the enforcement test for
// gt-dr664: with the harness's forbidden-root variable set, every in-process
// town-root resolver refuses the live town instead of returning it. Before
// this, beads.FindTownRoot walked up independently of workspace.Find, so a
// test binary running from a worktree inside the live town could open the
// production beads database and its Dolt server on :3307 in-process.
func TestHermeticHarnessRefusesLiveTownInProcess(t *testing.T) {
	town, inner := liveTownFixture(t)
	t.Setenv(workspace.EnvForbiddenTownRoot, town)

	for _, r := range liveTownResolvers {
		root, refusedLoudly := probeResolver(r.resolve, inner)
		if refusedLoudly {
			continue // refusal via panic: the loud path, and a pass
		}
		if root != "" {
			t.Errorf("%s resolved %q from %s; want refusal", r.name, root, inner)
		}
	}
}

// TestAssertLiveTownRefusedCatchesLeakingResolver pins the startup check
// itself: a resolver that ignores the guard must fail harness setup, not slip
// through because it is absent from liveTownResolvers.
func TestAssertLiveTownRefusedCatchesLeakingResolver(t *testing.T) {
	town, inner := liveTownFixture(t)
	t.Setenv(workspace.EnvForbiddenTownRoot, town)

	// Control: a resolver that refuses loudly passes the check.
	swapLiveTownResolvers(t, []liveTownResolver{
		{"loud", func(string) string { panic(workspace.ErrForbiddenTownRoot) }},
	})
	if err := assertLiveTownRefused(inner); err != nil {
		t.Fatalf("loud refusal must satisfy the check: %v", err)
	}

	// A resolver that returns the live root fails it, naming the resolver.
	swapLiveTownResolvers(t, []liveTownResolver{
		{"leaky", func(string) string { return town }},
	})
	err := assertLiveTownRefused(inner)
	if err == nil {
		t.Fatal("assertLiveTownRefused accepted a resolver that returned the live town")
	}
	if !strings.Contains(err.Error(), "leaky") || !strings.Contains(err.Error(), town) {
		t.Errorf("error must name the leaking resolver and the root it returned, got: %v", err)
	}

	// A resolver aimed outside the forbidden root is not a leak.
	swapLiveTownResolvers(t, []liveTownResolver{
		{"elsewhere", func(string) string { return t.TempDir() }},
	})
	if err := assertLiveTownRefused(inner); err != nil {
		t.Errorf("fixture town outside the forbidden root must pass: %v", err)
	}
}

// TestHermeticHarnessRefusesLiveTownWithoutSharingItsOwnProbe checks the other
// half of gt-dr664 independently of liveTownResolvers: the beads client itself
// must refuse to open a directory inside the live town, so no unlisted code
// path can reach production beads through the client.
func TestHermeticHarnessRefusesLiveTownWithoutSharingItsOwnProbe(t *testing.T) {
	town, inner := liveTownFixture(t)
	t.Setenv(workspace.EnvForbiddenTownRoot, town)

	if _, refused := probeResolver(func(string) string { return beads.FindTownRoot(town) }, town); !refused {
		t.Error("beads.FindTownRoot returned the live town root instead of refusing")
	}

	newClient := func(dir string) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("beads.New(%s) constructed a client on the live town", dir)
			}
		}()
		_ = beads.New(dir)
	}
	newClient(inner)
	newClient(filepath.Join(town, ".beads"))

	// A directory outside the forbidden root still constructs normally.
	_ = beads.New(t.TempDir())
}

// swapLiveTownResolvers replaces the probe list for one test and restores it.
func swapLiveTownResolvers(t *testing.T, list []liveTownResolver) {
	t.Helper()
	saved := liveTownResolvers
	liveTownResolvers = list
	t.Cleanup(func() { liveTownResolvers = saved })
}
