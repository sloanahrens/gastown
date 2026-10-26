// Command image-supervisor runs the sandbox image build for
// scripts/install-gt.sh under an alarm (gt-k6v12).
//
//	image-supervisor --alarm SECONDS [--lock DIR] [--gt PATH --fingerprint FP] -- COMMAND [ARG...]
//
// It detaches (a session of its own, no inherited descriptors, so a detached
// build never holds the install lock past the install), runs COMMAND in a
// process group of its own, and on the alarm stops the whole group and removes
// the lock DIR once the group is gone. It exits with the build's status,
// 128+signal if a signal ended it, or 124 when the alarm did. With a
// fingerprint it also raises an escalation for a build that did not finish and
// clears it for one that did.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/imagebuild"
)

func main() {
	imagebuild.Detach()
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("image-supervisor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	alarm := fs.Int("alarm", 0, "seconds the build may run before it is stopped")
	lock := fs.String("lock", "", "lock directory the build takes; removed after a stop that it names")
	gt := fs.String("gt", "", "gt binary that raises and clears the escalation")
	fingerprint := fs.String("fingerprint", "", "escalation fingerprint for a build that does not finish")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *alarm <= 0 || fs.NArg() == 0 || (*fingerprint != "") != (*gt != "") {
		fmt.Fprintln(stderr, "usage: image-supervisor --alarm SECONDS [--lock DIR] [--gt PATH --fingerprint FP] -- COMMAND [ARG...]")
		return 2
	}
	return imagebuild.Run(imagebuild.Config{
		Alarm:       time.Duration(*alarm) * time.Second,
		Lock:        *lock,
		GT:          *gt,
		Fingerprint: *fingerprint,
		Command:     fs.Args(),
		Stderr:      stderr,
	})
}
