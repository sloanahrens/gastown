//go:build integration

package land

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/checkpoint"
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
	// The merged commit exists in the lander's clone first; the fast-forward
	// has to carry the object with it.
	f.realSetMain = func(commit string) { gitT(t, f.repo, "push", "-q", "origin", commit+":refs/heads/main") }
	f.realOriginMain = func() string { return gitT(t, f.origin, "rev-parse", "refs/heads/main") }
	f.realRef = func(dir, ref string) string {
		out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", ref).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
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

// realAutoSaveOnBranch commits an auto-save checkpoint on the fixture's work
// branch and pushes it, so the range a landing brings to the target holds one
// and mergeWork squashes it away.
func realAutoSaveOnBranch(t *testing.T, f *landFixture) {
	t.Helper()
	seed := filepath.Join(filepath.Dir(f.origin), "seed")
	gitT(t, seed, "checkout", "-q", fixtureBranch)
	gitT(t, seed, "pull", "-q", "--ff-only", "origin", fixtureBranch)
	writeT(t, seed, "c.txt", "wip\n")
	gitT(t, seed, "add", "c.txt")
	gitT(t, seed, "commit", "-q", "-m", checkpoint.WIPCommitPrefix+" 2026-09-30")
	gitT(t, seed, "push", "-q", "origin", fixtureBranch)
	f.work.Head = gitT(t, seed, "rev-parse", "HEAD")
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

// TestIntegrationLandRecoversUnrecordedSquashLanding: the squash recovery over
// real git, where the patch a landing wrote and the patch of the head it
// merged are two diffs git patch-id has to agree on for the landing to be
// finished rather than landed a second time (gt-064oh).
func TestIntegrationLandRecoversUnrecordedSquashLanding(t *testing.T) {
	t.Parallel()
	f := newRealLandFixture(t)
	realAutoSaveOnBranch(t, f)
	recoversUnrecordedSquashLanding(t, f)
}

// TestIntegrationLandRecoversUnrecordedSquashLandingForkedBeforeItsBase: the
// same recovery over real git, where the merge base git reads and the two
// patch ids git patch-id has to agree on are its own — and the landing's base
// is ahead of the head's fork point, so the head's own change is not the diff
// between the two tips (gt-064oh).
func TestIntegrationLandRecoversUnrecordedSquashLandingForkedBeforeItsBase(t *testing.T) {
	t.Parallel()
	f := newRealLandFixture(t)
	realAutoSaveOnBranch(t, f)
	f.base = f.pushMain("d.txt", "someone else landed\n")
	merged := recoversUnrecordedSquashLanding(t, f)
	if got := f.parents(merged); len(got) != 1 || got[0] != f.base {
		t.Fatalf("the squash's parents = %v, want the later landing %s it merged onto", got, f.base)
	}
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

// TestIntegrationLandingCommitsAsTheAutomation: a real land's merge commit
// carries the landing committer name and the clone's configured email, while
// the author stays the range's and a GIT_COMMITTER_NAME inherited from the
// process environment does not win (gt-dc4y3).
func TestIntegrationLandingCommitsAsTheAutomation(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "Test Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Sloan Ahrens")
	f := newRealLandFixture(t)
	gitT(t, f.repo, "config", "user.name", "Configured User")
	gitT(t, f.repo, "config", "user.email", "configured@example.com")

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	got := gitT(t, f.origin, "show", "-s", "--format=%cn%n%ce%n%an%n%ae", res.LandedCommit)
	want := strings.Join([]string{LandingCommitterName, "configured@example.com", "Test Author", "author@example.com"}, "\n")
	if got != want {
		t.Fatalf("land commit identity:\n%s\nwant committer|email|author|email:\n%s", got, want)
	}
}
