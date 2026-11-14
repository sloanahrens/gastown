//go:build !linux

package procid

import (
	"os"
	"testing"
)

// Where StartToken has no reader, no ID can be taken for this process, and an
// ID recorded with any token never reads as running. A caller that cannot
// prove a process is its own must not signal it.
func TestStartTokenUnreadableWithoutReader(t *testing.T) {
	t.Parallel()
	if _, ok := StartToken(os.Getpid()); ok {
		t.Error("start token readable for this process on a platform with no reader")
	}
	if _, ok := Of(os.Getpid()); ok {
		t.Error("Of took an ID for this process on a platform with no reader")
	}
	self := ID{PID: os.Getpid(), Start: "1"}
	if self.Running(StartToken) {
		t.Error("recorded ID reads as running with no start token reader")
	}
}
