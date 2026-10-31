package crewfresh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// gitCommand runs git's argv and returns what it wrote to stdout and stderr.
type gitCommand func(ctx context.Context, argv []string) (stdout, stderr string, err error)

// realGit runs git's argv as a process.
func realGit(ctx context.Context, argv []string) (string, string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed git subcommand; the directory came from NewGit
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	return runCaptured(cmd, &stdout, &stderr)
}

// runCaptured runs cmd and returns what it wrote to the two buffers and its
// failure. The buffers are read after the process exits, and the spawn is the
// one branch the unit tier cannot drive: a unit test starts no process, so the
// integration tier is where this failure is exercised.
func runCaptured(cmd *exec.Cmd, stdout, stderr *bytes.Buffer) (string, string, error) {
	run := cmd.Run()
	return stdout.String(), stderr.String(), run
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
	stdout, stderr, err := command(ctx, argv)
	if err == nil {
		return stdout, nil
	}
	return stdout, gitCommandError(args, stderr, err, ctx.Err())
}

// gitCommandError is the error for a git command that failed: what ran, what
// git said, and the failure itself, wrapped so errors.Is and errors.As still
// read it.
func gitCommandError(args []string, stderr string, err, ctxErr error) error {
	name := "git " + strings.Join(args, " ")
	if ctxErr != nil {
		return fmt.Errorf("%s: %w", name, ctxErr)
	}
	if detail := strings.TrimSpace(stderr); detail != "" {
		return fmt.Errorf("%s: %s: %w", name, detail, err)
	}
	return fmt.Errorf("%s: %w", name, err)
}
