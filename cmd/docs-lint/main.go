// Command docs-lint runs the deterministic checks on the docs and comments
// tiers and lists the tiers the weekly doc audit slices from (gt-tpg0p.113, the
// port of the shell script, removed in gt-tpg0p.114).
//
//	docs-lint                 run all checks; one line per finding (path:line:
//	                          rule: message)
//	docs-lint --list <tier>   agent-facing | reference | historical | go
//	docs-lint --slice N M     N docs and M go files, least recently committed first
//	docs-lint --words         total words across the agent-facing tier
//
// It exits 0 when clean, 1 when a rule is broken, and 2 for a refused command
// line (an unknown option, a missing argument, a DOCS_LINT_CEILING that is not a
// number) or a check that could not run; the reason is on stderr. Environment:
// DOCS_LINT_ROOT is the repository root (default: the git top level of the
// working directory) and DOCS_LINT_CEILING the word ceiling (default 1500).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/docslint"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, gitTopLevel, os.Stdout, os.Stderr))
}

// run is the process minus os.Exit: a test hands it a command line, an
// environment and the two streams, and reads the exit code it would have exited
// with.
func run(args []string, getenv func(string) string, topLevel func() (string, error), stdout, stderr io.Writer) int {
	return docslint.Run(args, docslint.Env{
		Getenv:   getenv,
		TopLevel: topLevel,
		NewFS:    func(root string) docslint.FS { return docslint.OSFiles{Root: root} },
		NewGit:   func(root string) docslint.Git { return docslint.GitLog{Root: root} },
	}, stdout, stderr)
}

// gitTopLevel is the git top level of the working directory.
func gitTopLevel() (string, error) {
	return topLevelFrom(func() ([]byte, error) {
		return exec.Command("git", "rev-parse", "--show-toplevel").Output()
	})
}

// topLevelFrom reads the top level from what git printed, so a test reaches the
// failing branch without starting git.
func topLevelFrom(git func() ([]byte, error)) (string, error) {
	out, err := git()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", errors.New("git rev-parse --show-toplevel printed nothing")
	}
	return top, nil
}
