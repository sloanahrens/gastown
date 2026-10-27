// Command town-metrics is the plugin's entry point, built into the program
// plugin.md's [execution] block names; plugin.md beside it is the schedule and
// the contract. The hour has one implementation, `gt metrics run`, so this
// program hands its process to that command rather than carrying a second copy
// of the work (gt-hg64t).
//
// The runner starts a plugin's binary in the plugin's own directory inside the
// town with GT_TOWN_ROOT set (gt-tpg0p.1). The hour's record lands in the town
// that directory resolves to, so this program names that town before the
// handoff and refuses a run where the two disagree.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/steveyegge/gastown/internal/workspace"
)

func main() {
	os.Exit(runner{
		lookPath: exec.LookPath,
		exec:     syscall.Exec,
		townRoot: func() (string, error) { return resolveTown(workspace.FindFromCwdOrError, os.Getenv) },
	}.run(os.Environ(), os.Stderr))
}

// runner carries the seams a unit test replaces: lookPath answers without a
// PATH, exec records the call instead of ending the test process, and townRoot
// names the town the hour writes to.
type runner struct {
	lookPath func(string) (string, error)
	exec     func(path string, argv, env []string) error
	townRoot func() (string, error)
}

// run replaces this process with `gt metrics run`, forwarding env, and returns
// the process status only when the exec could not start. A gt that is not on
// PATH fails the hour rather than skipping it, because a skipped run reads as a
// clean hour that appended nothing, and an hour placed in no town is the same
// silent record.
func (r runner) run(env []string, stderr io.Writer) int {
	if _, err := r.townRoot(); err != nil {
		fmt.Fprintf(stderr, "town-metrics: cannot name the town: %v\n", err)
		return 1
	}
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

// resolveTown names the town the hour writes to: the one this process resolves,
// which is the one `gt metrics run` resolves from the same directory and
// environment. The runner's contract names that town twice, as the plugin's
// directory and as GT_TOWN_ROOT, so a pair that disagrees fails here rather
// than let the record land in a town the runner never promised.
func resolveTown(resolve func() (string, error), getenv func(string) string) (string, error) {
	town, err := resolve()
	if err != nil {
		return "", err
	}
	if fromEnv := getenv("GT_TOWN_ROOT"); fromEnv != "" && fromEnv != town {
		return "", fmt.Errorf("the plugin directory is in town %q but GT_TOWN_ROOT names %q", town, fromEnv)
	}
	return town, nil
}
