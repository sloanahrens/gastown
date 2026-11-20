// Command gt-hostload appends one host load line to a per-day file, for a
// launchd job that runs it once a minute (gt-emjwp9).
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
func sysctl(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // G204: name and args are the sampler's fixed sysctl line
	return string(out), err
}

// run appends one line to dir's day file and returns 0, or names the failing
// step in one stderr line and returns 1.
func run(args []string, stderr io.Writer, now func() time.Time, read func(name string, args ...string) (string, error)) int {
	fs := flag.NewFlagSet("gt-hostload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory of the per-day host load files")
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
	text, err := read("sysctl", "-n", "vm.loadavg")
	if err != nil {
		return fail(stderr, "vm.loadavg", err)
	}
	load, err := hostload.Parse(text)
	if err != nil {
		return fail(stderr, "vm.loadavg", err)
	}
	cores, err := read("sysctl", "-n", "hw.ncpu")
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
	return 0
}

func fail(stderr io.Writer, step string, err error) int {
	fmt.Fprintf(stderr, "gt-hostload: %s: %v\n", step, err)
	return 1
}
