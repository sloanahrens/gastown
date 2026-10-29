// Command uninstall-gt names the directory the installed gt and bd binaries
// are removed from, and refuses a directory a run must not touch.
//
// The directory comes from --install-dir, $INSTALL_GT_BIN_DIR, $INSTALL_DIR or
// the default $HOME/.local/bin, and the run prints it with what named it.
//
// Removal is not wired yet: this slice resolves and prints the target, so a
// run exits 0 having removed nothing, and gt-tpg0p.62 removes the two binaries.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	// "would remove" until gt-tpg0p.62 wires the removal: a target line and a
	// zero exit alone read as a completed uninstall.
	fmt.Fprintf(stdout, "%s: target %s (from %s); would remove %s and %s\n",
		uninstallgt.Program, target.Dir, target.Source,
		filepath.Join(target.Dir, "gt"), filepath.Join(target.Dir, "bd"))
	return uninstallgt.ExitOK
}
