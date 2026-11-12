package gtwrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// gitTimeout bounds each git call the facts reader makes.
const gitTimeout = 5 * time.Second

// gitFatal is the status git exits with on a fatal error. A directory outside any
// repository is one of them, and so are other failures.
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

// notARepoText is what git says, with its messages in English, when dir is a
// plain directory outside any repository. It is the whole form: a linked
// worktree whose gitdir was pruned, or a .git file that points nowhere, prints
// "not a git repository: <path>" with the same status, and that is a corrupt
// worktree, not a plain directory. Other fatal errors (a repository git refuses
// as owned by someone else, a corrupt or unreadable one) share the exit status
// 128 too, so neither the status nor a prefix of the text can tell them apart.
const notARepoText = "not a git repository (or any of the parent directories)"

// gitEnvOverrides are the variables that make git ignore -C, or search or read
// somewhere other than the repository at dir: another repository, work tree,
// index, common dir or object store, or a ceiling that stops the search for one.
var gitEnvOverrides = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES",
}

// OSGit runs git on the host, in dir, with args as an argument list and no shell.
// It is bounded by gitTimeout. The subprocess answers in English, so the refusal
// of a directory outside any repository is recognized, and it does not inherit
// the variables that would point it elsewhere than dir. An error names the
// subcommand and the first line of what git said, so a refusal such as
// "dubious ownership" reaches the caller; a run that hit the bound says so.
func OSGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		stderr := ""
		if errors.As(err, &exit) {
			stderr = string(exit.Stderr)
		}
		return "", gitError(err, stderr, args, ctx.Err() != nil)
	}
	return string(out), nil
}

// gitEnv is environ without the variables that override -C, and with git's
// messages in English.
func gitEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(gitEnvOverrides, name) && name != "LC_ALL" {
			out = append(out, entry)
		}
	}
	return append(out, "LC_ALL=C")
}

// gitError maps a failed git run onto the facts reader's errors. Only git's
// answer that dir is outside any repository (its fatal status with the
// not-a-repository text in stderr) is ErrNotWorktree, which the caller reads as
// "add nothing". Every other failure is an error that carries the first line of
// stderr: another fatal status, a timeout and a missing git included, so a broken
// git is refused here and never mistaken for a plain directory. timedOut says the
// run was stopped by gitTimeout, which would otherwise read "signal: killed".
func gitError(err error, stderr string, args []string, timedOut bool) error {
	if timedOut {
		return fmt.Errorf("gtwrap: git %s: did not finish within %s", strings.Join(args, " "), gitTimeout)
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) && coded.ExitCode() == gitFatal && strings.Contains(stderr, notARepoText) {
		return ErrNotWorktree
	}
	detail := ""
	if first, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n"); first != "" {
		detail = ": " + first
	}
	return fmt.Errorf("gtwrap: git %s: %w%s", strings.Join(args, " "), err, detail)
}
