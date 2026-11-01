//go:build !windows

package slot

import "golang.org/x/sys/unix"

// setCPUClassPriority sets this process's niceness to n —
// setpriority(PRIO_PROCESS, 0, n), the priority nice(1) -n 10 gives a process
// at the default niceness. The child inherits it across fork and exec.
func setCPUClassPriority(n int) error {
	return unix.Setpriority(unix.PRIO_PROCESS, 0, n)
}
