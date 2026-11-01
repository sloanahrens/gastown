// Command cpuslot runs one build command under the town's CPU-class slot when
// it starts from a crew checkout, and runs it unchanged everywhere else. The
// gate and presubmit recipes call it ahead of their go build, vet and test
// steps, so a burst of crew worktrees building at once queues instead of
// starving the daemon, bd and the dispatcher (gt-bq68w).
//
// The wrapper's own flags come before the command, and leading VAR=value
// assignments are env(1) assignments the command runs under:
//
//	go run ./internal/slot/cmd/cpuslot -dir tracker GT_TEST_DOCKER=0 go test ./...
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/workspace"
)

func main() {
	timeout := flag.Duration("timeout", slot.DefaultCPUClassWait, "how long to wait for a CPU-class slot before failing")
	dir := flag.String("dir", "", "working directory for the command (default: the wrapper's own)")
	flag.Parse()

	code, err := run(*timeout, *dir, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "cpuslot:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// run decides whether the caller's checkout is a crew worktree and runs args
// under the CPU class when it is. Every other tree — a polecat's sandbox
// checkout, the landing worker's gate, a run outside a town — gets the command
// untouched, so the wrapper changes nothing there.
func run(timeout time.Duration, dir string, args []string) (int, error) {
	return runFrom(timeout, dir, args, os.Getwd, workspace.Find)
}

// runFrom is run with the working directory and the town lookup injected, so a
// test drives the failure paths without changing its own process's directory.
// A town lookup that fails refuses rather than running the command unqueued: a
// crew build whose tree could not be read is exactly the build the class must
// not let loose on the host.
func runFrom(timeout time.Duration, dir string, args []string, getwd func() (string, error), find func(string) (string, error)) (int, error) {
	if len(args) == 0 {
		return 0, fmt.Errorf("no command given")
	}
	unslotted := func() (int, error) {
		return slot.RunCommandUnslotted(args, os.Getenv("PATH"), os.Stdin, os.Stdout, os.Stderr)
	}
	cwd, err := getwd()
	if err != nil {
		return 0, fmt.Errorf("reading the working directory: %w", err)
	}
	townRoot, err := find(cwd)
	if err != nil {
		return 0, fmt.Errorf("looking for the town above %s: %w", cwd, err)
	}
	if townRoot == "" {
		return unslotted()
	}
	role, crew := slot.CPUBuildRole(townRoot, cwd)
	if !crew {
		return unslotted()
	}
	return slot.RunCPU(townRoot, slot.CPURunOptions{
		Role:    role,
		Timeout: timeout,
		Size:    slot.CPUClassSize(townRoot),
		Args:    args,
		Path:    os.Getenv("PATH"),
		Dir:     dir,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
}
