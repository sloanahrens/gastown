// Command repo-guards runs the repository guards make lint runs (gt-tpg0p.107,
// the port of scripts/repo-guards.sh): go.mod has no replace directive but the
// tracker module's, .beads/issues.jsonl is not tracked, and every tracked .go
// file outside testdata is gofmt -s clean. It exits 1 when any guard fails.
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
	env := repoguard.Env{
		Git:      repoguard.CLIGit{},
		Exec:     repoguard.CLIExec{},
		LookPath: exec.LookPath,
		ReadFile: os.ReadFile,
	}
	os.Exit(run(os.Args[1:], env, os.Stdout, os.Stderr))
}

// run is main without the process: the arguments and the environment are
// given, and the guards' output goes to the two streams.
func run(args []string, env repoguard.Env, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	root, err := repoRoot(args, env)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
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

// repoRoot is the repository to check: the argument as given, or the top level
// of the repository the working directory is in. Either must be a git
// repository. A root that is not one would otherwise list no files and pass.
func repoRoot(args []string, env repoguard.Env) (string, error) {
	dir := "."
	if len(args) == 1 {
		dir = args[0]
	}
	out, err := env.Exec.Run(dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("repo-guards: %s is not a git repository", dir)
	}
	if len(args) == 1 {
		return dir, nil
	}
	return strings.TrimSpace(out), nil
}
