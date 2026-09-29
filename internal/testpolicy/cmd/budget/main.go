// Command budget runs `go test -json <args>` and fails when a converted package
// (one not listed in unconverted.txt) runs longer than the budget.
//
//	go run ./internal/testpolicy/cmd/budget -- -timeout 20m ./...
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/testpolicy"
)

func main() {
	budget := flag.Duration("budget", 10*time.Second, "per-package test time limit for converted packages")
	list := flag.String("unconverted", "internal/testpolicy/unconverted.txt", "packages exempt from the budget")
	overList := flag.String("overbudget", "internal/testpolicy/overbudget.txt", "converted packages exempt from the budget while a bead tracks their overrun; their times are reported on every run")
	flag.Parse()

	exempt, err := testpolicy.ReadList(*list)
	if err != nil {
		fmt.Fprintln(os.Stderr, "budget:", err)
		os.Exit(2)
	}
	overEntries, err := testpolicy.ReadOverBudget(*overList)
	if err != nil {
		fmt.Fprintln(os.Stderr, "budget:", err)
		os.Exit(2)
	}
	tracked := make(map[string]string, len(overEntries))
	for _, e := range overEntries {
		tracked[e.Package] = e.Bead
	}
	cmd := exec.Command("go", append([]string{"test", "-json"}, flag.Args()...)...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "budget:", err)
		os.Exit(2)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "budget:", err)
		os.Exit(2)
	}
	over, trackedRuns, scanErr := testpolicy.WatchBudgetTracked(stdout, os.Stdout, *budget, exempt, tracked, "github.com/steveyegge/gastown")
	if scanErr != nil {
		// WatchBudget stopped reading before the child was done writing (for
		// example a single line over its 16 MB scan buffer); drain the pipe
		// so the child's next Write doesn't block on a full pipe buffer and
		// hang cmd.Wait() forever.
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()

	if len(trackedRuns) > 0 {
		fmt.Fprintf(os.Stderr, "over budget (tracked, limit %s):\n", *budget)
		for _, r := range trackedRuns {
			fmt.Fprintf(os.Stderr, "  %s took %s (%s)\n", r.Package, r.Elapsed.Round(time.Millisecond), r.Bead)
		}
	}
	for _, o := range over {
		fmt.Fprintf(os.Stderr, "BUDGET: %s took %s (limit %s); slowest:", o.Package, o.Elapsed.Round(time.Millisecond), *budget)
		for _, t := range o.Slowest {
			fmt.Fprintf(os.Stderr, " %s %s", t.Name, t.Elapsed.Round(time.Millisecond))
		}
		fmt.Fprintln(os.Stderr)
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(waitErr, &exitErr):
		os.Exit(exitErr.ExitCode())
	case waitErr != nil || scanErr != nil:
		fmt.Fprintln(os.Stderr, "budget:", waitErr, scanErr)
		os.Exit(2)
	case len(over) > 0:
		os.Exit(1)
	}
}
