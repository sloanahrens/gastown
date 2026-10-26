// Command gt-installer is scripts/install-gt.sh in Go (gt-tpg0p, decision Q3:
// make install builds and runs this program). Nothing calls it yet; the
// Makefile switch is the last bead (gt-tpg0p.14).
//
//	gt-installer [--sha REF] [--source manual|rebuild-gt|post-land]
//	             [--slot-role ROLE] [--slot-timeout SECONDS] [--dry-run]
//	             [--no-image-first]
//
// The install sections land one bead at a time (gt-tpg0p.9 onward). Until one
// reports a result, a run ends in the exit guard as failed/unexpected with
// status 1, so a caller wired to this program early fails loudly rather than
// believing an install happened.
package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/installer"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the process's one way out. The deferred OnExit is the script's EXIT
// trap: whatever happens, the last line states a result and the status is 0-3.
func run(args []string, stdout, stderr io.Writer) (code int) {
	cfg, err := installer.Parse(args)
	c := &installer.Contract{
		Stdout:   stdout,
		Stderr:   stderr,
		Expected: cfg.SHA,
		Source:   cfg.Source,
		Start:    time.Now(),
	}
	defer func() { code = int(c.OnExit(installer.Exit(code))) }()
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	// No install section has landed yet, so nothing names a result and the
	// guard ends the run.
	return int(installer.ExitFailed)
}
