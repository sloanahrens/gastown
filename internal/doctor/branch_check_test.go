package doctor

import (
	"fmt"
	"path/filepath"
	"strings"
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

func TestCloneDivergenceCheck_RetiredMayorCloneIsNotReported(t *testing.T) {
	t.Parallel()
	// Both bands the check has a voice in: a warning one and a failure one.
	for _, behind := range []int{20, 60} {
		t.Run(fmt.Sprintf("%d behind", behind), func(t *testing.T) {
			t.Parallel()
			f := gitfake.New()
			townRoot := t.TempDir()
			cloneBehind(t, f, filepath.Join(townRoot, "devops", "mayor", "rig"), behind, false)

			result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

			if result.Status != StatusOK {
				t.Errorf("devops/mayor/rig %d behind, no work of its own: %v %v, want OK",
					behind, result.Status, result.Details)
			}
		})
	}
}

func TestCloneDivergenceCheck_MayorCloneWithItsOwnWorkIsReported(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	cloneBehind(t, f, filepath.Join(townRoot, "devops", "mayor", "rig"), 60, true)

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Status != StatusError {
		t.Fatalf("devops/mayor/rig 60 behind and holding a commit of its own: %v %v, want Error",
			result.Status, result.Details)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "devops/mayor/rig") {
		t.Errorf("details = %v, want one naming devops/mayor/rig", result.Details)
	}
}

func TestCloneDivergenceCheck_GastownMayorCloneIsStillReported(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	cloneBehind(t, f, filepath.Join(townRoot, "gastown", "mayor", "rig"), 60, false)

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Status != StatusError {
		t.Fatalf("gastown/mayor/rig 60 behind: %v %v, want Error", result.Status, result.Details)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "gastown/mayor/rig") {
		t.Errorf("details = %v, want one naming gastown/mayor/rig", result.Details)
	}
}

func TestCloneDivergenceCheck_StaleCrewCloneIsStillReported(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	cloneBehind(t, f, filepath.Join(townRoot, "devops", "crew", "sloan"), 60, false)

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Status != StatusError {
		t.Fatalf("devops/crew/sloan 60 behind: %v %v, want Error", result.Status, result.Details)
	}
}
