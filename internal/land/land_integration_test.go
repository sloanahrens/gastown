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
	"github.com/steveyegge/gastown/internal/git"
)

// The unit tier lands through gitfake. These run the same landings through
// real git: the clone, the throwaway worktree, the merge, the lease push and
// the read-back.
//
// The integration tag selects this tier, so a run must carry it (gt-sjwr7a):
//
//	go test -tags integration ./internal/land -run TestIntegrationLandTrackedHookMerge -count=1

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitAs(t, dir, "Test User", "test@example.com", args...)
}

// gitAs is gitT with the author and committer set to name and email: the seat
// that did a branch's work.
func gitAs(t *testing.T, dir, name, email string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME="+name, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+email)
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
	gitAs(t, seed, "Seat Opal", "opal@gastown.local", "commit", "-q", "-m", "feat: add b")
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

// trackedHooksOnMain commits .githooks scripts that each exit 1 on main and
// pushes it, returning the new main tip: the local hooks a landing must not
// run. The beads rig tracks these (bd hooks) and, in a pod, bd is the gtwrap
// wrapper that refuses a call with no BEADS_DIR, so a merge that runs one ends
// "Not committing merge" and fails the landing (gt-sjwr7a, be-4sgb).
func trackedHooksOnMain(t *testing.T, f *landFixture) string {
	t.Helper()
	seed := filepath.Join(filepath.Dir(f.origin), "seed")
	gitT(t, seed, "checkout", "-q", "main")
	gitT(t, seed, "pull", "-q", "--ff-only", "origin", "main")
	if err := os.MkdirAll(filepath.Join(seed, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-commit", "post-merge", "prepare-commit-msg"} {
		rel := filepath.Join(".githooks", name)
		if err := os.WriteFile(filepath.Join(seed, rel), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		gitT(t, seed, "add", rel)
		gitT(t, seed, "update-index", "--chmod=+x", rel)
	}
	gitT(t, seed, "commit", "-q", "-m", "main: tracked hooks that exit 1")
	gitT(t, seed, "push", "-q", "origin", "main")
	return gitT(t, seed, "rev-parse", "HEAD")
}

// TestIntegrationLandTrackedHookMerge: the landing's merge runs with the
// target's local hooks off, so a hook the repository tracks cannot fail it.
// The worktree is cut from main, whose tree carries the tracked .githooks and
// whose clone points core.hooksPath at them; prepare-commit-msg exits 1, so a
// merge that ran it would end "Not committing merge" and never move the target
// (gt-sjwr7a, be-4sgb).
func TestIntegrationLandTrackedHookMerge(t *testing.T) {
	t.Parallel()

	f := newRealLandFixture(t)
	f.base = trackedHooksOnMain(t, f)
	gitT(t, f.repo, "config", "core.hooksPath", ".githooks")

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land in a repo whose tracked hooks each exit 1: %v", err)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want landed %s", got, res.LandedCommit)
	}
	if parents := f.parents(res.LandedCommit); len(parents) != 2 || parents[0] != f.base || parents[1] != f.work.Head {
		t.Fatalf("landed commit parents = %v, want [%s %s]", parents, f.base, f.work.Head)
	}
}

// TestIntegrationLandTrackedHookSquash: the landing's squash runs with the
// target's local hooks off too. The base tracks .githooks and the lander's
// clone points core.hooksPath at them, and the range carries an auto-save
// commit, so mergeWork squashes and commits on the target's tree; a
// prepare-commit-msg or pre-commit the repository tracks exits 1, so a squash
// that ran one would fail the landing and never move the target (gt-sjwr7a,
// be-4sgb).
func TestIntegrationLandTrackedHookSquash(t *testing.T) {
	t.Parallel()

	f := newRealLandFixture(t)
	f.base = trackedHooksOnMain(t, f)
	gitT(t, f.repo, "config", "core.hooksPath", ".githooks")
	realAutoSaveOnBranch(t, f)

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("squash landing in a repo whose tracked hooks each exit 1: %v", err)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want landed %s", got, res.LandedCommit)
	}
	if parents := f.parents(res.LandedCommit); len(parents) != 1 || parents[0] != f.base {
		t.Fatalf("landed commit parents = %v, want the squash's single parent %s", parents, f.base)
	}
	if got := gitT(t, f.origin, "show", res.LandedCommit+":c.txt"); got != "wip" {
		t.Errorf("landed c.txt = %q, want the auto-save's work", got)
	}
}

func TestIntegrationLandMergesGatesPushesAndRecords(t *testing.T) {
	landsAndRecords(t, newRealLandFixture(t))
}

// TestIntegrationRecordCarriesPriorityAndLandingRecord runs the priority and
// estimate record over real git, where the landing merges and pushes for real
// before the record is written (gt-nxn93f).
func TestIntegrationRecordCarriesPriorityAndLandingRecord(t *testing.T) {
	t.Parallel()
	recordCarriesPriorityAndEstimate(t, newRealLandFixture(t))
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

// TestIntegrationLandCommitIdentity: a real land's merge commit is committed by the landing
// (gt-landing, its own address) and authored by the head's seat, whatever the
// clone's git config and the process's GIT_* environment say. The ambient
// identity an inherited environment carries does not reach the commit
// (gt-dc4y3, gt-vuv2n).
func TestIntegrationLandCommitIdentity(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "Ambient Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "ambient@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test Operator")
	t.Setenv("GIT_COMMITTER_EMAIL", "operator@example.test")
	f := newRealLandFixture(t)
	gitT(t, f.repo, "config", "user.name", "Configured User")
	gitT(t, f.repo, "config", "user.email", "configured@example.com")

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	got := gitT(t, f.origin, "show", "-s", "--format=%cn%n%ce%n%an%n%ae", res.LandedCommit)
	want := strings.Join([]string{LandingCommitterName, LandingCommitterEmail, "Seat Opal", "opal@gastown.local"}, "\n")
	if got != want {
		t.Fatalf("land commit identity:\n%s\nwant committer|email|author|email:\n%s", got, want)
	}
}

// TestIntegrationHeadAuthorFailsClosed: the head's author is read from the head
// commit through real git, and a head that cannot be read is an error, never a
// fallback to git config (gt-vuv2n).
func TestIntegrationHeadAuthorFailsClosed(t *testing.T) {
	f := newRealLandFixture(t)
	l := f.lander()
	l.authorOf = nil
	g := git.NewGit(f.repo)

	t.Run("reads the seat that authored the head", func(t *testing.T) {
		got, err := l.headAuthor(g, f.work)
		if err != nil {
			t.Fatalf("headAuthor: %v", err)
		}
		if want := (Identity{Name: "Seat Opal", Email: "opal@gastown.local"}); got != want {
			t.Fatalf("headAuthor = %+v, want %+v", got, want)
		}
	})
	t.Run("unreadable head is an error", func(t *testing.T) {
		if _, err := l.headAuthor(g, Work{Head: "refs/heads/no-such-branch"}); err == nil {
			t.Fatal("headAuthor over an unreadable head = nil error, want one")
		}
	})
}

// prePushHookOnMain commits a tracked .githooks/pre-push that runs body as the
// new main tip and points the lander's clone (and the worktrees cut from it)
// at .githooks. The rigs track their hooks there — the beads rig's pre-push
// chains `bd hooks run pre-push` — and in a pod bd refuses a call with no
// BEADS_DIR (gt-xg33gs, be-4sgb).
func prePushHookOnMain(t *testing.T, f *landFixture, body string) string {
	t.Helper()
	seed := filepath.Join(filepath.Dir(f.origin), "seed")
	if err := os.MkdirAll(filepath.Join(seed, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(".githooks", "pre-push")
	if err := os.WriteFile(filepath.Join(seed, rel), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", rel)
	gitT(t, seed, "update-index", "--chmod=+x", rel)
	gitT(t, seed, "commit", "-q", "-m", "main: tracked pre-push")
	gitT(t, seed, "push", "-q", "origin", "main")
	gitT(t, f.repo, "fetch", "-q", "origin")
	gitT(t, f.repo, "config", "core.hooksPath", ".githooks")
	return gitT(t, seed, "rev-parse", "HEAD")
}

// pushCandidate pushes the fixture's head as branch through the handle the
// lander opens at a worktree cut from base: the push the candidate gate makes.
func pushCandidate(t *testing.T, l *Lander, f *landFixture, branch string) error {
	t.Helper()
	dir := filepath.Join(f.workRoot, "wt")
	gitT(t, f.repo, "worktree", "add", "-q", "--detach", dir, f.base)
	t.Cleanup(func() {
		if err := git.NewGit(f.repo).WorktreeRemove(dir, true); err != nil {
			t.Errorf("removing the test worktree: %v", err)
		}
	})
	return (&CandidateGate{}).push(l.openLanding(dir, nil), branch, f.work.Head)
}

// TestIntegrationLandBeadsDirReachesPrePush: the candidate handle the landing
// opens carries the rig's BEADS_DIR, so the tracked pre-push that runs bd finds
// its store and the candidate push passes; with no BEADS_DIR the same hook
// refuses that push (gt-xg33gs). The empty case also proves the hook runs at
// all, so the first case is not green because the hook was never invoked.
//
// The test pins BEADS_DIR empty itself rather than leaning on the hermetic
// harness to scrub it, so neither case reads the caller's environment: an
// empty value is unset to the hook's `[ -n "$BEADS_DIR" ]`, and the handle's
// own entry lands after the process's, where git keeps the last one.
func TestIntegrationLandBeadsDirReachesPrePush(t *testing.T) {
	t.Setenv("BEADS_DIR", "")
	f := newRealLandFixture(t)
	f.base = prePushHookOnMain(t, f, "#!/bin/sh\n[ -n \"$BEADS_DIR\" ] || exit 1\n")
	l := f.lander()

	t.Run("a configured BeadsDir reaches the hook", func(t *testing.T) {
		l.BeadsDir = filepath.Join(f.town, ".beads")
		if err := pushCandidate(t, l, f, "land/gt-abc"); err != nil {
			t.Fatalf("candidate push with BeadsDir set: %v", err)
		}
	})
	t.Run("no BeadsDir leaves the hook to refuse the push", func(t *testing.T) {
		l.BeadsDir = ""
		if err := pushCandidate(t, l, f, "land/gt-def"); err == nil {
			t.Fatal("candidate push with no BeadsDir = nil error, want the pre-push hook's refusal")
		}
	})
}

// TestIntegrationLandBeadsDirKeepsThePrePushFence: a tracked pre-push that
// refuses every push still refuses it with BeadsDir set. The landing exports
// BEADS_DIR to its handles; it never switches the fence off (gt-xg33gs,
// gt-sjwr7a). BEADS_DIR is pinned empty first so the case cannot inherit one
// from the caller (gt-xg33gs).
func TestIntegrationLandBeadsDirKeepsThePrePushFence(t *testing.T) {
	t.Setenv("BEADS_DIR", "")
	f := newRealLandFixture(t)
	f.base = prePushHookOnMain(t, f, "#!/bin/sh\nexit 1\n")
	l := f.lander()
	l.BeadsDir = filepath.Join(f.town, ".beads")
	if err := pushCandidate(t, l, f, "land/gt-abc"); err == nil {
		t.Fatal("candidate push under a pre-push that always exits 1 = nil error, want the hook's refusal")
	}
}

// TestIntegrationLandBeadsDirReachesTheReadHandle: the handle the landing's
// reads and refs/landed push are made through carries BEADS_DIR too, so the
// tracked pre-push passes there; with none it refuses (gt-xg33gs). BEADS_DIR
// is pinned empty first, for the same reason as the candidate test.
func TestIntegrationLandBeadsDirReachesTheReadHandle(t *testing.T) {
	t.Setenv("BEADS_DIR", "")
	f := newRealLandFixture(t)
	prePushHookOnMain(t, f, "#!/bin/sh\n[ -n \"$BEADS_DIR\" ] || exit 1\n")
	gitT(t, f.repo, "pull", "-q", "--ff-only", "origin", "main")
	l := f.lander()

	t.Run("a configured BeadsDir reaches the hook", func(t *testing.T) {
		l.BeadsDir = filepath.Join(f.town, ".beads")
		if err := l.open(f.repo).Push("origin", f.work.Head+":refs/landed/gt-abc", false); err != nil {
			t.Fatalf("push through the read handle with BeadsDir set: %v", err)
		}
	})
	t.Run("no BeadsDir leaves the hook to refuse the push", func(t *testing.T) {
		l.BeadsDir = ""
		if err := l.open(f.repo).Push("origin", f.work.Head+":refs/landed/gt-def", false); err == nil {
			t.Fatal("push through the read handle with no BeadsDir = nil error, want the pre-push hook's refusal")
		}
	})
}
