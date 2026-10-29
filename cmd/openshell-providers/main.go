// Command openshell-providers imports the OpenShell profiles a polecat sandbox
// needs and creates the deepseek and forgejo-polecat providers from key files
// under ~/.config/gt (gt-tpg0p.53). This step of the port parses the command
// line, resolves the paths and refuses a missing openshell; the import itself
// arrives in a later step, so a run that gets that far refuses instead of
// reporting success for work it did not do, and make openshell-providers still
// runs scripts/openshell-providers.sh.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/openshellprov"
)

// notImplemented is what a run ends with until the import steps land: a run
// that created no provider must not exit 0 as though it created them.
const notImplemented = openshellprov.Name + ": the profile import is not implemented yet; make openshell-providers still runs scripts/openshell-providers.sh"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, openshellprov.ProcessHost))
}

// run is main without the process: the arguments and streams are given, and h
// supplies the working directory, environment, PATH and filesystem.
func run(args []string, out, errw io.Writer, h openshellprov.Host) int {
	opts, err := openshellprov.Parse(args)
	if err != nil {
		fmt.Fprintln(errw, err)
		return 2
	}
	if opts.Help {
		fmt.Fprintln(out, openshellprov.Usage)
		return 0
	}
	if _, err := h.LookPath("openshell"); err != nil {
		fmt.Fprintf(errw, "%s: openshell is not on PATH (brew install openshell)\n", openshellprov.Name)
		return 1
	}
	// The paths are resolved before anything is read: a run outside the
	// checkout has no profiles to import, and the import steps need the result.
	if _, err := openshellprov.ResolvePaths(h); err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	fmt.Fprintln(errw, notImplemented)
	return 1
}
