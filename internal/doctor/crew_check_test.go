package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// crewWithOperatorWorktree lays out a rig whose crew/ holds one crew clone
// (crew/sloan, .git directory), one linked worktree of it beside it
// (crew/sloan-topic, .git file), and the shared crew/.claude settings dir.
// Neither the worktree nor .claude has PRIME.md or a beads redirect.
func crewWithOperatorWorktree(t *testing.T) (townRoot, rigDir string) {
	t.Helper()
	townRoot = t.TempDir()
	rigDir = filepath.Join(townRoot, "myrig")
	for _, d := range []string{
		filepath.Join(rigDir, ".beads"),
		filepath.Join(rigDir, "crew", ".claude"),
		filepath.Join(rigDir, "crew", "sloan", ".git"),
		filepath.Join(rigDir, "crew", "sloan", ".beads"),
		filepath.Join(rigDir, "crew", "sloan-topic", ".beads"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(rigDir, ".beads", "PRIME.md"), "# prime\n")
	writeFile(t, filepath.Join(rigDir, "crew", "sloan", ".beads", "redirect"), "../../.beads\n")
	writeFile(t, filepath.Join(rigDir, "crew", "sloan-topic", ".git"),
		"gitdir: "+filepath.Join(rigDir, "crew", "sloan", ".git", "worktrees", "sloan-topic")+"\n")
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
	want := []string{filepath.Join(crewDir, "bare"), filepath.Join(crewDir, "sloan")}
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

	for _, check := range []Check{NewPrimingCheck(), NewStaleBeadsRedirectCheck()} {
		result := check.Run(ctx)
		for _, d := range result.Details {
			if strings.Contains(d, "sloan-topic") {
				t.Errorf("%s reported the operator worktree: %q", check.Name(), d)
			}
		}
	}

	for _, p := range findRigClones(rigDir) {
		if filepath.Base(p) == "sloan-topic" {
			t.Errorf("findRigClones returned the operator worktree %s", p)
		}
	}
	for _, p := range NewCloneDivergenceCheck().findAllClones(townRoot) {
		if filepath.Base(p) == "sloan-topic" {
			t.Errorf("clone-divergence scanned the operator worktree %s", p)
		}
	}
}
