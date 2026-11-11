//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

func daemonSignals() []os.Signal {
	return []os.Signal{
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
	}
}

func isLifecycleSignal(sig os.Signal) bool {
	return sig == syscall.SIGUSR1
}

func isReloadRestartSignal(sig os.Signal) bool {
	return sig == syscall.SIGUSR2
}

// isDrainSignal reports whether sig is the orchestrator's stop, which drains the
// landing pass in flight before the daemon shuts down (shutdown_drain.go).
func isDrainSignal(sig os.Signal) bool {
	return sig == syscall.SIGTERM
}
