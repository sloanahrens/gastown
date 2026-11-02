// Command isolation-verdict prints the isolation-probe verdict for a run
// directory; docs/guides/gke-isolation-probes.md states the rules.
//
// Usage: isolation-verdict DIR NODE. It prints one line per probe and then the
// VERDICT line on stdout, and why an input was unusable on stderr. It exits 0
// only for VERDICT NO-KNOWN-FAILURE, 1 for VERDICT INCONCLUSIVE or VERDICT
// FAIL, and 2 for a refused command line.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/isolationprobe"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the process minus os.Exit, so a test reads the output and the exit
// code it would have exited with.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: isolation-verdict DIR NODE")
		return 2
	}
	v := isolationprobe.Evaluate(args[0], args[1])
	for _, line := range v.Lines() {
		fmt.Fprintln(stdout, line)
	}
	for _, problem := range v.Problems {
		fmt.Fprintln(stderr, "isolation-verdict: "+problem)
	}
	return v.ExitCode()
}
