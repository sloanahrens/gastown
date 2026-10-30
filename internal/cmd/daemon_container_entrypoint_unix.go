//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// execEntrypointArgv replaces this process with argv, so the daemon is the
// container's process and receives its signals, with env added to this
// process's environment. It returns only when the exec fails.
func execEntrypointArgv(argv []string, env []string) error {
	return syscall.Exec(argv[0], argv, mergeEnv(os.Environ(), env))
}
