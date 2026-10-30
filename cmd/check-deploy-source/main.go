// Command check-deploy-source refuses to install a gt built from unmerged code
// (gt-tpg0p.64). It is the precondition of make install-local (gt-o848l).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/deploysource"
)

const (
	name  = "check-deploy-source"
	usage = "usage: " + name
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, deploysource.ExecGit{}, os.Getenv))
}

// run is main without the process: g runs the git commands in the repository
// being built, getenv reads the switches, and errw takes what the check
// prints. The check writes nothing to stdout, as the script did.
func run(args []string, errw io.Writer, g deploysource.Git, getenv func(string) string) int {
	if len(args) > 0 {
		fmt.Fprintf(errw, "%s: unknown argument: %s (%s)\n", name, args[0], usage)
		return 2
	}
	err := deploysource.Run(g, deploysource.OptionsFromEnv(getenv), errw)
	if err == nil {
		return 0
	}
	var refusal *deploysource.Refusal
	if errors.As(err, &refusal) {
		// Run printed the refusal and its hints already.
		return 1
	}
	fmt.Fprintf(errw, "%s: %v\n", name, err)
	return 1
}
