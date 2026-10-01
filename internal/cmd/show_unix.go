//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// execBdShow replaces the current process with 'bd show'.
// Resolves the correct rig directory from the bead's prefix via routes.jsonl
// so that rig-prefixed beads (e.g., myproject-abc) are found in their rig
// database rather than only the town-level hq database. (GH#2126)
func execBdShow(args []string) error {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		return fmt.Errorf("bd not found in PATH: %w", err)
	}

	invocation := currentBdShowInvocation(args)
	bdPath, err = prepareBdShowExec(bdPath, invocation, os.Getwd, os.Chdir)
	if err != nil {
		return err
	}

	return syscall.Exec(bdPath, invocation.ExecArgs, invocation.Env)
}

// prepareBdShowExec anchors a relative bdPath to the working directory (read
// through getwd), then moves into the invocation's directory through chdir, so
// the exec that follows finds both.
func prepareBdShowExec(bdPath string, invocation bdShowInvocation, getwd func() (string, error), chdir func(string) error) (string, error) {
	if !filepath.IsAbs(bdPath) {
		wd, err := getwd()
		if err != nil {
			return "", fmt.Errorf("resolve bd path %q: %w", bdPath, err)
		}
		bdPath = filepath.Join(wd, bdPath)
	}
	if invocation.Dir != "" {
		if err := chdir(invocation.Dir); err != nil {
			return "", fmt.Errorf("chdir %s: %w", invocation.Dir, err)
		}
	}
	return bdPath, nil
}
