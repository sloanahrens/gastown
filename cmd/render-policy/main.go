// Command render-policy renders the polecat sandbox policy for a Kubernetes
// gateway from the Docker policy (deploy/polecat/policy.yaml), so the two
// cannot drift apart. It writes the rendered policy to stdout.
//
//	render-policy [--dolt-host H] [--forgejo-host H] [--tracker-host H]
//	              [--allowed-ip IP] [BASE]
//
// It exits 0 after writing the policy, 2 for a refused command line (an unknown
// option, a flag with no value, a value it would not render), and 1 when BASE
// cannot be read or the repository root cannot be found; the reason is on
// stderr and stdout is empty.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/k8spolicy"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, k8spolicy.ProcessHost))
}

// run is main without the process: the arguments and the streams are given, and
// h supplies the working directory the repository root is found from and the
// filesystem BASE is read through. Nothing is written to out unless the whole
// render succeeded.
func run(args []string, out, errw io.Writer, h k8spolicy.Host) int {
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
	base, err := k8spolicy.ReadBase(h, opts.Base)
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	if err := k8spolicy.Validate(opts); err != nil {
		fmt.Fprintln(errw, err)
		return 2
	}
	rendered, err := k8spolicy.Render(string(base), k8spolicy.Params{
		DoltHost:    opts.DoltHost,
		ForgejoHost: opts.ForgejoHost,
		TrackerHost: opts.TrackerHost,
		TrackerPort: k8spolicy.DefaultTrackerPort,
		AllowedIP:   opts.AllowedIP,
	})
	if err != nil {
		// Validate has accepted these values, so a refusal here is the render's
		// own check disagreeing with it.
		fmt.Fprintf(errw, "%s: %v\n", k8spolicy.Name, err)
		return 1
	}
	if _, err := io.WriteString(out, rendered); err != nil {
		fmt.Fprintf(errw, "%s: writing the policy: %v\n", k8spolicy.Name, err)
		return 1
	}
	return 0
}
