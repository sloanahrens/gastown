package crewfresh

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// testDir stands in for a clone path: the commands a unit test drives must
// name it in -C and never touch a real repository.
const testDir = "/crew/clone"

// fakeCommand is a git command a test scripts: it answers from the fields the
// case set and records every argv it was given.
type fakeCommand struct {
	stdout string
	err    error
	argvs  [][]string
}

func (f *fakeCommand) run(_ context.Context, argv []string) (string, error) {
	f.argvs = append(f.argvs, argv)
	return f.stdout, f.err
}

// exitError is a command failure that exited with a status, which is what git
// merge-base reports the answer "not an ancestor" as.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (e exitError) ExitCode() int { return int(e) }

// scriptedGit is the Git over a scripted command, so a unit test reaches both
// the answers and the failures without starting a process.
func scriptedGit(f *fakeCommand) Git {
	return &gitExec{dir: testDir, command: f.run}
}

// wantArgv fails the test unless argv is the one git command the method was
// meant to run.
func wantArgv(t *testing.T, f *fakeCommand, want ...string) {
	t.Helper()
	if len(f.argvs) != 1 {
		t.Fatalf("ran %d git commands, want 1: %v", len(f.argvs), f.argvs)
	}
	if !slices.Equal(f.argvs[0], want) {
		t.Errorf("argv = %v, want %v", f.argvs[0], want)
	}
}

// Failures a case drives through every method: the error each one must return
// when git refuses.
var commandFailures = []struct {
	name    string
	command *fakeCommand
	call    func(Git) error
	wantArg []string
}{
	{
		name:    "fetch",
		command: &fakeCommand{err: errors.New("no such remote")},
		call:    func(g Git) error { return g.Fetch(time.Minute) },
		wantArg: []string{"git", "-C", testDir, "fetch", "origin", "main"},
	},
	{
		name:    "branch",
		command: &fakeCommand{err: errors.New("not a git repository")},
		call:    func(g Git) error { _, err := g.Branch(); return err },
		wantArg: []string{"git", "-C", testDir, "rev-parse", "--abbrev-ref", "HEAD"},
	},
	{
		name:    "clean",
		command: &fakeCommand{err: errors.New("index unreadable")},
		call:    func(g Git) error { _, err := g.Clean(); return err },
		wantArg: []string{"git", "-C", testDir, "status", "--porcelain"},
	},
	{
		name:    "behind",
		command: &fakeCommand{err: errors.New("origin/main is not a commit")},
		call:    func(g Git) error { _, err := g.Behind(); return err },
		wantArg: []string{"git", "-C", testDir, "rev-list", "--count", "HEAD..origin/main"},
	},
	{
		name:    "ancestor",
		command: &fakeCommand{err: exitError(2)},
		call:    func(g Git) error { _, err := g.HeadIsAncestorOfOriginMain(); return err },
		wantArg: []string{"git", "-C", testDir, "merge-base", "--is-ancestor", "HEAD", "origin/main"},
	},
	{
		name:    "fast-forward",
		command: &fakeCommand{err: errors.New("not possible to fast-forward")},
		call:    func(g Git) error { return g.FastForwardMain() },
		wantArg: []string{"git", "-C", testDir, "merge", "--ff-only", "origin/main"},
	},
}

// A git command that fails is an error at every method, so no caller reads a
// refusal as an answer (gt-vpj84).
func TestGitMethodsReturnTheCommandFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range commandFailures {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(scriptedGit(tc.command)); err == nil {
				t.Fatal("method = nil error, want the command's failure")
			}
			wantArgv(t, tc.command, tc.wantArg...)
		})
	}
}

// Fetch asks origin for main under the caller's timeout (gt-vpj84).
func TestGitFetchAsksOriginForMain(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{}
	if err := scriptedGit(command).Fetch(30 * time.Second); err != nil {
		t.Fatalf("Fetch = error %v, want nil", err)
	}
	wantArgv(t, command, "git", "-C", testDir, "fetch", "origin", "main")
}

// Fetch reports its deadline when it runs out, rather than the git error the
// kill leaves behind (gt-vpj84).
func TestGitFetchReportsTheDeadlineWhenItRunsOut(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{err: errors.New("signal: killed")}

	err := scriptedGit(command).Fetch(0)
	if err == nil {
		t.Fatal("Fetch with an expired deadline = nil error, want the deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Fetch error = %q, want it to wrap context.DeadlineExceeded", err)
	}
}

// Branch is the checked-out branch (gt-vpj84).
func TestGitBranchReadsTheCheckedOutBranch(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{stdout: "main\n"}

	branch, err := scriptedGit(command).Branch()
	if err != nil {
		t.Fatalf("Branch = error %v, want nil", err)
	}
	if branch != mainBranch {
		t.Errorf("Branch = %q, want %q", branch, mainBranch)
	}
	wantArgv(t, command, "git", "-C", testDir, "rev-parse", "--abbrev-ref", "HEAD")
}

// A detached HEAD is no branch to Run, which rev-parse prints as "HEAD"
// (gt-vpj84).
func TestGitBranchReadsADetachedHeadAsNoBranch(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{stdout: "HEAD\n"}

	branch, err := scriptedGit(command).Branch()
	if err != nil {
		t.Fatalf("Branch = error %v, want nil", err)
	}
	if branch != "" {
		t.Errorf("Branch = %q, want \"\"", branch)
	}
}

// An empty porcelain status is a clean clone of the one branch it reports
// (gt-vpj84).
func TestGitCleanReadsAnEmptyStatusAsClean(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{}

	clean, err := scriptedGit(command).Clean()
	if err != nil {
		t.Fatalf("Clean = error %v, want nil", err)
	}
	if !clean {
		t.Error("Clean = false, want true for empty porcelain output")
	}
	wantArgv(t, command, "git", "-C", testDir, "status", "--porcelain")
}

// Any porcelain output is a change, and the clone is not clean (gt-vpj84).
func TestGitCleanReadsPorcelainOutputAsChanges(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{stdout: " M readme.txt\n?? scratch.txt\n"}

	clean, err := scriptedGit(command).Clean()
	if err != nil {
		t.Fatalf("Clean = error %v, want nil", err)
	}
	if clean {
		t.Error("Clean = true, want false when porcelain output is not empty")
	}
}

// Behind is the count rev-list prints (gt-vpj84).
func TestGitBehindReadsTheCount(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{stdout: "3\n"}

	behind, err := scriptedGit(command).Behind()
	if err != nil {
		t.Fatalf("Behind = error %v, want nil", err)
	}
	if behind != 3 {
		t.Errorf("Behind = %d, want 3", behind)
	}
	wantArgv(t, command, "git", "-C", testDir, "rev-list", "--count", "HEAD..origin/main")
}

// A count git did not print is an unread error, not the zero it parses to
// (gt-vpj84).
func TestGitBehindRejectsANonNumberCount(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{stdout: "not a count\n"}

	if _, err := scriptedGit(command).Behind(); err == nil {
		t.Fatal("Behind with a non-number count = nil error, want an error")
	}
}

// Exit 0 from merge-base --is-ancestor is the answer "yes" (gt-vpj84).
func TestGitAncestorIsTrueWhenGitExitsZero(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{}

	ancestor, err := scriptedGit(command).HeadIsAncestorOfOriginMain()
	if err != nil {
		t.Fatalf("HeadIsAncestorOfOriginMain = error %v, want nil", err)
	}
	if !ancestor {
		t.Error("HeadIsAncestorOfOriginMain = false, want true on exit 0")
	}
	wantArgv(t, command, "git", "-C", testDir, "merge-base", "--is-ancestor", "HEAD", "origin/main")
}

// Exit 1 from merge-base --is-ancestor is the answer "no", not a failure
// (gt-vpj84).
func TestGitAncestorReadsExitOneAsNo(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{err: exitError(1)}

	ancestor, err := scriptedGit(command).HeadIsAncestorOfOriginMain()
	if err != nil {
		t.Fatalf("HeadIsAncestorOfOriginMain = error %v, want nil", err)
	}
	if ancestor {
		t.Error("HeadIsAncestorOfOriginMain = true, want false on exit 1")
	}
}

// FastForwardMain moves main with a fast-forward merge and nothing else
// (gt-vpj84).
func TestGitFastForwardMergesOriginMainFastForwardOnly(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{}

	if err := scriptedGit(command).FastForwardMain(); err != nil {
		t.Fatalf("FastForwardMain = error %v, want nil", err)
	}
	wantArgv(t, command, "git", "-C", testDir, "merge", "--ff-only", "origin/main")
}

// A finished process that failed reports the failure and none of the output
// it managed to print, so partial output never passes as an answer
// (gt-vpj84).
func TestGitOutputReportsTheProcessFailure(t *testing.T) {
	t.Parallel()
	failure := errors.New("exit status 128")

	out, err := gitOutput([]byte("partial answer\n"), failure)
	if !errors.Is(err, failure) {
		t.Errorf("gitOutput = error %v, want the process's failure", err)
	}
	if out != "" {
		t.Errorf("gitOutput = %q, want \"\": a failed process's output is not an answer", out)
	}
}

// A finished process that succeeded reports what it printed (gt-vpj84).
func TestGitOutputReportsStdoutOnSuccess(t *testing.T) {
	t.Parallel()

	out, err := gitOutput([]byte("main\n"), nil)
	if err != nil {
		t.Fatalf("gitOutput = error %v, want nil", err)
	}
	if out != "main\n" {
		t.Errorf("gitOutput = %q, want %q", out, "main\n")
	}
}

// A failed command's partial output is not an answer, so run returns none of
// it (gt-vpj84).
func TestGitRunReturnsNoOutputWithAnError(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{stdout: "partial answer\n", err: errors.New("boom")}

	out, err := (&gitExec{dir: testDir, command: command.run}).run(context.Background(), "status", "--porcelain")
	if err == nil {
		t.Fatal("run = nil error, want the command's failure")
	}
	if out != "" {
		t.Errorf("run = %q, want \"\": a failed command's output is not an answer", out)
	}
}

// A failure names the command that failed and wraps the failure, so a caller
// can tell a missing remote from a lock with errors.Is (gt-vpj84).
func TestGitCommandErrorNamesTheCommandAndWrapsTheFailure(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{err: errors.New("boom")}

	err := scriptedGit(command).FastForwardMain()
	if err == nil {
		t.Fatal("FastForwardMain = nil error, want one")
	}
	if want := "git merge --ff-only origin/main"; !strings.Contains(err.Error(), want) {
		t.Errorf("FastForwardMain error = %q, want it to name %q", err, want)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("FastForwardMain error = %q, want it to wrap the failure", err)
	}
}
