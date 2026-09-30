package newgit

import (
	"testing"

	gogit "github.com/steveyegge/gastown/internal/git"
)

func TestNew(t *testing.T) {
	t.Parallel()
	_ = gogit.NewGit(t.TempDir())
	_ = gogit.NewGitWithDir("a", "b")
}
