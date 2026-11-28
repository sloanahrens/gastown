package presubmit

import (
	"flag"
	"fmt"
	"io"
)

// RunLandedIntegration parses -base (default HEAD^1, the first parent of HEAD; an empty value refused), asks list for the changed integration tests and the not-run files of that base, and hands both to RunLandedPackages with goTest (gt-d4usdc). An unknown flag or empty base is 2 before list runs; a list error is printed to stderr and is 1 with nothing run; otherwise the result is RunLandedPackages' own.
func RunLandedIntegration(args []string, stdout, stderr io.Writer, list func(base string) ([]RedBasePackage, []string, error), goTest func(args []string) error) int {
	fs := flag.NewFlagSet("landed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "HEAD^1", "the git ref the landed change is checked against")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" {
		fmt.Fprintln(stderr, "landed: -base must not be empty")
		return 2
	}
	packages, notRun, err := list(*base)
	if err != nil {
		fmt.Fprintf(stderr, "landed: %v\n", err)
		return 1
	}
	return RunLandedPackages(packages, notRun, stdout, stderr, goTest)
}
