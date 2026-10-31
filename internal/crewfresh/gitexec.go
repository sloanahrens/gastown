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

// gitExec is the Git of one clone, answered by running the git binary there.
type gitExec struct {
	dir string
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

// HeadIsAncestorOfOriginMain is `git merge-base --is-ancestor HEAD
// origin/main`: exit 1 is the answer "no", and every other failure is an unread
// error rather than an answer.
func (g *gitExec) HeadIsAncestorOfOriginMain() (bool, error) {
	_, err := g.run(context.Background(), "merge-base", "--is-ancestor", "HEAD", "origin/main")
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
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
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...) //nolint:gosec // G204: fixed git subcommand; dir is the clone the caller named
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.String(), fmt.Errorf("git %s: %w", strings.Join(args, " "), ctxErr)
	}
	// What git said is the only account of why it refused, and the wrapped
	// error keeps errors.As able to read an exit code from it.
	if detail := strings.TrimSpace(stderr.String()); detail != "" {
		return stdout.String(), fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), detail, err)
	}
	return stdout.String(), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}
