package polecatimage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// testFullSHA is the commit the rig repo resolves a ref to, in the
	// 40-character form git prints.
	testFullSHA = "abc1234" + "0000000000000000000000000000000000000"
	// testShortSHA is what the log lines name it by.
	testShortSHA = "abc123400000"
)

// buildReply answers the calls BuildAt makes: git's three subcommands, make,
// and the docker label read that follows the build.
type buildReply struct {
	resolve, add, remove, build, label result
	// worktree, when set, is created when git is asked to make it, so a
	// fallback removal has something to remove.
	worktree string
	// onBuild runs while make does, which is how a test moves the clock.
	onBuild func()
}

// okBuild is a build that succeeds and leaves the label naming the commit.
func okBuild() buildReply {
	return buildReply{
		resolve: result{stdout: testFullSHA + "\n"},
		label:   result{stdout: testFullSHA + "\n"},
	}
}

func (b buildReply) answer(t *testing.T) func(command) result {
	return func(c command) result {
		switch {
		case c.name == "git" && hasArg(c.args, "rev-parse"):
			return b.resolve
		case c.name == "git" && hasArg(c.args, "add"):
			if b.worktree != "" {
				if err := os.MkdirAll(b.worktree, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			return b.add
		case c.name == "git" && hasArg(c.args, "remove"):
			return b.remove
		case c.name == "make":
			if b.onBuild != nil {
				b.onBuild()
			}
			return b.build
		case c.name == "docker":
			return b.label
		}
		return result{}
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func worktreePath(tr *testRun) string {
	return filepath.Join(tr.Cfg.BuildRoot, testFullSHA)
}

func TestBuildAtBuildsInADetachedWorktree(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	b := okBuild()
	b.onBuild = func() { tr.now = testNow.Add(42 * time.Second) }
	tr.reply = b.answer(t)
	if err := tr.BuildAt(testFullSHA); err != nil {
		t.Fatalf("BuildAt() failed: %v", err)
	}
	worktree := worktreePath(tr)
	git := tr.callsTo("git")
	if len(git) != 3 {
		t.Fatalf("git was called %d times, want 3: %v", len(git), git)
	}
	wantGit := [][]string{
		{"-C", tr.Cfg.RigRepo, "rev-parse", "--verify", testFullSHA + "^{commit}"},
		{"-C", tr.Cfg.RigRepo, "worktree", "add", "--detach", worktree, testFullSHA},
		{"-C", tr.Cfg.RigRepo, "worktree", "remove", "--force", worktree},
	}
	for i, want := range wantGit {
		if strings.Join(git[i].args, " ") != strings.Join(want, " ") {
			t.Errorf("git call %d = %q, want %q", i, git[i].args, want)
		}
	}
	makeCalls := tr.callsTo("make")
	if len(makeCalls) != 1 {
		t.Fatalf("make was called %d times, want 1", len(makeCalls))
	}
	if makeCalls[0].dir != worktree {
		t.Errorf("make ran in %q, want the worktree %q", makeCalls[0].dir, worktree)
	}
	if got := strings.Join(makeCalls[0].args, " "); got != "polecat-image BD_SRC="+tr.Cfg.BDSrc {
		t.Errorf("make ran with %q, want the build target and BD_SRC", got)
	}
	tr.wantLogged(t, "building gastown-polecat:dev at "+testShortSHA+" (BD_SRC="+tr.Cfg.BDSrc+")")
	tr.wantLogged(t, "build took 42s at "+testShortSHA)
	if tr.Worktree != "" {
		t.Errorf("Worktree = %q after a finished build, want empty", tr.Worktree)
	}
}

func TestBuildAtReportsAnUnknownCommit(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	b := okBuild()
	b.resolve = result{stderr: "fatal: bad revision 'nope^{commit}'\n", exit: 128}
	tr.reply = b.answer(t)
	err := tr.BuildAt("nope")
	want := "the installed commit nope is not in " + tr.Cfg.RigRepo + ": fatal: bad revision 'nope^{commit}'"
	if err == nil || err.Error() != want {
		t.Fatalf("BuildAt() error = %v, want %q", err, want)
	}
	tr.wantLogged(t, "ERROR: "+want)
	if calls := tr.callsTo("git"); len(calls) != 1 {
		t.Errorf("git was called %d times after an unknown commit, want only the resolve", len(calls))
	}
}

func TestBuildAtReportsAWorktreeItCannotMake(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	b := okBuild()
	b.add = result{stderr: "fatal: already registered\n", exit: 128}
	tr.reply = b.answer(t)
	err := tr.BuildAt(testFullSHA)
	want := "cannot make a worktree of " + testFullSHA + " under " + tr.Cfg.BuildRoot
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("BuildAt() error = %v, want %q", err, want)
	}
	tr.wantLogged(t, "ERROR: "+want)
	if makeCalls := tr.callsTo("make"); len(makeCalls) != 0 {
		t.Error("make ran without a worktree")
	}
}

func TestBuildAtReportsAFailedBuild(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	b := okBuild()
	b.worktree = worktreePath(tr)
	b.build = result{stdout: numberedLines(20), exit: 2}
	tr.reply = b.answer(t)
	err := tr.BuildAt(testFullSHA)
	want := "make polecat-image failed at " + testShortSHA
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("BuildAt() error = %v, want %q", err, want)
	}
	tr.wantLogged(t, "ERROR: "+want)
	tr.wantLogged(t, "build output line 20")
	if strings.Contains(tr.output(), "build output line 5\n") {
		t.Error("the log has the start of the build output, want only its tail")
	}
	if tr.Worktree != worktreePath(tr) {
		t.Errorf("Worktree = %q, want the worktree left for Cleanup", tr.Worktree)
	}
	if !exists(worktreePath(tr)) {
		t.Error("Cleanup has no worktree to remove: the failed build's directory is gone")
	}
}

func TestBuildAtReportsAnImageWithAnotherCommit(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	b := okBuild()
	b.label = result{stdout: "1111111\n"}
	tr.reply = b.answer(t)
	err := tr.BuildAt(testFullSHA)
	want := "after the build gastown-polecat:dev carries '1111111', not " + testShortSHA
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("BuildAt() error = %v, want %q", err, want)
	}
	tr.wantLogged(t, "ERROR: "+want)
}

func TestBuildAtReportsAnImageThatIsMissing(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	b := okBuild()
	b.label = result{exit: 1}
	tr.reply = b.answer(t)
	err := tr.BuildAt(testFullSHA)
	want := "after the build gastown-polecat:dev carries '', not " + testShortSHA
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("BuildAt() error = %v, want %q", err, want)
	}
	tr.wantLogged(t, "ERROR: "+want)
}

func TestBuildAtReportsABuildRootItCannotMake(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	if err := os.WriteFile(tr.Cfg.BuildRoot, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.reply = okBuild().answer(t)
	err := tr.BuildAt(testFullSHA)
	want := "cannot make " + tr.Cfg.BuildRoot + ": "
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("BuildAt() error = %v, want %q", err, want)
	}
	tr.wantLogged(t, "ERROR: "+want)
}

func TestBuildAtRemovesTheWorktreeGitCouldNot(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	b := okBuild()
	b.worktree = worktreePath(tr)
	b.remove = result{stderr: "fatal: not a working tree\n", exit: 128}
	tr.reply = b.answer(t)
	if err := tr.BuildAt(testFullSHA); err != nil {
		t.Fatalf("BuildAt() failed: %v", err)
	}
	if exists(worktreePath(tr)) {
		t.Error("the worktree git could not remove is still there")
	}
}

// numberedLines is n numbered lines, so a test can tell which end of a failed
// build's output survived the tail.
func numberedLines(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("build output line %d", i+1)
	}
	return strings.Join(lines, "\n") + "\n"
}
