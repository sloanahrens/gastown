// Command lintpin checks the golangci-lint on PATH against the version the
// gastown Makefile pins, and installs that version when it is absent or
// differs.
//
// It is the linter check make lint and make lint-tools run, so both take the
// pin from GOLANGCI_LINT_VERSION and a wrong linter is refused, not silently
// used:
//
//	make lint       -> go run ./cmd/lintpin check -pin v2.13.2
//	make lint-tools -> go run ./cmd/lintpin install -pin v2.13.2
//
// check exits 0 when PATH carries the pin, and 1 with the reason on stderr
// when it does not. install exits 0 after it has ensured the pin, and 1 when
// the install fails. Both exit 2 for a wrong command line or an empty pin.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/lintpin"
)

func main() {
	os.Exit(run(os.Args[1:], execRunner, os.Stdout, os.Stderr))
}

// execRunner runs the named command and returns its standard output.
func execRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output() //nolint:gosec // G204: lintpin names golangci-lint and go, and the only variable argument is the Makefile's pin
}

// run is main without os.Exit: a test hands it a command line and a runner,
// and reads the exit code and the streams.
func run(args []string, runCmd lintpin.Runner, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "lintpin: name a mode, check or install")
		return 2
	}
	mode, rest := args[0], args[1:]

	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	pin := fs.String("pin", "", "the golangci-lint version the Makefile pins, such as v2.13.2")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "lintpin: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *pin == "" {
		fmt.Fprintln(stderr, "lintpin: -pin is empty: pass the Makefile's GOLANGCI_LINT_VERSION")
		return 2
	}

	switch mode {
	case "check":
		if _, err := lintpin.Verify(runCmd, *pin); err != nil {
			fmt.Fprintf(stderr, "lint: %v\n", err)
			return 1
		}
		return 0
	case "install":
		installed, err := lintpin.Install(runCmd, *pin)
		if err != nil {
			fmt.Fprintf(stderr, "lint-tools: %v\n", err)
			return 1
		}
		if installed {
			fmt.Fprintf(stdout, "lint-tools: installed golangci-lint %s\n", *pin)
		} else {
			fmt.Fprintf(stdout, "lint-tools: golangci-lint %s is already on PATH\n", *pin)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "lintpin: unknown mode %q: name check or install\n", mode)
		return 2
	}
}
