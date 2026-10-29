//go:build unix

package bird

import "syscall"

// execProcess replaces this process with path. claude runs in place of gt, so
// the pid the registry recorded before the exec is the claude process rather
// than a parent that outlives it.
func execProcess(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
