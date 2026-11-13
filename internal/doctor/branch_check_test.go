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

// TestCloneDivergenceCheck_TownSourceCheckoutsAreNotScanned: findAllClones returns
// neither <rig>/mayor/rig nor the town gt source checkout, although both are far
// behind and hold commits of their own. The gt source drift report is retired.
func TestCloneDivergenceCheck_TownSourceCheckoutsAreNotScanned(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	cloneBehind(t, f, filepath.Join(townRoot, "devops", "mayor", "rig"), 60, true)
	cloneBehind(t, f, filepath.Join(townRoot, ".runtime", "build-src", "gastown"), 60, true)

	clones := NewCloneDivergenceCheck().findAllClones(townRoot)

	if len(clones) != 0 {
		t.Errorf("findAllClones = %v, want none: mayor/rig and the town gt source are not scanned", clones)
	}
}

// TestCloneDivergenceCheck_CrewWithoutCloneReportsNoClones: a crew directory
// that holds no git clone is not a clone to check, so the check reports none.
func TestCloneDivergenceCheck_CrewWithoutCloneReportsNoClones(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "devops", "crew", "operator"), 0755); err != nil {
		t.Fatal(err)
	}

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Status != StatusOK || result.Message != "No clones found" {
		t.Errorf("crew dir with no clone: %v %q, want OK 'No clones found'", result.Status, result.Message)
	}
}

// TestCloneDivergenceCheck_UnreadableCloneIsSkipped: a crew clone whose git state
// cannot be read is skipped, not fatal. Its .git is a directory the fake
// repository does not hold, so reading its branch fails. The check then reports
// 'No valid git clones found', the wording gt-9gnar changed without a test (gt-jx8sh).
func TestCloneDivergenceCheck_UnreadableCloneIsSkipped(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "devops", "crew", "operator", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Status != StatusOK || result.Message != "No valid git clones found" {
		t.Errorf("unreadable clone: %v %q, want OK 'No valid git clones found'", result.Status, result.Message)
	}
}
