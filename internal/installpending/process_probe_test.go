package installpending

import (
	"errors"
	"testing"
)

// TestAliveOnOpenError pins the Windows "gone" rule: only the platform's
// invalid-pid error reads as dead, and every other failure — access denied
// included — fails safe to alive (gt-vc34c).
func TestAliveOnOpenError(t *testing.T) {
	t.Parallel()
	invalidID := errors.New("invalid parameter")
	accessDenied := errors.New("access denied")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"invalid pid is gone", invalidID, false},
		{"wrapped invalid pid is gone", errors.Join(errors.New("open"), invalidID), false},
		{"access denied is alive", accessDenied, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := aliveOnOpenError(tc.err, invalidID); got != tc.want {
				t.Errorf("aliveOnOpenError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestAliveOnExitCode pins the second Windows probe's rule: a failed exit-code
// read fails safe to alive, and a code other than the still-active one means
// the process has exited (gt-vc34c).
func TestAliveOnExitCode(t *testing.T) {
	t.Parallel()
	const stillActiveCode = 259
	for _, tc := range []struct {
		name string
		err  error
		code uint32
		want bool
	}{
		{"still-active code is alive", nil, stillActiveCode, true},
		{"an exit code is gone", nil, 0, false},
		{"an unreadable exit code is alive", errors.New("query failed"), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := aliveOnExitCode(tc.err, tc.code, stillActiveCode); got != tc.want {
				t.Errorf("aliveOnExitCode(err=%v, code=%d) = %v, want %v", tc.err, tc.code, got, tc.want)
			}
		})
	}
}
