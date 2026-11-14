package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// gitCall is one git invocation: the arguments after "git" (any --git-dir
// prefix already applied), the directory it runs in ("" means the process
// cwd), extra environment on top of the process's own, the keys taken out of
// it, stdin, and a deadline (0 means none).
type gitCall struct {
	dir     string
	args    []string
	env     []string
	unset   []string
	stdin   string
	timeout time.Duration
}

// runFunc is the unexported name of ExecFunc, whose comment states the contract
// a runner meets. Git sends every git process through one, so tests can record
// and answer them without starting git.
type runFunc func(c gitCall) (stdout, stderr string, err error)

// ExecCall is one git invocation, the value an ExecFunc is handed; it is an
// alias of gitCall, so a runner here and a caller's runner receive the same
// value.
type ExecCall = gitCall

// Dir is the directory the call runs in, "" meaning the process cwd.
func (c gitCall) Dir() string { return c.dir }

// Args is the arguments after "git", with any --git-dir prefix already
// applied. It is a copy, so a runner that changes it changes only what it
// holds.
func (c gitCall) Args() []string { return append([]string(nil), c.args...) }

// Env is the environment the call adds on top of the process's own. It is a
// copy, for the reason Args is.
func (c gitCall) Env() []string { return append([]string(nil), c.env...) }

// Unset is the keys the call removes from the process's environment. It is a
// copy, for the reason Args is.
func (c gitCall) Unset() []string { return append([]string(nil), c.unset...) }

// Stdin is what the call feeds git on stdin; "" means nothing.
func (c gitCall) Stdin() string { return c.stdin }

// Timeout is the deadline after which the call is killed; 0 means none.
func (c gitCall) Timeout() time.Duration { return c.timeout }

// ExecFunc runs one git invocation and returns what it wrote to stdout and
// stderr. Its error carries a non-zero exit as an ExitCode() int method, as
// *exec.ExitError does, and a deadline kill as ErrTimedOut, the two shapes the
// Git methods read; it is an alias of runFunc, so a caller outside this package
// can pass its own runner to NewGitWithExec without a conversion.
type ExecFunc = runFunc

// NewGitWithExec creates a Git wrapper for the given directory that sends every
// git call through exec instead of running git on PATH; a nil exec keeps the
// default, the real git on PATH, as NewGit has.
func NewGitWithExec(workDir string, exec ExecFunc) *Git {
	return &Git{workDir: workDir, exec: exec}
}

// errTimedOut is wrapped by a runFunc's error when a call ran out its
// timeout and was killed.
var errTimedOut = errors.New("git timed out")

// runner returns the runFunc g sends git through: the real git on PATH
// unless a test set one. The observer, when set, wraps that run below the
// config-read retry, so a retry is counted as the second process it starts.
func (g *Git) runner() runFunc {
	run := realRun
	if g.exec != nil {
		run = g.exec
	}
	if g.observe != nil {
		run = observed(run, g.observe)
	}
	if g.configRetry {
		return g.withConfigReadRetry(run)
	}
	return run
}

// WithRunObserver returns a copy of g whose every git process calls observe
// once, before the process starts, so a caller can count the git calls a
// piece of work makes. A call that fails is counted too. A nil observe counts
// nothing. The receiver is left as it was.
func (g *Git) WithRunObserver(observe func()) *Git {
	c := *g
	c.observe = observe
	return &c
}

// observed wraps run so observe is called before each invocation of it.
func observed(run runFunc, observe func()) runFunc {
	return func(c gitCall) (string, string, error) {
		observe()
		return run(c)
	}
}

// realRun runs git on PATH.
//
// A call with a timeout is killable as a whole (boundRemoteCommand), and a
// call with a timeout or extra environment (a push, which may fork a remote
// helper) captures its output in files rather than pipes (runCapturingOutput).
// Every other call runs in its own detached process group with pipes.
func realRun(c gitCall) (string, string, error) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if c.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", c.args...)
	cmd.Dir = c.dir
	if len(c.env) > 0 || len(c.unset) > 0 {
		cmd.Env = callEnv(c.env, c.unset)
	}
	if c.stdin != "" {
		cmd.Stdin = strings.NewReader(c.stdin)
	}
	if c.timeout > 0 {
		boundRemoteCommand(cmd)
	} else {
		util.SetDetachedProcessGroup(cmd)
	}

	var stdout, stderr string
	var err error
	if c.timeout > 0 || len(c.env) > 0 {
		stdout, stderr, err = runCapturingOutput(cmd)
	} else {
		var out, errOut bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errOut
		err = cmd.Run()
		stdout, stderr = out.String(), errOut.String()
	}
	if err != nil && c.timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout, stderr, errTimedOut
	}
	return stdout, stderr, err
}

// callEnv is the environment a git call runs with: the process's own, less the
// keys the call unsets, plus the variables it adds. A key in both keeps the
// added value, because os/exec runs the last entry for a key.
//
// Unsetting is a removal rather than an empty value on purpose: git reads an
// empty GIT_DIR as the current directory (it reports "not a git repository"
// with an empty path) and an empty GIT_WORK_TREE as an invalid path, so
// exporting them empty breaks every command instead of clearing them.
func callEnv(extra, unset []string) []string {
	env := os.Environ()
	if len(unset) > 0 {
		kept := make([]string, 0, len(env))
		for _, entry := range env {
			key, _, _ := strings.Cut(entry, "=")
			if containsKey(unset, key) {
				continue
			}
			kept = append(kept, entry)
		}
		env = kept
	}
	return append(env, extra...)
}

// containsKey reports whether keys holds key.
func containsKey(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// exitCode returns the exit status err carries, or -1 when it carries none
// (git did not start, or was killed). It matches any error with an
// ExitCode() method, so a test runner's errors work as well as
// *exec.ExitError.
func exitCode(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}
