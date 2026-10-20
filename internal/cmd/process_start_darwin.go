package cmd

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStart returns an opaque token for when pid started, or "" when it
// cannot be read. Two reads of the same process agree; a process that later
// reuses the PID reads differently. It is read in-process, so the capacity
// scan spawns nothing.
func processStart(pid int) string {
	if pid <= 0 {
		return ""
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return ""
	}
	start := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", start.Sec, start.Usec)
}
