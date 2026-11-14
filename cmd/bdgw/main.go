// bdgw is the bd gateway: the command the bd container runs. Its modes share one
// policy, one executor and one call log. serve is the HTTP gateway the
// forwarder calls; exec runs one call for a caller already inside the container
// (the Mac wrapper's docker target), so a local call is checked and logged the
// same way; ready is the same readiness answer as GET /readyz, for the
// container's health check. The design is docs/design/bd-container.md.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// The command's exit statuses. exec returns bd's own status instead; these are
// for the modes that have nothing of bd's to pass through.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// exitRefused is the status exec returns when the gateway refused the call
// before bd started: the status the frame protocol's error frame is paired
// with.
const exitRefused = bdgateway.ExitRefused

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one mode. It takes the streams so a test can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stderr)
	case "exec":
		return runExec(args[1:], stdout, stderr)
	case "ready":
		return runReady(args[1:], stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "bdgw: unknown mode %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	// A usage line that cannot be written is the caller's output stream gone;
	// the exit status is the answer either way.
	_, _ = fmt.Fprint(w, `bdgw is the bd gateway.

  bdgw serve [flags]                  run the HTTP gateway
  bdgw exec [flags] -- <bd args>      run one bd call inside the container
  bdgw ready [flags]                  exit 0 when the gateway could serve a call
`)
}
