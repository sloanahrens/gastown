//go:build integration

package testutil

import (
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/workspace"
)

// These set the process's own forbidden-root variable and cwd: they check
// the real in-process resolvers and ScratchTown's chdir, which read process
// state the unit tier's fake harness does not reach.

// TestIntegrationHermeticHarnessRefusesLiveTownInProcess is the enforcement test for
// gt-dr664: with the harness's forbidden-root variable set, every in-process
// town-root resolver refuses the live town instead of returning it. Before
// this, beads.FindTownRoot walked up independently of workspace.Find, so a
// test binary running from a worktree inside the live town could open the
// production beads database and its Dolt server on :3307 in-process.
func TestIntegrationHermeticHarnessRefusesLiveTownInProcess(t *testing.T) {
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

// TestIntegrationHermeticHarnessRefusesLiveTownWithoutSharingItsOwnProbe checks the other
// half of gt-dr664 independently of liveTownResolvers: the beads client itself
// must refuse to open a directory inside the live town, so no unlisted code
// path can reach production beads through the client.
func TestIntegrationHermeticHarnessRefusesLiveTownWithoutSharingItsOwnProbe(t *testing.T) {
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

func TestIntegrationScratchTownCwdResolvesToScratch(t *testing.T) {
	town := ScratchTown(t)

	root, err := workspace.FindFromCwd()
	if err != nil {
		t.Fatalf("FindFromCwd: %v", err)
	}
	// Resolve symlinks on both sides (macOS /var -> /private/var).
	wantReal, _ := filepath.EvalSymlinks(town)
	gotReal, _ := filepath.EvalSymlinks(root)
	if gotReal != wantReal {
		t.Errorf("FindFromCwd = %q, want scratch town %q", root, town)
	}
}
