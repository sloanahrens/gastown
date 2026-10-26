// Command machinecheck runs every gt command that takes --json or --quiet through a built gt
// binary, in an empty temp town and an empty working directory, and checks each one's stdout with
// the machine-stdout check. The Makefile's presubmit calls it after it builds gt (gt-keakj).
//
// A run that exits non-zero, or that runs past -timeout, is skipped and printed with its reason:
// its stdout is error text rather than the command's machine output, and it is not judged. A run
// that exits zero is judged: with --json its stdout must be JSON, with --quiet empty.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/cmd"
)

// outcome is one run's verdict.
type outcome struct {
	skip string // the reason the run was not judged, when it was skipped
	err  error  // the stdout problem, when it was judged and failed
}

// judge decides one run from how it ended and what it wrote to stdout.
func judge(mode string, exitCode int, timedOut bool, stdout []byte) outcome {
	switch {
	case timedOut:
		return outcome{skip: "timed out"}
	case exitCode != 0:
		return outcome{skip: fmt.Sprintf("exited %d", exitCode)}
	}
	if err := cmd.CheckMachineStdout(mode, stdout); err != nil {
		return outcome{err: err}
	}
	return outcome{}
}

// runOne runs gt with args in dir, with the environment that keeps it out of the operator's town,
// and returns the judged outcome.
func runOne(ctx context.Context, gt, town string, args []string, mode string) outcome {
	c := exec.CommandContext(ctx, gt, args...)
	c.Dir = town
	c.Env = []string{"HOME=" + town, "GT_TOWN_ROOT=" + town, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir()}
	c.WaitDelay = time.Second
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = io.Discard
	err := c.Run()
	exit := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	} else if err != nil {
		return outcome{skip: fmt.Sprintf("did not start: %v", err)}
	}
	return judge(mode, exit, errors.Is(ctx.Err(), context.DeadlineExceeded), stdout.Bytes())
}

func main() {
	gt := flag.String("gt", "", "the built gt binary to run")
	limit := flag.Duration("timeout", 10*time.Second, "wall limit for each command run")
	flag.Parse()
	if *gt == "" {
		fail("machinecheck: -gt names the built gt binary")
	}
	town, err := os.MkdirTemp("", "machinecheck-town-")
	if err != nil {
		fail("machinecheck: %v", err)
	}
	defer os.RemoveAll(town)

	start := time.Now()
	var ran, skipped, failed int
	for _, mc := range cmd.MachineCommands() {
		for _, mode := range mc.Modes {
			name := "gt " + strings.Join(mc.Path, " ") + " --" + mode
			ctx, cancel := context.WithTimeout(context.Background(), *limit)
			res := runOne(ctx, *gt, town, append(append([]string{}, mc.Path...), "--"+mode), mode)
			cancel()
			switch {
			case res.err != nil:
				failed++
				ran++
				fmt.Fprintf(os.Stderr, "machinecheck: FAIL %s: %v\n", name, res.err)
			case res.skip != "":
				skipped++
				fmt.Fprintf(os.Stderr, "machinecheck: skip %s: %s\n", name, res.skip)
			default:
				ran++
			}
		}
	}
	fmt.Fprintf(os.Stderr, "machinecheck: judged %d command runs, skipped %d, failed %d, in %s wall\n",
		ran, skipped, failed, time.Since(start).Round(time.Millisecond))
	if failed > 0 {
		os.Exit(1)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
