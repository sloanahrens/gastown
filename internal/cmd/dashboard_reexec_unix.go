//go:build !windows

package cmd

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

const dashboardCanReexec = true

func fileInode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// dashboardListener serves on the socket a previous dashboard handed over when
// fd is set, and binds addr otherwise. inherited reports which.
func dashboardListener(addr, fd string) (ln net.Listener, inherited bool, err error) {
	if fd == "" {
		ln, err = net.Listen("tcp", addr)
		return ln, false, err
	}
	n, err := strconv.Atoi(fd)
	if err != nil {
		return nil, false, fmt.Errorf("%s=%q: %w", dashboardListenFDEnv, fd, err)
	}
	f := os.NewFile(uintptr(n), "dashboard-listener")
	defer f.Close()
	ln, err = net.FileListener(f)
	return ln, true, err
}

// reexecSelf replaces this process with a fresh run of the same binary and
// arguments, handing it the listening socket. It returns only on failure, with
// this process still serving.
func reexecSelf(path string, ln net.Listener) error {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return fmt.Errorf("listener is %T, not a TCP listener", ln)
	}
	f, err := tl.File()
	if err != nil {
		return err
	}
	fd := int(f.Fd()) // Fd puts the shared socket in blocking mode; undone below
	restore := func() {
		_ = syscall.SetNonblock(fd, true)
		_ = f.Close()
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil { // clear close-on-exec
		restore()
		return err
	}
	err = syscall.Exec(path, os.Args, execEnv(os.Environ(), fd))
	restore()
	return err
}
