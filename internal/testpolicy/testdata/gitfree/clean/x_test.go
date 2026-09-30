package clean

import (
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// A comment naming git.NewGit and exec.Command("git") is not a call.
func TestFake(t *testing.T) {
	t.Parallel()
	_ = gitfake.New()
	_ = "git"
}
