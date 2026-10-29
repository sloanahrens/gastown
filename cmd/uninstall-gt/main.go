// Command uninstall-gt names the directory the installed gt and bd binaries
// are removed from, and refuses a directory a run must not touch.
//
// The directory comes from --install-dir, $INSTALL_GT_BIN_DIR, $INSTALL_DIR or
// the default $HOME/.local/bin, and the run prints it with what named it.
// Removing the two binaries is the next step of the port (gt-tpg0p.62).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/uninstallgt"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// run is the whole command against getenv, the seam a test supplies its own
// variables through so it never reads the developer's.
func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	opts, err := uninstallgt.Parse(args)
	if errors.Is(err, uninstallgt.ErrHelp) {
		fmt.Fprintln(stdout, uninstallgt.Usage)
		return uninstallgt.ExitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, uninstallgt.Usage)
		return uninstallgt.ExitRefused
	}
	target, err := uninstallgt.Resolve(opts, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "%s: refusing to run: %s\n", uninstallgt.Program, err)
		return uninstallgt.ExitRefused
	}
	fmt.Fprintf(stdout, "%s: target %s (from %s)\n", uninstallgt.Program, target.Dir, target.Source)
	return uninstallgt.ExitOK
}
