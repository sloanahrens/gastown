//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// dieLike reports the signal that ended the test binary. Windows has no
// signal to re-raise, so it returns the shell's 128+N exit code.
func dieLike(sig syscall.Signal) int {
	fmt.Fprintf(os.Stderr, "budget %s: test binary killed by %v\n", execTestArg, sig)
	return 128 + int(sig)
}
