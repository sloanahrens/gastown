//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// execEntrypointArgv replaces this process with argv, so the daemon is the
// container's process and receives its signals. It returns only when the exec
// fails.
func execEntrypointArgv(argv []string) error {
	return syscall.Exec(argv[0], argv, os.Environ())
}
