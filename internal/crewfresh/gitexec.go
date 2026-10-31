package crewfresh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// gitWaitDelay bounds how long Wait may keep waiting once the deadline has run
// out. Git is not one process — a fetch forks a remote helper — so the kill
// reaches the whole process group (util.SetProcessGroup), and this is the
// backstop should a helper survive that kill and hold the output pipe.
var gitWaitDelay = util.ProcessGroupKillGrace + 3*time.Second

// localGitTimeout bounds each git call that is not the fetch. A local read or
// the fast-forward touches no network, so a minute is far past any healthy run;
// what it guards against is a hang (a stale lock, a dead mount) that would
// otherwise stall the caller for good (gt-vpj84).
const localGitTimeout = time.Minute

// gitEnv is the environment git runs in: base (the caller's own), with every
// way git could stop to ask a question turned off. A prompt cannot be answered
// from a background fetch, and left on it waits out the deadline instead of
// failing at once. GIT_TERMINAL_PROMPT covers git's own prompt, GIT_ASKPASS the
// helper it would fall back to, and BatchMode ssh's password and host-key
// questions; an ssh command the caller already set is theirs and stays.
func gitEnv(base []string) []string {
	env := append(slices.Clone(base), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
	if !slices.ContainsFunc(base, func(kv string) bool { return strings.HasPrefix(kv, "GIT_SSH_COMMAND=") }) {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

// gitCommand runs git's argv and returns what it wrote to stdout.
type gitCommand func(ctx context.Context, argv []string) (stdout string, err error)

// realGit runs git's argv as a process. Git's standard error is read but never
// repeated: it can quote a remote URL that carries a credential. A failure is
// named by its command, its exit status and, when stderr matches a phrase
// gitReason knows, a fixed reason of this package's own wording
// (internal/deploysource makes the same choice about the text).
func realGit(ctx context.Context, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed git subcommand; the directory came from NewGit
	// The deadline has to reach git's helpers, and Wait has to stop when it
	// does: killing git alone leaves a helper holding the output pipe, which
	// held Wait well past the caller's timeout (gt-vpj84).
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = gitWaitDelay
	cmd.Env = gitEnv(os.Environ())
	stderr := &cappedWriter{limit: stderrCap}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	return gitOutput(out, withReason(err, stderr.Bytes()))
}

// stderrCap is how much of git's standard error is kept to read a reason from.
const stderrCap = 4096

// cappedWriter keeps the first limit bytes written to it and drops the rest,
// so a chatty git cannot grow the caller's memory.
type cappedWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (w *cappedWriter) Bytes() []byte { return w.buf.Bytes() }

// gitReasons maps phrases git prints to the reason this package states
// instead. The reason is a constant, never git's own text, so nothing a remote
// URL carried can reach an error. The first matching row wins, so the
// specific phrases come before the general ones.
var gitReasons = []struct{ phrase, reason string }{
	{"could not read username", "authentication required"},
	{"terminal prompts disabled", "authentication required"},
	{"authentication failed", "authentication required"},
	{"permission denied", "authentication required"},
	{"could not resolve host", "remote unreachable"},
	{"connection refused", "remote unreachable"},
	{"connection timed out", "remote unreachable"},
	{"network is unreachable", "remote unreachable"},
	{"couldn't find remote ref", "remote ref not found"},
	{"repository not found", "remote not found"},
	{"does not appear to be a git repository", "remote not found"},
	{"not possible to fast-forward", "not a fast-forward"},
	{"index.lock", "the index is locked by another git process"},
	{"not a git repository", "not a git repository"},
}

// gitReason is the fixed reason for git's standard error, or "" when it
// matches no known phrase.
func gitReason(stderr []byte) string {
	text := strings.ToLower(string(stderr))
	for _, r := range gitReasons {
		if strings.Contains(text, r.phrase) {
			return r.reason
		}
	}
	return ""
}

// reasonedError is a git failure with the reason its standard error stated.
// Unwrap keeps the underlying failure readable to errors.Is and errors.As, so
// the exit status still reaches HeadIsAncestorOfOriginMain.
type reasonedError struct {
	err    error
	reason string
}

func (e *reasonedError) Error() string { return e.err.Error() + " (" + e.reason + ")" }

func (e *reasonedError) Unwrap() error { return e.err }

// withReason adds the reason git's standard error gives to err; with no err or
// no known reason it returns err as it was.
func withReason(err error, stderr []byte) error {
	if err == nil {
		return nil
	}
	if reason := gitReason(stderr); reason != "" {
		return &reasonedError{err: err, reason: reason}
	}
	return err
}

// gitOutput is what a finished git process reported: its standard output when
// it succeeded, and nothing but its failure when it did not. A failed
// command's partial output must never pass as an answer. This is also where
// the unit tier drives the process's failure, since a unit test starts no
// process to fail.
func gitOutput(out []byte, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// gitExec is the Git of one clone, answered by running the git binary there.
type gitExec struct {
	dir string
	// command is nil for the real git; a test sets it to drive a failure the
	// unit tier may not start a process to reach.
	command gitCommand
	// localTimeout bounds each call but the fetch; zero means localGitTimeout.
	localTimeout time.Duration
}

// NewGit returns the Git for the clone at dir, running every command as
// `git -C dir ...`.
func NewGit(dir string) Git {
	return &gitExec{dir: dir}
}

// Fetch runs `git fetch origin main`, giving up after timeout.
func (g *gitExec) Fetch(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := g.run(ctx, "fetch", "origin", "main")
	return err
}

// Branch is the checked-out branch, "" for a detached HEAD: --abbrev-ref
// prints "HEAD" for one, and Run's State speaks "" for it.
func (g *gitExec) Branch() (string, error) {
	out, err := g.local("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if branch := strings.TrimSpace(out); branch != "HEAD" {
		return branch, nil
	}
	return "", nil
}

// Clean reports whether `git status --porcelain` printed nothing.
func (g *gitExec) Clean() (bool, error) {
	out, err := g.local("status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// Behind is `git rev-list --count HEAD..origin/main`.
func (g *gitExec) Behind() (int, error) {
	out, err := g.local("rev-list", "--count", "HEAD..origin/main")
	if err != nil {
		return 0, err
	}
	behind, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("read the commit count git printed: %w", err)
	}
	return behind, nil
}

// exitCoder is a command failure carrying the status it exited with.
type exitCoder interface {
	ExitCode() int
}

// HeadIsAncestorOfOriginMain is `git merge-base --is-ancestor HEAD
// origin/main`: exit 1 is the answer "no", and every other failure is an unread
// error rather than an answer.
func (g *gitExec) HeadIsAncestorOfOriginMain() (bool, error) {
	_, err := g.local("merge-base", "--is-ancestor", "HEAD", "origin/main")
	var exited exitCoder
	if errors.As(err, &exited) && exited.ExitCode() == 1 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// FastForwardMain runs `git merge --ff-only origin/main`, the only ref move
// this package makes: no reset, no checkout and no pull can move a clone git
// would not fast-forward.
func (g *gitExec) FastForwardMain() error {
	_, err := g.local("merge", "--ff-only", "origin/main")
	return err
}

// local runs one git command that is not the fetch, under localTimeout: every
// call is bounded, so none can stall the caller on a hung git (gt-vpj84).
func (g *gitExec) local(args ...string) (string, error) {
	timeout := g.localTimeout
	if timeout <= 0 {
		timeout = localGitTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return g.run(ctx, args...)
}

// run runs one git command in the clone and returns its standard output.
func (g *gitExec) run(ctx context.Context, args ...string) (string, error) {
	argv := append([]string{"git", "-C", g.dir}, args...)
	command := g.command
	if command == nil {
		command = realGit
	}
	stdout, err := command(ctx, argv)
	if err != nil {
		// Nothing of a failed command's output is an answer (gitOutput).
		return "", gitCommandError(args, err, ctx.Err())
	}
	return stdout, nil
}

// gitCommandError is the error for a git command that failed: what ran and
// the failure itself, wrapped so errors.Is and errors.As still read it. It
// does not repeat git's output, which can carry a credential-bearing remote
// URL.
func gitCommandError(args []string, err, ctxErr error) error {
	name := "git " + strings.Join(args, " ")
	if ctxErr != nil {
		return fmt.Errorf("%s: %w", name, ctxErr)
	}
	return fmt.Errorf("%s: %w", name, err)
}
