package gtwrap

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gitTimeout bounds each git call the facts reader makes.
const gitTimeout = 5 * time.Second

// gitFatal is the status git exits with on a fatal error. A directory outside any
// repository is one of them.
const gitFatal = 128

// ErrNotWorktree reports that a directory is not inside a git worktree.
var ErrNotWorktree = errors.New("not inside a git worktree")

// Git runs git with args in dir and returns its standard output. A test supplies
// its own, so the facts are read with no repository.
type Git func(dir string, args ...string) (string, error)

// WorktreeFacts are the branch and HEAD commit of a worktree. gt done needs them
// for a crew branch, and the pod cannot read them from the Mac's tree.
type WorktreeFacts struct {
	Branch string
	Head   string
}

// ReadWorktreeFacts reads the branch and HEAD of the worktree at dir. It reports
// ok false, with no error, when dir is not a worktree or HEAD is detached: the
// caller then adds nothing, and gt done refuses the call in the pod. Any other
// failure is an error, so the call is refused here and never reaches gt done
// with facts it could not read.
func ReadWorktreeFacts(git Git, dir string) (facts WorktreeFacts, ok bool, err error) {
	branch, err := git(dir, "branch", "--show-current")
	if errors.Is(err, ErrNotWorktree) {
		return WorktreeFacts{}, false, nil
	}
	if err != nil {
		return WorktreeFacts{}, false, fmt.Errorf("gtwrap: read the branch of the worktree at %s: %w", dir, err)
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return WorktreeFacts{}, false, nil
	}
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return WorktreeFacts{}, false, fmt.Errorf("gtwrap: read the HEAD commit of the worktree at %s: %w", dir, err)
	}
	return WorktreeFacts{Branch: branch, Head: strings.TrimSpace(head)}, true, nil
}

// OSGit runs git on the host, in dir, with args as an argument list and no shell.
// It is bounded by gitTimeout. Its errors name the subcommand and never git's
// output, which can hold a path.
func OSGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", gitError(err, args)
	}
	return string(out), nil
}

// gitError maps a failed git run onto the facts reader's errors. Git exits with
// its fatal status when dir is outside any repository, and also on other fatal
// errors; every fatal status maps to ErrNotWorktree, so gt done is refused in the
// pod. Any other failure, a timeout or a missing git included, is an error.
func gitError(err error, args []string) error {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) && coded.ExitCode() == gitFatal {
		return ErrNotWorktree
	}
	return fmt.Errorf("gtwrap: git %s: %w", strings.Join(args, " "), err)
}
