package presubmit

import (
	"flag"
	"fmt"
	"io"
)

// RedBaseExit parses -base (default origin/main, an empty value refused) and
// makes a directory with mkdirTemp("", "redbase-*"), calls check on it, and
// removes it with removeAll after the check on every path (gt-khdcj6). An
// unknown flag or empty base is 2 before either call; a make, check or removal
// failure, or a not-ok check, is 1 with the error on stderr; an ok check is 0.
func RedBaseExit(args []string, stdout, stderr io.Writer, mkdirTemp func(dir, pattern string) (string, error), removeAll func(string) error, check func(base, dir string, out io.Writer) (bool, error)) int {
	fs := flag.NewFlagSet("redbase", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "origin/main", "the git ref the change is checked against")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" {
		fmt.Fprintln(stderr, "redbase: -base must not be empty")
		return 2
	}
	dir, err := mkdirTemp("", "redbase-*")
	if err != nil {
		fmt.Fprintf(stderr, "redbase: making the temporary directory: %v\n", err)
		return 1
	}
	ok, checkErr := check(*base, dir, stdout)
	removeErr := removeAll(dir)
	if checkErr != nil {
		fmt.Fprintf(stderr, "redbase: %v\n", checkErr)
	}
	if removeErr != nil {
		fmt.Fprintf(stderr, "redbase: removing %s: %v\n", dir, removeErr)
	}
	if checkErr != nil || removeErr != nil || !ok {
		return 1
	}
	return 0
}
