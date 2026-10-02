//go:build integration

package land

import (
	"context"
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

// The union test's seed files: list.txt is covered by merge=union in the
// seed's .gitattributes, plain.txt is not, so the same two appends must land
// clean on one and conflict on the other.
const (
	unionListSeed = "# A grow-only list for the union merge test.\n"
	plainListSeed = "# The same list with no union rule.\n"
)

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
	writeT(t, seed, ".gitattributes", "list.txt merge=union\n")
	writeT(t, seed, "list.txt", unionListSeed)
	writeT(t, seed, "plain.txt", plainListSeed)
	gitT(t, seed, "add", "a.txt", ".gitattributes", "list.txt", "plain.txt")
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

// appendT appends line to name in dir: one side of a landing's two appends.
func appendT(t *testing.T, dir, name, line string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// realAppendOnBranch commits line appended to name on the fixture's work
// branch and pushes it, moving the landing's head: the branch's half of the
// two appends a union merge has to reconcile.
func realAppendOnBranch(t *testing.T, f *landFixture, name, line string) {
	t.Helper()
	seed := filepath.Join(filepath.Dir(f.origin), "seed")
	gitT(t, seed, "checkout", "-q", fixtureBranch)
	gitT(t, seed, "pull", "-q", "--ff-only", "origin", fixtureBranch)
	appendT(t, seed, name, line)
	gitT(t, seed, "add", name)
	gitT(t, seed, "commit", "-q", "-m", "branch: append to "+name)
	gitT(t, seed, "push", "-q", "origin", fixtureBranch)
	f.work.Head = gitT(t, seed, "rev-parse", "HEAD")
}

// TestIntegrationLandUnionMergesTwoAppends: .gitattributes' merge=union rule
// lets main and the work branch each append to the same grow-only list, so
// the landing succeeds with both lines once. The control runs the same two
// appends on a file with no rule and expects RejectConflict naming it, which
// is what shows the rule, not luck, made the first landing clean.
func TestIntegrationLandUnionMergesTwoAppends(t *testing.T) {
	t.Parallel()

	f := newRealLandFixture(t)
	f.base = f.realPushMain("list.txt", unionListSeed+"main's line\n")
	realAppendOnBranch(t, f, "list.txt", "branch's line\n")
	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land with a union rule: %v", err)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want landed %s", got, res.LandedCommit)
	}
	landed := gitT(t, f.origin, "show", res.LandedCommit+":list.txt")
	for _, want := range []string{"grow-only list", "main's line", "branch's line"} {
		if n := strings.Count(landed, want); n != 1 {
			t.Errorf("%q appears %d times in the landed list.txt, want once:\n%s", want, n, landed)
		}
	}

	g := newRealLandFixture(t)
	g.base = g.realPushMain("plain.txt", plainListSeed+"main's line\n")
	realAppendOnBranch(t, g, "plain.txt", "branch's line\n")
	_, err = g.lander().Land(context.Background(), g.work)
	rej := g.assertRejected(t, err, RejectConflict, LabelRework)
	if len(rej.Conflicting) != 1 || rej.Conflicting[0] != "plain.txt" {
		t.Errorf("control conflicting = %v, want [plain.txt]", rej.Conflicting)
	}
}

func TestIntegrationLandMergesGatesPushesAndRecords(t *testing.T) {
	landsAndRecords(t, newRealLandFixture(t))
}

func TestIntegrationLandConflictIsARejectionWithFiles(t *testing.T) {
	conflictIsARejection(t, newRealLandFixture(t))
}

// TestIntegrationLandPastADeadLandingsWorktree: a landing that died before its
// cleanup leaves a registered git worktree at WorkRoot/wt. Every landing
// checks out at that one path (gt-2ycne.2), so the next must clear it, through
// real git, which refuses to add a worktree where one is registered.
func TestIntegrationLandPastADeadLandingsWorktree(t *testing.T) {
	t.Parallel()
	f := newRealLandFixture(t)
	if err := os.MkdirAll(f.workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(f.workRoot, "wt")
	gitT(t, f.repo, "worktree", "add", "-q", "--detach", dead, "origin/main")
	writeT(t, dead, "stale.txt", "x\n")
	landsAndRecords(t, f)
	if out := gitT(t, f.repo, "worktree", "list", "--porcelain"); strings.Contains(out, dead) {
		t.Errorf("the dead landing's worktree is still registered after the landing:\n%s", out)
	}
}
