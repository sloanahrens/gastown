//go:build integration

package land

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The unit tier lands through gitfake. These run the same landings through
// real git: the clone, the throwaway worktree, the merge, the lease push and
// the read-back.

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeT(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRealLandFixture builds newLandFixture's shape with real git: a bare
// origin, an author's clone that pushes main and the branch, and the
// lander's clone.
func newRealLandFixture(t *testing.T) *landFixture {
	t.Helper()
	f := newLandFixtureAt(t)
	root := filepath.Dir(f.origin)
	seed := filepath.Join(root, "seed")
	gitT(t, root, "init", "-q", "--bare", "-b", "main", f.origin)
	gitT(t, root, "clone", "-q", f.origin, seed)
	gitT(t, seed, "config", "core.hooksPath", "/dev/null")
	gitT(t, seed, "checkout", "-q", "-b", "main")
	writeT(t, seed, "a.txt", "one\ntwo\nthree\n")
	gitT(t, seed, "add", "a.txt")
	gitT(t, seed, "commit", "-q", "-m", "main: seed")
	gitT(t, seed, "push", "-q", "origin", "main")
	gitT(t, seed, "checkout", "-q", "-b", fixtureBranch)
	writeT(t, seed, "b.txt", "work\n")
	gitT(t, seed, "add", "b.txt")
	gitT(t, seed, "commit", "-q", "-m", "feat: add b")
	gitT(t, seed, "push", "-q", "origin", fixtureBranch)
	head := gitT(t, seed, "rev-parse", "HEAD")
	gitT(t, seed, "checkout", "-q", "main")
	gitT(t, root, "clone", "-q", f.origin, f.repo)
	f.base = gitT(t, seed, "rev-parse", "origin/main")
	f.ready(head)
	f.realPushMain = func(name, body string) string {
		gitT(t, seed, "checkout", "-q", "main")
		gitT(t, seed, "pull", "-q", "--ff-only", "origin", "main")
		writeT(t, seed, name, body)
		gitT(t, seed, "add", name)
		gitT(t, seed, "commit", "-q", "-m", "main: "+name)
		gitT(t, seed, "push", "-q", "origin", "main")
		return gitT(t, seed, "rev-parse", "HEAD")
	}
	f.realOriginMain = func() string { return gitT(t, f.origin, "rev-parse", "refs/heads/main") }
	f.realParents = func(commit string) []string {
		return strings.Fields(gitT(t, f.origin, "rev-list", "--parents", "-n", "1", commit))[1:]
	}
	return f
}

func TestIntegrationLandMergesGatesPushesAndRecords(t *testing.T) {
	landsAndRecords(t, newRealLandFixture(t))
}

func TestIntegrationLandConflictIsARejectionWithFiles(t *testing.T) {
	conflictIsARejection(t, newRealLandFixture(t))
}
