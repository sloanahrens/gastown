// Command integrationrefs reports which integration-tagged test files in the
// changed directories name a declaration the branch changed (gt-9p2wh7). The
// gate compiles those tests but never runs them, so the check is information:
// it prints one summary line on every run, so a run that judged nothing reads as
// that rather than as silence. A hit does not fail the run. The Makefile hands it
// the branch's base; internal/presubmit reads the branch's diff and judges it.
package main

import (
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

// run is main without the process: it judges the branch, prints the summary
// line, and returns the exit code. 0 means the check ran, whatever it found, 1
// that git could not be read, 2 that the flags could not be read.
func run(args []string, w io.Writer, git presubmit.Runner) int {
	fs := flag.NewFlagSet("integrationrefs", flag.ContinueOnError)
	fs.SetOutput(w)
	base := fs.String("base", "origin/main", "ref the branch is compared against")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" {
		fmt.Fprintf(w, "integrationrefs: -base names the ref the branch is compared against\n")
		return 2
	}
	res, err := presubmit.IntegrationRefs(git, *base)
	if err != nil {
		fmt.Fprintf(w, "integrationrefs: cannot judge the branch: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "integrationrefs: judged %d changed declaration(s) and %d integration-tagged file(s)\n", res.Declarations, res.TaggedFiles)
	return 0
}

// gitOutput runs git in the working directory. Standard error reaches the
// terminal so git's own message is visible; only standard output is returned,
// never echoed into an error, since captured output can hold a secret.
func gitOutput(args []string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}
