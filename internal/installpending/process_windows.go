//go:build windows

package installpending

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is what GetExitCodeProcess reports for a process that has not
// exited.
const stillActive = 259

// processAlive reports whether pid names a running process. Windows has no
// signal 0, so the probe opens the process and reads its exit code: only an
// invalid pid is a "gone" answer, and every other error — access denied
// included — reads as alive, because a live writer read as dead would let a
// sling start beside the install (gt-vc34c).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
