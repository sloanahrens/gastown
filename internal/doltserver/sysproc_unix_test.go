//go:build !windows

package doltserver

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

// TestProcessIsAliveEPERM injects the signal error so a live Dolt server owned
// by another uid is testable without root.
func TestProcessIsAliveEPERM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"probe succeeded", nil, true},
		{"no such process", syscall.ESRCH, false},
		{"no such process, wrapped", fmt.Errorf("probe: %w", syscall.ESRCH), false},
		{"os.Process reports ESRCH as done", os.ErrProcessDone, false},
		{"reaped child", &os.SyscallError{Syscall: "kill", Err: syscall.ESRCH}, false},
		{"exists under another uid", syscall.EPERM, true},
		{"exists under another uid, via os.Process", &os.SyscallError{Syscall: "kill", Err: syscall.EPERM}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := processAliveAfterSignal(tc.err); got != tc.want {
				t.Errorf("processAliveAfterSignal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestProcessIsAliveCurrentProcess(t *testing.T) {
	t.Parallel()
	if !processIsAlive(os.Getpid()) {
		t.Error("processIsAlive(this process) = false, want true")
	}
}

func TestProcessIsAliveNonPositivePID(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1} {
		if processIsAlive(pid) {
			t.Errorf("processIsAlive(%d) = true, want false", pid)
		}
	}
}
