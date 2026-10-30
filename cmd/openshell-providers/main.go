// Command openshell-providers imports the OpenShell profiles a polecat sandbox
// needs and creates the deepseek and forgejo-polecat providers from key files
// under ~/.config/gt (gt-tpg0p.53). make openshell-providers runs this command.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/openshellprov"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, openshellprov.ProcessHost))
}

// run is main without the process: the arguments and streams are given, and h
// supplies the working directory, environment, PATH, filesystem and the exec
// seam every openshell and gt call goes through.
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
	// checkout has no profiles to import, and the import needs the result.
	paths, err := openshellprov.ResolvePaths(h)
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	if err := openshellprov.NewRun(h, opts, paths, out, errw).Execute(); err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	return 0
}
