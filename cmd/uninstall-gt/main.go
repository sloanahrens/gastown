// Command uninstall-gt removes exactly the installed gt and bd binaries from a
// named directory, and refuses a directory a run must not touch (gt-acdfp).
package main

import (
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/uninstallgt"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, uninstallgt.OSEnv()))
}

// run is main without the process, so a test drives the command through its
// own streams and a filesystem under a temporary directory.
func run(args []string, stdout, stderr io.Writer, env uninstallgt.Env) int {
	return uninstallgt.Run(args, stdout, stderr, env)
}
