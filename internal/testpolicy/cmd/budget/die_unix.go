//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// reraised are the signals this process can die of the way the test binary
// did: the Go runtime's default action for each is to terminate by that same
// signal. (SIGQUIT, SIGABRT and the synchronous faults make the runtime print
// a traceback of this process and exit 2 instead.)
var reraised = map[syscall.Signal]bool{
	syscall.SIGKILL: true, syscall.SIGTERM: true, syscall.SIGINT: true, syscall.SIGHUP: true,
}

// dieLike ends this process the way a signal ended the test binary, so go
// test reports "signal: killed" (or the like) exactly as it would without
// the wrapper. For a signal it cannot re-raise, it says which signal it was
// on stderr and returns the shell's 128+N exit code instead.
func dieLike(sig syscall.Signal) int {
	if reraised[sig] {
		signal.Reset(sig)
		_ = syscall.Kill(os.Getpid(), sig)
		// Delivery to this process is asynchronous (another thread may take
		// it); the process ends within microseconds. The wait only bounds
		// the fallback below should the signal somehow not be fatal.
		time.Sleep(time.Second)
	}
	fmt.Fprintf(os.Stderr, "budget %s: test binary killed by %v\n", execTestArg, sig)
	return 128 + int(sig)
}
