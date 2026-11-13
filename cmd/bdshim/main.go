// bdshim stands in for bd on PATH and sends each command to the store that
// owns its rig: the Dolt store through bd.real, or the tracker through a verb
// handler.
package main

import (
	"os"

	"github.com/steveyegge/gastown/internal/bdshim"
)

// Build and Commit receive the build id the image stamps the shim with: the
// same two halves bd.real carries, since the shim stands in for that bd. They
// are declared so the linker's -X reaches them; a -X naming no variable is
// dropped without a word. No verb of the shim reads them yet.
var (
	Build  string
	Commit string
)

func main() {
	os.Exit(bdshim.OsMain(os.Args[1:]))
}
