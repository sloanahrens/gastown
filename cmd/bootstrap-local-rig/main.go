// Command bootstrap-local-rig creates a clean Gas Town rig from a local source
// repo by running gt rig add with --local-repo, then optionally records the
// polecat role agent in the rig settings. Ported from
// scripts/bootstrap-local-rig.sh (gt-tpg0p.95).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/rigbootstrap"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, osExec))
}

// run is main without the process, so a test drives the command through its own
// streams and a fake gt and git.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, execFn rigbootstrap.Exec) int {
	opts, err := rigbootstrap.ParseOptions(args)
	if errors.Is(err, rigbootstrap.ErrHelp) {
		_, _ = fmt.Fprint(stdout, rigbootstrap.Usage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		_, _ = fmt.Fprint(stderr, rigbootstrap.Usage)
		return 2
	}
	return rigbootstrap.Run(ctx, opts, rigbootstrap.Deps{
		Exec:   execFn,
		Files:  rigbootstrap.OSFiles{},
		Stdout: stdout,
		Stderr: stderr,
	})
}

// osExec runs a program on the host with the command's stdin and the given
// output streams. It returns the program's exit code, and an error only when the
// program could not start.
func osExec(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: gt and git are the operator's own tools, named on the command line
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return exitStatus(cmd.Run())
}

// exitStatus is the exit code of a program that ran, or the error when it could
// not start.
func exitStatus(err error) (int, error) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}
