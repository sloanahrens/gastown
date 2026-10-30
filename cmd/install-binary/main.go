// Command install-binary replaces an installed binary atomically and signs the
// copy with the town's identity before it lands (gt-tpg0p.68). It is the Go
// port of scripts/install-binary.sh, the replace the Makefile's install-local
// recipe runs:
//
//	install-binary <built-binary> <install-dir> [name]
//
// The copy is written beside the target and renamed over it, so a concurrent
// reader sees the old binary or the new one, never a partial copy (gt-0het).
package main

import (
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/binaryinstall"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	return binaryinstall.Run(args, stderr, binaryinstall.Installer{}, binaryinstall.Signer{})
}
