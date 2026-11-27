// Command gt-rehearsal is the Mac's load rehearsal binary (gt-13dre6.32).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/steveyegge/gastown/internal/callercwd"
	"github.com/steveyegge/gastown/internal/rehearsal"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand. A command line it cannot use exits 2; a record
// it cannot read exits 1.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, "command", "name a subcommand: run, compare")
	}
	switch args[0] {
	case "compare":
		return compare(args[1:], stdout, stderr)
	case "run":
		ctx, stop := interruptContext(context.Background(), signal.NotifyContext)
		defer stop()
		return runCommand(ctx, args[1:], callercwd.Require, rehearsal.Execute, stdout, stderr)
	default:
		return usage(stderr, "command", fmt.Sprintf("unknown subcommand %q", args[0]))
	}
}

// compare prints the two record files' report: a is before and b is after, and
// the report lives once, in rehearsal.Compare.
func compare(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		return usage(stderr, "compare", fmt.Sprintf("want two record files, got %d", len(args)))
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

// usage reports a wrong command line in one stderr line and returns 2: the
// caller's arguments are at fault, not a record it could not read.
func usage(stderr io.Writer, step, msg string) int {
	fmt.Fprintf(stderr, "gt-rehearsal: %s: %s\n", step, msg)
	return 2
}

// fail names the step in one stderr line and returns 1: the command line was
// right but a record or the output failed.
func fail(stderr io.Writer, step string, err error) int {
	fmt.Fprintf(stderr, "gt-rehearsal: %s: %v\n", step, err)
	return 1
}
