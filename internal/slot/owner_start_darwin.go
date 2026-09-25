//go:build darwin

package slot

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// processStartToken returns pid's start time as recorded by the kernel
// (kinfo_proc p_starttime, "sec.usec"). It never changes for the life of a
// process, so a live pid whose token differs from the one recorded at
// container creation is a different process that reused the pid. ok is false
// when the kernel will not say.
func processStartToken(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return "", false
	}
	st := kp.Proc.P_starttime
	if st.Sec == 0 && st.Usec == 0 {
		return "", false
	}
	return strconv.FormatInt(st.Sec, 10) + "." + strconv.FormatInt(int64(st.Usec), 10), true
}
