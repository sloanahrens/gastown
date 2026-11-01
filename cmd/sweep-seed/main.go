// Command sweep-seed fills the sweep runner's seed volume with the container
// images the packages' tests start (gt-aw5jn). The runner's engine sits on an
// internal network and cannot pull; every run copies the seed into its own
// volume first. Run it once per engine, and again when the image list changes,
// while no sweep is running:
//
//	sweep-seed --context colima-gt-control [--image REF]...
//
// With no --image it pulls sweeprunner.DefaultSeedImages.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/sweeprunner"
)

func main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr, execStep))
}

// StepRunner runs one step's argv and returns its error; a test replaces it.
type StepRunner func(argv []string, stdout, stderr io.Writer) error

// execStep runs argv as a child process with the command's own output.
func execStep(argv []string, stdout, stderr io.Writer) error {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: argv is the docker CLI and arguments rendered by sweeprunner from validated values
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// Run parses args, renders the seed plan and runs it: each step in order until
// one fails, then the teardown steps whatever happened. It returns 0 when every
// step passed, 1 when a step failed and 2 for a command line it cannot use.
func Run(args []string, stdout, stderr io.Writer, run StepRunner) int {
	seed := sweeprunner.SeedSpec{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--context", "--image":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "sweep-seed: %s needs a value\n", args[i])
				return 2
			}
			if args[i] == "--context" {
				seed.Context = args[i+1]
			} else {
				seed.Images = append(seed.Images, args[i+1])
			}
			i++
		default:
			fmt.Fprintf(stderr, "sweep-seed: unknown argument %q\nusage: sweep-seed --context CTX [--image REF]...\n", args[i])
			return 2
		}
	}
	if len(seed.Images) == 0 {
		seed.Images = append(seed.Images, sweeprunner.DefaultSeedImages...)
	}
	plan, err := sweeprunner.Docker{}.RenderSeed(seed)
	if err != nil {
		fmt.Fprintf(stderr, "sweep-seed: %v\nusage: sweep-seed --context CTX [--image REF]...\n", err)
		return 2
	}
	// The seed is read by every run's copy step, so it is not written while a sweep
	// runs. This is a check before the first write, not a lock: a sweep that starts
	// in the moments after it is not seen, so seed while the daemon is quiet.
	var running bytes.Buffer
	if err := run([]string{"docker", "--context", seed.Context, "ps", "--filter", "name=" + sweeprunner.ContainerPrefix, "--format", "{{.Names}}"}, &running, stderr); err != nil {
		fmt.Fprintf(stderr, "sweep-seed: could not list the running sweep containers: %v\n", err)
		return 1
	}
	if names := strings.Fields(running.String()); len(names) > 0 {
		fmt.Fprintf(stderr, "sweep-seed: sweep containers are running on %s (%s); seed after they end\n", seed.Context, strings.Join(names, " "))
		return 1
	}
	failed := false
	for _, step := range plan.Steps {
		if step.Teardown {
			continue
		}
		fmt.Fprintf(stdout, "+ %s\n", strings.Join(step.Argv, " "))
		if err := run(step.Argv, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "sweep-seed: step failed: %v\n", err)
			failed = true
			break
		}
	}
	for _, step := range plan.Teardown() {
		fmt.Fprintf(stdout, "+ %s\n", strings.Join(step.Argv, " "))
		if err := run(step.Argv, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "sweep-seed: teardown failed: %v\n", err)
			failed = true
		}
	}
	if failed {
		return 1
	}
	fmt.Fprintf(stdout, "sweep-seed: %d image(s) are in %s\n", len(seed.Images), sweeprunner.SeedVolume)
	return 0
}
