//go:build windows

package installpending

import "golang.org/x/sys/windows"

// stillActive is what GetExitCodeProcess reports for a process that has not
// exited.
const stillActive = 259

// processAlive reports whether pid names a running process. Windows has no
// signal 0, so the probe opens the process and reads its exit code. The
// decisions about what each probe's answer means live in process_probe.go,
// which every host's tests can drive (gt-vc34c).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return aliveOnOpenError(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	err = windows.GetExitCodeProcess(h, &code)
	return aliveOnExitCode(err, code, stillActive)
}
