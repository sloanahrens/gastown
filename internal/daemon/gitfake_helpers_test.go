package daemon

import (
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// useGitfake points d's git at a fresh gitfake world and returns it: every
// repository d opens is the world's, and nothing runs git.
func useGitfake(t *testing.T, d *Daemon) *gitfake.Fake {
	t.Helper()
	f := gitfake.New()
	d.openGitFn = func(dir string) daemonGit {
		g, ok := f.Open(dir).(daemonGit)
		if !ok {
			t.Fatalf("gitfake does not implement daemonGit for %s", dir)
		}
		return g
	}
	return f
}
