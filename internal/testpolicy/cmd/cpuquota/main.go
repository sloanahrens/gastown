// Command cpuquota prints the CPUs the gate may use: testpolicy.CPUQuota over
// the real cgroup files, environment and runtime.NumCPU, as an integer and a
// newline (gt-035jct). The unit tier's wiring to it is a later bead.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"

	"github.com/steveyegge/gastown/internal/testpolicy"
)

// run prints the quota on out, and a note on errOut for each cgroup read that
// failed other than by absence: an absent file is normal on cgroup v1 and v2.
func run(out, errOut io.Writer, read func(string) ([]byte, error), env func(string) string, numCPU int) error {
	var noteErr error
	n := testpolicy.CPUQuota(func(path string) ([]byte, error) {
		b, err := read(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			_, werr := fmt.Fprintf(errOut, "cpuquota: %s unreadable (%v), falling back\n", path, err)
			noteErr = errors.Join(noteErr, werr)
		}
		return b, err
	}, env, numCPU)
	_, err := fmt.Fprintf(out, "%d\n", n)
	return errors.Join(err, noteErr)
}

func main() {
	if err := run(os.Stdout, os.Stderr, os.ReadFile, os.Getenv, runtime.NumCPU()); err != nil {
		fmt.Fprintln(os.Stderr, "cpuquota:", err)
		os.Exit(1)
	}
}
