package procid

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestProbeAlive(t *testing.T) {
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
		{"any other error reads as alive", syscall.EINVAL, true},
	}
	for _, tc := range cases {
		if got := ProbeAlive(tc.err); got != tc.want {
			t.Errorf("%s: ProbeAlive(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
