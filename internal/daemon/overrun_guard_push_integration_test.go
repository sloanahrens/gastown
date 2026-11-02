//go:build integration

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestIntegrationOverrunPushNonFastForwardNothingUnique runs the refused-push
// check on real git: a local branch sitting at an old main commit, with origin
// holding a different history for the same branch name, pushes nothing origin
// lacks, so the push counts as done and the stop releases the seat instead of
// parking it (gt-ao7zh).
func TestIntegrationOverrunPushNonFastForwardNothingUnique(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunPushFixture(t, d.config.TownRoot)
	// The incident's shape: the local branch was never moved off the main
	// commit it was cut from, so its tip is an ancestor of origin's branch.
	f.seatBranchAt(t, "origin/main")

	fake := newOverrunFake(20*time.Minute, stopSeat)
	seams := fake.seams()
	seams.push = d.overrunPushBranch
	d.runOverrunGuard(seams)

	if !slices.Contains(fake.calls, "retire "+stopSeat.Rig+"/"+stopSeat.Polecat) {
		t.Errorf("calls = %q, want the seat retired: the refused push had nothing left to send", fake.calls)
	}
	if len(fake.handBackNotes) != 1 || strings.Contains(fake.handBackNotes[0], "exists only in the seat's worktree") {
		t.Errorf("hand-back notes = %q, want no note naming the branch as worktree-only", fake.handBackNotes)
	}
}

// TestIntegrationOverrunPushNonFastForwardLandedPatch runs the same check when
// the local branch holds a copy of origin's patch with a different commit id
// (a cherry-pick, as a rebase leaves one): ancestry cannot see it, and git
// cherry's "-" line is what makes the push count as done (gt-ao7zh).
func TestIntegrationOverrunPushNonFastForwardLandedPatch(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunPushFixture(t, d.config.TownRoot)
	f.seatBranchAt(t, "origin/main")
	mustRunGit(t, f.seat, "cherry-pick", f.branchSHA)
	if tip := mustRunGit(t, f.seat, "rev-parse", "HEAD"); tip == f.branchSHA {
		t.Fatalf("the cherry-pick reproduced origin's commit %s: the branch would be a fast-forward", tip)
	}

	if err := d.overrunPushBranch(stopSeat); err != nil {
		t.Fatalf("push = %v, want nil: origin's branch already holds the tip's patch", err)
	}
}

// TestIntegrationOverrunPushNonFastForwardUniqueWork keeps the divergent case
// that really carries unpushed work refused and paused (gt-ao7zh).
func TestIntegrationOverrunPushNonFastForwardUniqueWork(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunPushFixture(t, d.config.TownRoot)
	f.seatBranchAt(t, "origin/main")
	f.writeAndCommit(t, f.seat, "unique.txt", "work origin lacks\n")

	var diverged *overrunPushDivergedError
	if err := d.overrunPushBranch(stopSeat); !errors.As(err, &diverged) || diverged.OriginSHA != f.branchSHA {
		t.Fatalf("push = %v, want the divergence error at origin sha %s", err, f.branchSHA)
	}
}

// overrunPushFixture is a bare origin, a seed checkout that writes origin's
// history, and the seat's clone of origin at the path the stop resolves a
// polecat worktree to.
type overrunPushFixture struct {
	origin    string
	seed      string
	seat      string
	branchSHA string
}

// newOverrunPushFixture builds the fixture under townRoot: origin's main holds
// the base commit, and origin's branch for stopSeat holds one more commit the
// seat's clone fetches but does not contain on its own branch.
func newOverrunPushFixture(t *testing.T, townRoot string) *overrunPushFixture {
	t.Helper()
	dir := t.TempDir()
	f := &overrunPushFixture{
		origin: filepath.Join(dir, "origin.git"),
		seed:   filepath.Join(dir, "seed"),
		seat:   filepath.Join(townRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig),
	}
	mustRunGit(t, dir, "init", "--bare", f.origin)
	mustRunGit(t, f.origin, "symbolic-ref", "HEAD", "refs/heads/main")

	mustRunGit(t, dir, "init", f.seed)
	mustRunGit(t, f.seed, "config", "user.name", "gastown test")
	mustRunGit(t, f.seed, "config", "user.email", "test@example.invalid")
	f.writeAndCommit(t, f.seed, "base.txt", "base\n")
	mustRunGit(t, f.seed, "branch", "-M", "main")
	mustRunGit(t, f.seed, "remote", "add", "origin", f.origin)
	mustRunGit(t, f.seed, "push", "origin", "main")

	// The one commit origin's branch has above the base. Its date is fixed in
	// the past so a later cherry-pick of it cannot land on the same commit id
	// and turn the divergent push into a fast-forward.
	if err := os.WriteFile(filepath.Join(f.seed, "branch.txt"), []byte("pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, f.seed, "add", "branch.txt")
	mustRunGitDated(t, f.seed, "commit", "-m", "add branch.txt")
	mustRunGit(t, f.seed, "push", "origin", "HEAD:refs/heads/"+stopSeat.Branch)
	f.branchSHA = mustRunGit(t, f.seed, "rev-parse", "HEAD")
	mustRunGit(t, f.seed, "reset", "--hard", "HEAD~1")

	if err := os.MkdirAll(filepath.Dir(f.seat), 0o755); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "clone", f.origin, f.seat)
	mustRunGit(t, f.seat, "config", "user.name", "gastown test")
	mustRunGit(t, f.seat, "config", "user.email", "test@example.invalid")
	return f
}

// seatBranchAt puts the seat's local branch, under stopSeat's name, at start.
func (f *overrunPushFixture) seatBranchAt(t *testing.T, start string) {
	t.Helper()
	mustRunGit(t, f.seat, "checkout", "-b", stopSeat.Branch, start)
}

// mustRunGitDated commits in workDir with a date fixed in the past, so a later
// cherry-pick of that commit cannot reproduce its id.
func mustRunGitDated(t *testing.T, workDir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// writeAndCommit writes name in workDir and commits it.
func (f *overrunPushFixture) writeAndCommit(t *testing.T, workDir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, workDir, "add", name)
	mustRunGit(t, workDir, "commit", "-m", "add "+name)
}
