// bdshim stands in for bd on PATH and sends each command to the store that
// owns its rig: the Dolt store through bd.real, or the tracker through a verb
// handler.
package main

import (
	"os"

	"github.com/steveyegge/gastown/internal/bdshim"
)

func main() {
	os.Exit(bdshim.OsMain(os.Args[1:]))
}
