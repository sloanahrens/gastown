// Command tier-sweep runs the hourly tier sweep: the tiers `make gate` does not
// run, each reported as one summary line.
//
//	tier-sweep [--root dir] [shell] [integration] [race]
//
// With no tier named it runs shell, integration and race, in that order. Each
// tier's summary line goes to stdout as the tier ends; the progress lines and
// every error go to stderr. It exits 0 when every tier was GREEN, 1 when any was
// RED, and 2 for an unreadable command line, a log directory it cannot make, or
// a tier it does not offer (reached after the tiers before it have printed).
// The daemon parses the lines byte for byte (internal/daemon/tier_sweep.go).
package main

import (
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

func main() {
	os.Exit(tiersweep.Run(os.Args[1:], os.Stdout, os.Stderr, processDeps()))
}

// processDeps is the real process behind the sweep's seams.
func processDeps() tiersweep.Deps {
	return tiersweep.Deps{
		Host:     tiersweep.ProcessHost,
		Exec:     tiersweep.ProcessExec,
		ReadFile: os.ReadFile,
		Environ:  os.Environ(),
		Now:      time.Now,
	}
}
