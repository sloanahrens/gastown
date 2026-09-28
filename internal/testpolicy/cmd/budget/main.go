// Command budget runs `go test -json <args>` and fails when a converted package
// (one not listed in unconverted.txt) runs longer than the budget.
//
//	go run ./internal/testpolicy/cmd/budget -- -timeout 20m ./...
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/testpolicy"
)

func main() {
	budget := flag.Duration("budget", 10*time.Second, "per-package test time limit for converted packages")
	list := flag.String("unconverted", "internal/testpolicy/unconverted.txt", "packages exempt from the budget")
	flag.Parse()

	exempt, err := testpolicy.ReadList(*list)
	if err != nil {
		fmt.Fprintln(os.Stderr, "budget:", err)
		os.Exit(2)
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
	over, scanErr := testpolicy.WatchBudget(stdout, os.Stdout, *budget, exempt, "github.com/steveyegge/gastown")
	waitErr := cmd.Wait()

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
