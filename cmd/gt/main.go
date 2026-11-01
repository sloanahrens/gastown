// gt is the Gas Town CLI for managing multi-agent workspaces.
package main

import (
	"os"
	// The gt binary carries its own time zone database. The daemon container's
	// base image has no /usr/share/zoneinfo, and a zone name in the town's
	// settings (polecat_pool.timezone) then stopped the daemon at boot.
	_ "time/tzdata"

	"github.com/steveyegge/gastown/internal/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
