package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// cloneBehind lays out at clonePath a clone of its own origin that is behind
// commits behind origin/main. With ownWork the clone also holds a commit of its
// own.
func cloneBehind(t *testing.T, f *gitfake.Fake, clonePath string, behind int, ownWork bool) {
	t.Helper()
	origin := fakeRemote(t, f, filepath.Join(t.TempDir(), "origin.git"), nil)
	f.Clone(t, origin, clonePath)
	if ownWork {
		f.Commit(t, clonePath, "main", "local work", map[string]string{"local.txt": "mine\n"})
	}
	for i := 0; i < behind; i++ {
		f.Commit(t, origin, "main", fmt.Sprintf("advance %d", i),
			map[string]string{"README.md": fmt.Sprintf("# advance %d\n", i)})
	}
	if err := f.OpenBranchRepo(clonePath).Fetch("origin"); err != nil {
		t.Fatalf("fetch origin: %v", err)
	}
}

func TestCloneDivergenceCheck_StaleCrewCloneIsStillReported(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	cloneBehind(t, f, filepath.Join(townRoot, "devops", "crew", "operator"), 60, false)

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Status != StatusError {
		t.Fatalf("devops/crew/operator 60 behind: %v %v, want Error", result.Status, result.Details)
	}
}

// TestCloneDivergenceCheck_MayorCloneIsNotScanned: the check no longer looks at
// <rig>/mayor/rig at all, so a mayor/rig clone that is far behind, and holds
// commits of its own, is not in the report.
func TestCloneDivergenceCheck_MayorCloneIsNotScanned(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	cloneBehind(t, f, filepath.Join(townRoot, "devops", "mayor", "rig"), 60, true)

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Status != StatusOK || result.Message != "No clones found" {
		t.Errorf("devops/mayor/rig 60 behind with its own work: %v %q %v, want OK 'No clones found'",
			result.Status, result.Message, result.Details)
	}
}

// TestCloneDivergenceCheck_NoValidClonesMessageIsUnchanged pins the wording,
// which gt-9gnar changed without a test (gt-jx8sh). A crew clone whose git
// state cannot be read is skipped, which leaves no clone to report.
func TestCloneDivergenceCheck_NoValidClonesMessageIsUnchanged(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "devops", "crew", "operator", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Message != "No valid git clones found" {
		t.Errorf("message = %q, want %q", result.Message, "No valid git clones found")
	}
}
