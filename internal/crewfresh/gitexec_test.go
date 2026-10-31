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

// errHungPastBound is what a hung fake reports when no deadline could end it.
var errHungPastBound = errors.New("the call ran without a deadline that could end it")

// fakeCommand is a git command a test scripts: it answers from the fields the
// case set and records every argv it was given.
type fakeCommand struct {
	stdout string
	err    error
	argvs  [][]string
	// bounded records, per command, whether it ran under a deadline.
	bounded []bool
	// live records, per command, whether its context was still open when the
	// command started.
	live []bool
	// hang makes the command wait for its context to end, as a hung git does.
	hang bool
}

func (f *fakeCommand) run(ctx context.Context, argv []string) (string, error) {
	f.argvs = append(f.argvs, argv)
	_, hasDeadline := ctx.Deadline()
	f.bounded = append(f.bounded, hasDeadline)
	f.live = append(f.live, ctx.Err() == nil)
	if f.hang {
		// A context with no deadline can never end this wait, so the fake
		// reports that at once instead of waiting for good: a call that lost
		// its bound fails its test, it does not hang it.
		if _, ok := ctx.Deadline(); !ok {
			return "", errHungPastBound
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
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
	return &gitExec{dir: testDir, command: f.run, localTimeout: time.Millisecond}
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

// Every method runs its git command under a deadline, so no call can stall the
// caller on a hung git: the fetch under its caller's timeout and each other
// call under localTimeout (gt-vpj84).
func TestGitEveryCallRunsUnderADeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range commandFailures {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			command := &fakeCommand{err: tc.command.err}
			_ = tc.call(scriptedGit(command))
			if len(command.bounded) != 1 || !command.bounded[0] {
				t.Errorf("ran under a deadline = %v, want one command run under one", command.bounded)
			}
		})
	}
}

// A git that never answers ends at the deadline with the deadline as the
// error, at every method: the context ends the wait, not a clock the test
// reads (gt-vpj84).
func TestGitHungCallEndsWithTheDeadline(t *testing.T) {
	t.Parallel()
	hung := &gitExec{dir: testDir, command: (&fakeCommand{hang: true}).run, localTimeout: time.Millisecond}
	calls := map[string]func() error{
		// The fetch takes its own timeout from the caller.
		"fetch":        func() error { return hung.Fetch(time.Millisecond) },
		"branch":       func() error { _, err := hung.Branch(); return err },
		"clean":        func() error { _, err := hung.Clean(); return err },
		"behind":       func() error { _, err := hung.Behind(); return err },
		"ancestor":     func() error { _, err := hung.HeadIsAncestorOfOriginMain(); return err },
		"fast-forward": hung.FastForwardMain,
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := call(); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("hung call error = %v, want it to wrap context.DeadlineExceeded", err)
			}
		})
	}
}

// An unset localTimeout is the package default, never no deadline (gt-vpj84).
func TestGitLocalCallsDefaultToTheLocalTimeout(t *testing.T) {
	t.Parallel()
	command := &fakeCommand{}
	if _, err := (&gitExec{dir: testDir, command: command.run}).Branch(); err != nil {
		t.Fatal(err)
	}
	if len(command.bounded) != 1 || !command.bounded[0] {
		t.Errorf("ran under a deadline = %v, want one command run under one", command.bounded)
	}
	// A zero timeout is a deadline that has already passed, which would fail
	// every call before git started.
	if len(command.live) != 1 || !command.live[0] {
		t.Errorf("deadline open when the command started = %v, want open", command.live)
	}
}

// git is told never to stop and ask: no terminal prompt, no askpass helper and
// no ssh question, and an ssh command the caller chose is left alone
// (gt-vpj84).
func TestGitEnvTurnsEveryPromptOff(t *testing.T) {
	t.Parallel()
	base := []string{"HOME=/h", "GIT_TERMINAL_PROMPT=1"}
	got := gitEnv(base)
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"} {
		if !slices.Contains(got, want) {
			t.Errorf("env lacks %q: %v", want, got)
		}
	}
	if !slices.Equal(base, []string{"HOME=/h", "GIT_TERMINAL_PROMPT=1"}) {
		t.Errorf("gitEnv changed its input: %v", base)
	}
	own := gitEnv([]string{"GIT_SSH_COMMAND=ssh -i key"})
	if slices.Contains(own, "GIT_SSH_COMMAND=ssh -o BatchMode=yes") || !slices.Contains(own, "GIT_SSH_COMMAND=ssh -i key") {
		t.Errorf("env replaced the caller's ssh command: %v", own)
	}
}

// Git's standard error becomes a fixed reason of this package's wording, and
// text outside the table becomes none (gt-vpj84).
func TestGitReasonNamesOnlyKnownPhrases(t *testing.T) {
	t.Parallel()
	cases := []struct{ stderr, want string }{
		{"fatal: could not read Username for 'https://x@host': terminal prompts disabled", "authentication required"},
		{"fatal: unable to access 'https://host/r.git/': Could not resolve host: host", "remote unreachable"},
		{"fatal: couldn't find remote ref main", "remote ref not found"},
		{"fatal: Not possible to fast-forward, aborting.", "not a fast-forward"},
		{"fatal: Unable to create '/r/.git/index.lock': File exists.", "the index is locked by another git process"},
		{"fatal: not a git repository (or any parent): .git", "not a git repository"},
		{"fatal: something git has never said", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := gitReason([]byte(tc.stderr)); got != tc.want {
			t.Errorf("gitReason(%q) = %q, want %q", tc.stderr, got, tc.want)
		}
	}
}

// A reasoned failure names the reason and nothing of git's text, and still
// answers errors.As for the exit status (gt-vpj84).
func TestGitWithReasonKeepsTheFailureAndOmitsGitsText(t *testing.T) {
	t.Parallel()
	const secret = "token-do-not-print-4f2a"
	err := withReason(exitError(128), []byte("fatal: Authentication failed for 'https://"+secret+"@host/r.git'"))
	if want := "exit status 128 (authentication required)"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error repeats git's text: %q", err)
	}
	var exited exitCoder
	if !errors.As(err, &exited) || exited.ExitCode() != 128 {
		t.Errorf("errors.As exitCoder = %v, want status 128", exited)
	}
	if plain := exitError(1); withReason(plain, []byte("unknown")) != error(plain) {
		t.Error("a failure with no known reason changed")
	}
	if withReason(nil, []byte("fatal: Authentication failed")) != nil {
		t.Error("withReason(nil) is not nil")
	}
}

// What a chatty git writes to standard error is kept only up to the cap
// (gt-vpj84).
func TestCappedWriterKeepsTheFirstBytesOnly(t *testing.T) {
	t.Parallel()
	w := &cappedWriter{limit: 5}
	for _, chunk := range []string{"ab", "cdef", "gh"} {
		n, err := w.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v, want the whole chunk taken", chunk, n, err)
		}
	}
	if got := string(w.Bytes()); got != "abcde" {
		t.Errorf("kept %q, want %q", got, "abcde")
	}
}
