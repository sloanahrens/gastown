// Command gomodrationale fails a branch whose commit changes a go.mod or
// go.sum file without a rationale line in its message, one of `make
// presubmit`'s checks (gt-b9j3o.6). The Makefile hands it the branch's base;
// internal/presubmit reads the commits and judges them, so a polecat learns
// that its dependency change is unexplained in its own presubmit rather than
// from om at landing.
//
// A branch whose commits gt done squashed carries the message of the squashed
// commit, so the rationale belongs there: the report names the message to
// amend.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/presubmit"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, gitOutput))
}

// run is main without the process: it reads the branch, reports what it judged
// and each finding, and returns the exit code. 0 means every go.mod or go.sum
// change carries a rationale line, 1 that one does not or that git could not be
// read, 2 that the flags could not be read.
func run(args []string, w io.Writer, git presubmit.Runner) int {
	fs := flag.NewFlagSet("gomodrationale", flag.ContinueOnError)
	fs.SetOutput(w)
	base := fs.String("base", "origin/main", "ref the branch is compared against")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" {
		fmt.Fprintf(w, "gomodrationale: -base names the ref the branch is compared against\n")
		return 2
	}
	commits, err := presubmit.ReadCommits(git, *base, "HEAD")
	if err != nil {
		fmt.Fprintf(w, "gomodrationale: cannot read the branch: %v\n", err)
		return 1
	}
	return report(w, presubmit.MissingRationales(commits))
}

// report writes what the check judged, on every run, so a run that judged no
// commit reads as that rather than as a clean branch, then each finding. It
// returns 1 when there is a finding.
func report(w io.Writer, res presubmit.RationaleResult) int {
	fmt.Fprintf(w, "gomodrationale: judged %d commit(s) changing a go.mod or go.sum, %d without a rationale line\n", res.Judged, len(res.Missing))
	for _, f := range res.Missing {
		fmt.Fprintf(w, "%s %s: changes %s with no rationale\n", shortSHA(f.SHA), f.Subject, strings.Join(f.Files, ", "))
	}
	if len(res.Missing) > 0 {
		fmt.Fprintf(w, "gomodrationale: %s and add a line such as \"Rationale: <why the module graph moved>\"\n", fix(len(res.Missing)))
		return 1
	}
	return 0
}

// shortSHA abbreviates a commit id to the twelve characters git prints, so a
// finding names a commit that can be pasted into git show.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// fix names the rewrite that repairs the findings: an amend of the one commit,
// or a rewrite of every flagged message when the branch flagged more.
func fix(n int) string {
	if n == 1 {
		return "amend the commit message"
	}
	return "rewrite the message of each of those commits"
}

// gitOutput runs git in the working directory. Standard error reaches the
// terminal so git's own message is visible; only standard output is returned,
// never echoed into an error, since captured output can hold a secret.
func gitOutput(args []string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}
