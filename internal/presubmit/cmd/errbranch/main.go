// Command errbranch fails a branch that adds an error return no unit test
// drives, one of `make presubmit`'s checks (gt-b9j3o.5). The Makefile hands it
// the branch's own diff against PRESUBMIT_BASE and the cover profile `go test`
// wrote for the packages the branch changed; internal/presubmit judges them,
// so a polecat learns of the untested failure path in its own presubmit rather
// than from om at landing.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/presubmit"
)

func main() {
	base := flag.String("base", "origin/main", "ref the branch is compared against")
	cover := flag.String("cover", "", "go test cover profile of the changed packages")
	flag.Parse()
	if *cover == "" {
		failf("errbranch: -cover names the profile of the changed packages' tests")
	}
	profile, err := os.Open(*cover)
	if err != nil {
		failf("errbranch: %v", err)
	}
	defer profile.Close()

	diff := exec.Command("git", "-c", "core.quotePath=false",
		"diff", "--no-color", "--no-ext-diff", "--unified=0", *base+"...HEAD")
	diff.Stderr = os.Stderr
	out, err := diff.Output()
	if err != nil {
		failf("errbranch: git diff %s...HEAD failed: %v", *base, err)
	}

	module, err := modulePath()
	if err != nil {
		failf("errbranch: %v", err)
	}
	res, err := presubmit.UntestedErrorBranches(bytes.NewReader(out), profile, module)
	if err != nil {
		failf("errbranch: %v", err)
	}
	if err := report(os.Stderr, res); err != nil {
		failf("%v", err)
	}
}

// report writes what the check judged, on every run, so a check that could see nothing reads as
// that and not as a clean branch, then each finding. It returns an error when there is a finding.
func report(w io.Writer, res presubmit.Result) error {
	fmt.Fprintf(w, "errbranch: judged %d of %d added error returns in Go sources (%d in quoted paths, not read)\n",
		res.Judged, res.Added, res.Quoted)
	if len(res.Findings) == 0 {
		return nil
	}
	for _, f := range res.Findings {
		fmt.Fprintf(w, "%s:%d: added error branch no test drives: %s\n", f.Path, f.Line, f.Text)
	}
	return fmt.Errorf("errbranch: %d added error branch no test drives; add a unit test that takes the failure path", len(res.Findings))
}

// modulePath is the module the profile names files by, read from the module itself.
func modulePath() (string, error) {
	out, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		return "", fmt.Errorf("go list -m: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
