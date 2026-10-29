// Command render-policy renders the polecat sandbox policy for a Kubernetes
// gateway from the Docker policy (deploy/polecat/policy.yaml), so the two
// cannot drift apart. This slice is the command line and the input checks; the
// render arrives in gt-tpg0p.83, so a run that validates refuses rather than
// reporting a policy it did not write (gt-tpg0p.81).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/k8spolicy"
)

// notImplemented is what a valid run ends with until the render lands: a run
// that wrote no policy must not exit 0 as though it had.
const notImplemented = k8spolicy.Name + ": the render is not implemented yet (gt-tpg0p.83); deploy/k8s/render-policy.sh still renders the Kubernetes policy"

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, k8spolicy.ProcessHost))
}

// run is main without the process: the arguments and the stream are given, and
// h supplies the working directory the repository root is found from and the
// filesystem BASE is read through.
func run(args []string, errw io.Writer, h k8spolicy.Host) int {
	opts, err := k8spolicy.Parse(args, h)
	if err != nil {
		fmt.Fprintln(errw, err)
		var usage *k8spolicy.UsageError
		if errors.As(err, &usage) {
			return 2
		}
		return 1
	}
	// The script checked BASE before its two input rules, so a policy file it
	// cannot read is reported ahead of a flag value.
	if _, err := k8spolicy.ReadBase(h, opts.Base); err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	if err := k8spolicy.Validate(opts); err != nil {
		fmt.Fprintln(errw, err)
		return 2
	}
	fmt.Fprintln(errw, notImplemented)
	return 1
}
