package doctor

import (
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// withGit points ctx's git at the gitfake world f.
func withGit(ctx *CheckContext, f *gitfake.Fake) *CheckContext {
	ctx.openGit = func(gitDir, workDir string) Repo { return f.OpenWithDir(gitDir, workDir).(Repo) }
	return ctx
}

var _ Repo = gitfake.PathRepo(nil)

// fakeRemote makes a bare repository at dir in f with one commit on main
// holding files (a README when nil), and returns dir.
func fakeRemote(t *testing.T, f *gitfake.Fake, dir string, files map[string]string) string {
	t.Helper()
	if files == nil {
		files = map[string]string{"README.md": "# " + filepath.Base(dir) + "\n"}
	}
	f.InitBare(t, dir)
	f.Commit(t, dir, "main", "initial", files)
	return dir
}

// fakeClone makes origin (a bare repository with main) and a clone of it at
// dir, and returns origin.
func fakeClone(t *testing.T, f *gitfake.Fake, dir string) string {
	t.Helper()
	origin := fakeRemote(t, f, dir+"-origin.git", nil)
	f.Clone(t, origin, dir)
	return origin
}

// fetchRemote adds remote name at url to the checkout at dir and fetches it.
func fetchRemote(t *testing.T, f *gitfake.Fake, dir, name, url string) {
	t.Helper()
	f.AddRemote(t, dir, name, url)
	if err := f.OpenBranchRepo(dir).Fetch(name); err != nil {
		t.Fatalf("fetch %s: %v", name, err)
	}
}
