// Command repo-guards runs the repository guards that make lint runs: go.mod has
// no replace directive but the tracker module's, .beads/issues.jsonl is not
// tracked, and every tracked .go file outside testdata is gofmt -s clean. It
// exits 0 when every guard passes, 1 when a guard fails or the root is not a git
// repository, and 2 when the arguments are wrong or the root is inside a
// repository but is not its top level.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/repoguard"
)

const usage = "repo-guards: usage: repo-guards [repo-root]"

func main() {
	os.Exit(run(os.Args[1:], liveEnv(), os.Stdout, os.Stderr))
}

// liveEnv is the production wiring of the guards: the git binary, gofmt from
// PATH, and the real filesystem for go.mod.
func liveEnv() repoguard.Env {
	return repoguard.Env{
		Git:      repoguard.CLIGit{},
		Exec:     repoguard.CLIExec{},
		LookPath: exec.LookPath,
		ReadFile: os.ReadFile,
	}
}

// run is main without the process: the arguments and the environment are
// given, and the guards' output goes to the two streams.
func run(args []string, env repoguard.Env, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	root, code, err := repoRoot(args, env)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return code
	}
	rep := repoguard.Check(root, env)
	for _, line := range rep.Replaces {
		fmt.Fprintln(stdout, line)
	}
	for _, line := range rep.Problems {
		fmt.Fprintln(stderr, line)
	}
	if rep.Failed() {
		return 1
	}
	return 0
}

// repoRoot is the repository to check: the top level of the repository the
// working directory is in, or the argument, which must itself be that top
// level. Checked from a subdirectory the guards would read that subtree: go.mod
// is missing there, and a tracked root file looks untracked, so a repository
// could pass without being read. It returns the exit status to refuse with.
func repoRoot(args []string, env repoguard.Env) (root string, code int, err error) {
	dir := "."
	if len(args) == 1 {
		dir = args[0]
	}
	out, err := env.Exec.Run(dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", 1, fmt.Errorf("repo-guards: %s is not a git repository", dir)
	}
	top := strings.TrimSpace(out)
	if len(args) == 0 {
		return top, 0, nil
	}
	if same, err := sameDir(dir, top); err != nil || !same {
		return "", 2, fmt.Errorf("repo-guards: %s is not the top level of its repository (%s)", dir, top)
	}
	return top, 0, nil
}

// sameDir reports whether a and b name the same directory. The file system
// answers it: each path is stat'ed, which follows symlinks, and the two results
// are compared by identity. So "dir/", "./dir" and "dir/../dir" name "dir", and
// a path through a symlink names its target. A path that does not exist is an
// error, not a match.
func sameDir(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ia, ib), nil
}
