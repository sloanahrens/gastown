// Command install-lock is scripts/install-gt.sh's install lock: it takes an
// exclusive flock, then becomes the install (gt-1jgci).
//
//	install-lock --lock FILE --wait SECONDS [--marker FILE] -- COMMAND [ARG...]
//
// On success the process is replaced by COMMAND with the lock fd inherited, so
// the lock lives exactly as long as the install does. It exits 75 when the lock
// did not free within SECONDS (the script maps that to its busy exit 3) and 1
// when it could not get as far as the exec.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/installlock"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("install-lock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lockPath := fs.String("lock", "", "lock file to hold exclusively; created when missing")
	wait := fs.Int("wait", 0, "seconds to wait for the lock before exiting 75")
	marker := fs.String("marker", "", "file to remove just before the command is exec'd")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *lockPath == "" || *wait < 0 || fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: install-lock --lock FILE --wait SECONDS [--marker FILE] -- COMMAND [ARG...]")
		return 2
	}
	return installlock.Run(installlock.Config{
		LockPath: *lockPath,
		Wait:     time.Duration(*wait) * time.Second,
		Marker:   *marker,
		Command:  fs.Args(),
		Env:      os.Environ(),
		Stderr:   stderr,
	})
}
