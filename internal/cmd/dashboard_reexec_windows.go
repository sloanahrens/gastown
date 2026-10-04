//go:build windows

package cmd

import (
	"net"
	"os"
)

// dashboardCanReexec is false: a replaced binary cannot be exec'd over a
// running process here, so the dashboard keeps running the build it started as.
const dashboardCanReexec = false

func fileInode(os.FileInfo) uint64 { return 0 }

func dashboardListener(addr, _ string) (net.Listener, bool, error) {
	ln, err := net.Listen("tcp", addr)
	return ln, false, err
}

func reexecSelf(string, net.Listener) error { return nil }
