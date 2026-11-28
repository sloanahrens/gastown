// Command gt-hostload appends one host load line and then one CPU line to
// per-day files, for a launchd job that runs it once a minute (gt-emjwp9,
// gt-fjznor).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, time.Now, sysctl))
}

// sysctl bounds each read, so a hung sysctl cannot hold the sampler past its
// minute.
func sysctl(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // G204: name and args are the sampler's fixed sysctl line
	return string(out), err
}

// sysctlTimeout bounds each sysctl read; the CPU reads keep their own bounds.
const sysctlTimeout = 5 * time.Second

// run appends the load line and then the CPU line to dir's day files and
// returns 0, or names each failing step in one stderr line and returns 1.
func run(args []string, stderr io.Writer, now func() time.Time, read func(timeout time.Duration, name string, args ...string) (string, error)) int {
	fs := flag.NewFlagSet("gt-hostload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory of the per-day host load and CPU files")
	if err := fs.Parse(args); err != nil {
		return fail(stderr, "flags", err)
	}
	if *dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fail(stderr, "home", err)
		}
		*dir = filepath.Join(home, "gt", ".runtime", "host-load")
	}
	text, err := read(sysctlTimeout, "sysctl", "-n", "vm.loadavg")
	if err != nil {
		return fail(stderr, "vm.loadavg", err)
	}
	load, err := hostload.Parse(text)
	if err != nil {
		return fail(stderr, "vm.loadavg", err)
	}
	cores, err := read(sysctlTimeout, "sysctl", "-n", "hw.ncpu")
	if err != nil {
		return fail(stderr, "hw.ncpu", err)
	}
	ncpu, err := strconv.Atoi(strings.TrimSpace(cores))
	if err != nil {
		return fail(stderr, "hw.ncpu", err)
	}
	t := now()
	if err := hostload.Append(*dir, t, hostload.Line(t, load, ncpu)); err != nil {
		return fail(stderr, "append", err)
	}
	rec, errs := collectCPU(read)
	for _, err := range errs {
		fmt.Fprintf(stderr, "gt-hostload: %v\n", err)
	}
	if err := hostload.Append(filepath.Join(*dir, "cpu"), t, hostload.CPULine(t, rec)); err != nil {
		fmt.Fprintf(stderr, "gt-hostload: cpu append: %v\n", err)
		return 1
	}
	if len(errs) > 0 {
		return 1
	}
	return 0
}

func fail(stderr io.Writer, step string, err error) int {
	fmt.Fprintf(stderr, "gt-hostload: %s: %v\n", step, err)
	return 1
}
