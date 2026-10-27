// Command town-metrics runs one town-metrics hour: the single-flight lock, the
// snapshot and the analysis when it is due. The hour is `gt metrics run`, so
// this program hands its process to that command instead of carrying a second
// copy of the work (gt-hg64t, gt-98pgy). plugin.md beside it is the schedule and
// the contract.
//
// The daemon execs a plugin's binary with no shell and no arguments, from the
// plugin directory, with GT_TOWN_ROOT set (gt-tpg0p.1); `gt metrics run`
// resolves the town from either, which is what the shell wrapper's `cd` did.
// make build writes bin/town-metrics and gt plugin sync copies it to the town,
// so a sync ahead of a build ships a plugin whose entry point is missing.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

func main() {
	os.Exit(runner{lookPath: exec.LookPath, exec: syscall.Exec}.run(os.Environ(), os.Stderr))
}

// runner carries the seams a unit test replaces: lookPath answers without a
// PATH, and exec records the call instead of ending the test process.
type runner struct {
	lookPath func(string) (string, error)
	exec     func(path string, argv, env []string) error
}

// run replaces this process with `gt metrics run`, forwarding env, and returns
// the process status only when the exec could not start. A gt that is not on
// PATH fails the hour rather than skipping it, because a skipped run reads as a
// clean hour that appended nothing.
func (r runner) run(env []string, stderr io.Writer) int {
	path, err := r.lookPath("gt")
	if err != nil {
		fmt.Fprintf(stderr, "town-metrics: cannot find gt: %v\n", err)
		return 1
	}
	// A real exec returns only when it failed.
	if err := r.exec(path, []string{"gt", "metrics", "run"}, env); err != nil {
		fmt.Fprintf(stderr, "town-metrics: cannot exec %s: %v\n", path, err)
		return 1
	}
	return 0
}
