package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The operator's crew clone and the linked topic worktree beside it. The
// fixture lays both out and the negative checks below assert neither is
// reported, so each name is one constant: the fixture and the needle cannot
// drift apart.
const (
	operatorClone    = "operator"
	operatorWorktree = "operator-topic"
)

// crewWithOperatorWorktree lays out a rig whose crew/ holds one crew clone
// (crew/operator, .git directory), one linked worktree of it beside it
// (crew/operator-topic, .git file), and the shared crew/.claude settings dir.
// Neither the worktree nor .claude has PRIME.md or a beads redirect.
func crewWithOperatorWorktree(t *testing.T) (townRoot, rigDir string) {
	t.Helper()
	townRoot = t.TempDir()
	rigDir = filepath.Join(townRoot, "myrig")
	for _, d := range []string{
		filepath.Join(rigDir, ".beads"),
		filepath.Join(rigDir, "crew", ".claude"),
		filepath.Join(rigDir, "crew", operatorClone, ".git"),
		filepath.Join(rigDir, "crew", operatorClone, ".beads"),
		filepath.Join(rigDir, "crew", operatorWorktree, ".beads"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(rigDir, ".beads", "PRIME.md"), "# prime\n")
	writeFile(t, filepath.Join(rigDir, "crew", operatorClone, ".beads", "redirect"), "../../.beads\n")
	writeFile(t, filepath.Join(rigDir, "crew", operatorWorktree, ".git"),
		"gitdir: "+filepath.Join(rigDir, "crew", operatorClone, ".git", "worktrees", operatorWorktree)+"\n")
	return townRoot, rigDir
}

func TestCrewCloneDirs_SkipsLinkedWorktreesAndDotDirs(t *testing.T) {
	t.Parallel()
	_, rigDir := crewWithOperatorWorktree(t)
	// A crew dir with no .git at all is still a crew member (half-made clone).
	if err := os.MkdirAll(filepath.Join(rigDir, "crew", "bare"), 0o755); err != nil {
		t.Fatal(err)
	}

	crewDir := filepath.Join(rigDir, "crew")
	got := crewCloneDirs(crewDir)
	want := []string{filepath.Join(crewDir, "bare"), filepath.Join(crewDir, operatorClone)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("crewCloneDirs = %v, want %v", got, want)
	}
}

func TestCrewCloneDirs_MissingDir(t *testing.T) {
	t.Parallel()
	if got := crewCloneDirs(filepath.Join(t.TempDir(), "nope")); len(got) != 0 {
		t.Errorf("crewCloneDirs(missing) = %v, want none", got)
	}
}

func TestCrewChecks_IgnoreOperatorWorktrees(t *testing.T) {
	t.Parallel()
	townRoot, rigDir := crewWithOperatorWorktree(t)
	ctx := &CheckContext{TownRoot: townRoot}

	for _, check := range []Check{primingCheck(), NewStaleBeadsRedirectCheck()} {
		result := check.Run(ctx)
		for _, d := range result.Details {
			if strings.Contains(d, operatorWorktree) {
				t.Errorf("%s reported the operator worktree: %q", check.Name(), d)
			}
		}
	}

	for _, p := range findRigClones(rigDir) {
		if filepath.Base(p) == operatorWorktree {
			t.Errorf("findRigClones returned the operator worktree %s", p)
		}
	}
	for _, p := range NewCloneDivergenceCheck().findAllClones(townRoot) {
		if filepath.Base(p) == operatorWorktree {
			t.Errorf("clone-divergence scanned the operator worktree %s", p)
		}
	}
}
