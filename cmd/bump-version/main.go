// Command bump-version bumps the version across the Gas Town release files.
// This slice is the command line alone: parsing, the version rule and the flag
// rules of scripts/bump-version.sh; the run that edits files arrives in
// gt-tpg0p.75.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/bumpversion"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is the command with its error stream injected, so main_test.go can pin
// the lines and the exit code without a subprocess.
func run(args []string, errw io.Writer) int {
	_, err := bumpversion.Parse(args)
	switch {
	case err == nil:
		// The run that edits, commits and tags arrives in gt-tpg0p.75; until
		// then a readable command line is the whole success condition.
		return 0
	case errors.Is(err, bumpversion.ErrUsage):
		fmt.Fprintf(errw, "%s", bumpversion.Usage)
		return 1
	}
	var usage *bumpversion.UsageError
	if errors.As(err, &usage) {
		fmt.Fprintln(errw, usage.Msg)
		fmt.Fprintf(errw, "%s", bumpversion.Usage)
		return 1
	}
	fmt.Fprintln(errw, err)
	return 1
}
