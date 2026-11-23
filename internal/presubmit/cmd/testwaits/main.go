// Command testwaits fails a branch that adds a test wait with no bound, one of `make presubmit`'s checks
// (gt-4ndz7z). The Makefile hands it the branch's own diff against PRESUBMIT_BASE; internal/presubmit judges
// the added test lines, so a polecat learns of the unbounded wait in its own presubmit rather than from om.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/presubmit"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, gitOutput))
}

// run is main without the process: 0 means every added test line waits within a bound, 1 that one does not or
// that the branch could not be read, 2 that the flags could not be read.
func run(args []string, w io.Writer, git presubmit.Runner) int {
	fs := flag.NewFlagSet("testwaits", flag.ContinueOnError)
	fs.SetOutput(w)
	base := fs.String("base", "origin/main", "ref the branch is compared against")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" {
		fmt.Fprintln(w, "testwaits: -base names the ref the branch is compared against")
		return 2
	}
	diff, err := presubmit.ReadDiff(git, *base)
	if err != nil {
		fmt.Fprintf(w, "testwaits: cannot read the branch: %v\n", err)
		return 1
	}
	// Files are read at HEAD, the commit the diff ends at, so the diff's line numbers are the ones parsed.
	res, err := presubmit.JudgeTestWaits(bytes.NewReader(diff), func(p string) ([]byte, error) {
		return git([]string{"show", "HEAD:" + p})
	})
	if err != nil {
		fmt.Fprintf(w, "testwaits: %v\n", err)
		return 1
	}
	// The judged count prints on every run, so a run that judged no added test
	// line reads as that rather than as a clean branch.
	fmt.Fprintf(w, "testwaits: judged %d added test line(s), %d with a wait with no bound\n", res.Judged, len(res.Findings))
	for _, f := range res.Findings {
		fmt.Fprintf(w, "%s:%d: added wait with no bound: %s\n", f.Path, f.Line, f.Text)
	}
	if len(res.Findings) == 0 {
		return 0
	}
	fmt.Fprintln(w, "testwaits: bound each wait with a select case on a deadline, a timeout, or a loop condition")
	return 1
}

// gitOutput runs git in the working directory. Standard error reaches the
// terminal so git's own message is visible; only standard output is returned,
// never echoed into an error, since captured output can hold a secret.
func gitOutput(args []string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}
