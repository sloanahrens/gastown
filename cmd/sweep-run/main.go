// Command sweep-run is the entrypoint of the container that runs one rig's tier
// sweep off the host: it clones the rig's bare repository at the commit, runs
// internal/tiersweep in this process, and leaves result.json and the logs in its
// output directory for the daemon to read (gt-dibnm).
//
//	sweep-run --rig R --sha S [--tiers a,b] [--jobs N] [--out DIR] [--repo DIR]
//
// A flag the command line leaves out takes the value the rendered container sets
// in GT_SWEEP_RIG, GT_SWEEP_SHA, GT_SWEEP_TIERS or GT_SWEEP_JOBS, which is how
// both backends pass a run (internal/sweeprunner). The exit code is 0 whenever a
// result was written, whatever its verdicts; Run states the non-zero ones.
package main

import (
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/sweeprunner"
)

func main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr, processDeps()))
}

// processDeps is the real process and filesystem behind the runner's seams.
func processDeps() Deps {
	return Deps{
		Clone:       gitClone,
		Sweep:       processSweep,
		WriteResult: sweeprunner.Write,
		MkdirTemp:   os.MkdirTemp,
		MkdirAll:    os.MkdirAll,
		RemoveAll:   os.RemoveAll,
		Environ:     os.Environ(),
		Now:         time.Now,
	}
}
