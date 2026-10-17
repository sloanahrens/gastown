package doctor

import (
	"fmt"
	"os"
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

// townSourceCheckout marks clonePath as a gt source checkout: it writes the
// marker version.hasGtSource reads, which is what makes a clone the tree the
// town builds from.
func townSourceCheckout(t *testing.T, clonePath string) {
	t.Helper()
	dir := filepath.Join(clonePath, "cmd", "gt")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCloneDivergenceCheck_TownSourceCheckoutIsStillReported(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	source := filepath.Join(townRoot, "gastown", "mayor", "rig")
	cloneBehind(t, f, source, 60, false)
	townSourceCheckout(t, source)

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Status != StatusError {
		t.Fatalf("the town's source checkout 60 behind: %v %v, want Error", result.Status, result.Details)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "gastown/mayor/rig") {
		t.Errorf("details = %v, want one naming gastown/mayor/rig", result.Details)
	}
}

// TestCloneDivergenceCheck_RetiredGastownCloneIsNotReported: the exemption
// follows what a clone holds, not the name of its rig, so a gastown clone
// holding no gt source is the same retired leftover as any other rig's
// (gt-jx8sh).
func TestCloneDivergenceCheck_RetiredGastownCloneIsNotReported(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	cloneBehind(t, f, filepath.Join(townRoot, "gastown", "mayor", "rig"), 60, false)

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Status != StatusOK {
		t.Errorf("gastown/mayor/rig 60 behind, holding no gt source and no work of its own: %v %v, want OK",
			result.Status, result.Details)
	}
}

// TestCloneDivergenceCheck_NoValidClonesMessageIsUnchanged pins the wording,
// which gt-9gnar changed without a test (gt-jx8sh).
func TestCloneDivergenceCheck_NoValidClonesMessageIsUnchanged(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	townRoot := t.TempDir()
	// The town's only clone is a retired one, so the check exempts it and has
	// nothing left to report.
	cloneBehind(t, f, filepath.Join(townRoot, "devops", "mayor", "rig"), 60, false)

	result := NewCloneDivergenceCheck().Run(withGit(&CheckContext{TownRoot: townRoot}, f))

	if result.Message != "No valid git clones found" {
		t.Errorf("message = %q, want %q", result.Message, "No valid git clones found")
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
