// Command cpuslot runs one build command under the town's CPU-class slot when
// it starts from a crew checkout, and runs it unchanged everywhere else. The
// gate and presubmit recipes call it ahead of their go build, vet and test
// steps, so a burst of crew worktrees building at once queues instead of
// starving the daemon, bd and the dispatcher (gt-bq68w).
//
// The wrapper's own flags come before the command, and leading VAR=value
// assignments are env(1) assignments the command runs under:
//
//	go run ./internal/slot/cmd/cpuslot GT_TEST_DOCKER=0 go test ./...
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/workspace"
)

func main() {
	timeout := flag.Duration("timeout", slot.DefaultCPUClassWait, "how long to wait for a CPU-class slot before failing")
	flag.Parse()

	code, err := run(*timeout, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "cpuslot:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// host is what run needs from its process, injected so a test drives every
// branch without changing its own process's directory.
type host struct {
	// getwd and find locate the town above the caller.
	getwd func() (string, error)
	find  func(string) (string, error)
	// run executes the command outside the CPU class, unchanged.
	run func([]string) (int, error)
	// queued executes the command after taking a CPU-class slot.
	queued func(townRoot, role string, args []string) (int, error)
	// warn receives the notice a failed town lookup prints before the command
	// runs unchanged.
	warn io.Writer
}

// run decides whether the caller's checkout is a crew worktree and runs args
// under the CPU class when it is; every other tree gets the command untouched.
func run(timeout time.Duration, args []string) (int, error) {
	return runFrom(args, host{
		getwd: os.Getwd,
		find:  workspace.Find,
		run: func(args []string) (int, error) {
			return slot.RunCommandUnslotted(args, os.Getenv("PATH"), os.Stdin, os.Stdout, os.Stderr)
		},
		queued: func(townRoot, role string, args []string) (int, error) {
			return slot.RunCPU(townRoot, slot.CPURunOptions{
				Role:    role,
				Timeout: timeout,
				Size:    slot.CPUClassSize(townRoot),
				Args:    args,
				Path:    os.Getenv("PATH"),
				Stdin:   os.Stdin,
				Stdout:  os.Stdout,
				Stderr:  os.Stderr,
			})
		},
		warn: os.Stderr,
	})
}

// runFrom runs args under a CPU-class slot only when cwd is inside a crew
// checkout of the town find reports. A town that cannot be read is not a crew
// checkout: the command runs unchanged, since the class is a load bound and a
// build outside it must not fail over the bound's own books.
func runFrom(args []string, h host) (int, error) {
	if len(args) == 0 {
		return 0, fmt.Errorf("no command given")
	}
	cwd, err := h.getwd()
	if err != nil {
		return 0, fmt.Errorf("reading the working directory: %w", err)
	}
	townRoot, err := h.find(cwd)
	if err != nil {
		fmt.Fprintf(h.warn, "cpuslot: looking for the town above %s: %v; running the command without a CPU-class slot\n", cwd, err)
		return h.run(args)
	}
	if townRoot == "" {
		return h.run(args)
	}
	role, crew := slot.CPUBuildRole(townRoot, cwd)
	if !crew {
		return h.run(args)
	}
	return h.queued(townRoot, role, args)
}
