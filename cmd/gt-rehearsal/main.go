// Command gt-rehearsal is the Mac's load rehearsal binary (gt-13dre6.32).
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/rehearsal"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, "command", fmt.Errorf("name a subcommand: compare"))
	}
	switch args[0] {
	case "compare":
		return compare(args[1:], stdout, stderr)
	default:
		return fail(stderr, "command", fmt.Errorf("unknown subcommand %q", args[0]))
	}
}

// compare prints the two record files' report: a is before and b is after, and
// the report lives once, in rehearsal.Compare.
func compare(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		return fail(stderr, "compare", fmt.Errorf("want two record files, got %d", len(args)))
	}
	before, err := rehearsal.Read(args[0])
	if err != nil {
		return fail(stderr, "compare", err)
	}
	after, err := rehearsal.Read(args[1])
	if err != nil {
		return fail(stderr, "compare", err)
	}
	if _, err := fmt.Fprint(stdout, rehearsal.Compare(before, after)); err != nil {
		return fail(stderr, "print", err)
	}
	return 0
}

// fail names the step in one stderr line, so a runbook can tell what stopped the run.
func fail(stderr io.Writer, step string, err error) int {
	fmt.Fprintf(stderr, "gt-rehearsal: %s: %v\n", step, err)
	return 1
}
