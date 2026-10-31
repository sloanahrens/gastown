package crewfresh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// gitEnv is the environment git runs in: the caller's own, with the terminal
// prompt off. A prompt cannot be answered from a background fetch, and left on
// it waits out the deadline instead of failing at once.
func gitEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
}

// gitCommand runs git's argv and returns what it wrote to stdout.
type gitCommand func(ctx context.Context, argv []string) (stdout string, err error)

// realGit runs git's argv as a process. Git's standard error is discarded at
// the spawn: it can quote a remote URL that carries a credential, so a
// failure is named by its command and exit status only (internal/deploysource
// makes the same choice).
func realGit(ctx context.Context, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed git subcommand; the directory came from NewGit
	// The deadline has to reach git's helpers, and Wait has to stop when it
	// does: killing git alone leaves a helper holding the output pipe, which
	// held Wait well past the caller's timeout (gt-vpj84).
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = gitWaitDelay
	cmd.Env = gitEnv()
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	return gitOutput(out, err)
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
	out, err := g.run(context.Background(), "rev-parse", "--abbrev-ref", "HEAD")
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
	out, err := g.run(context.Background(), "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// Behind is `git rev-list --count HEAD..origin/main`.
func (g *gitExec) Behind() (int, error) {
	out, err := g.run(context.Background(), "rev-list", "--count", "HEAD..origin/main")
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
	_, err := g.run(context.Background(), "merge-base", "--is-ancestor", "HEAD", "origin/main")
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
	_, err := g.run(context.Background(), "merge", "--ff-only", "origin/main")
	return err
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
