package daemon

import (
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// Wiring guards: each test below leaves one seam nil and proves the
// production collaborator behind it actually ran. The rest of the package
// replaces these seams, so without a guard a seam whose nil path stopped
// calling the real thing would pass every other test.

// TestGitSeamDefaultsToRealGit guards the git seam's nil path: with no opener
// set, the daemon opens a *git.Git on the directory asked for.
func TestGitSeamDefaultsToRealGit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if g, ok := (&Daemon{}).gitAt(dir).(*git.Git); !ok || g.WorkDir() != dir {
		t.Errorf("Daemon.gitAt(%s) = %T; want a *git.Git on it", dir, (&Daemon{}).gitAt(dir))
	}
}
