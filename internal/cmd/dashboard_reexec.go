package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	dashboardBinaryPoll    = 3 * time.Second
	dashboardVerifyTimeout = 10 * time.Second

	// dashboardListenFDEnv names the inherited listening socket a restarted
	// dashboard serves on, so the port never closes and the rebind cannot fail.
	dashboardListenFDEnv = "GT_DASHBOARD_LISTEN_FD"
)

// execEnv is env with the inherited-listener variable set to fd. Any earlier
// value is dropped: Go keeps the first of duplicate keys at startup, so a second
// restart that only appended would read the first restart's stale fd number.
func execEnv(env []string, fd int) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, dashboardListenFDEnv+"=") {
			out = append(out, kv)
		}
	}
	return append(out, fmt.Sprintf("%s=%d", dashboardListenFDEnv, fd))
}

// binaryStamp identifies one build of the executable on disk. make install
// replaces the file, so a new inode, size or mtime means a new binary.
type binaryStamp struct {
	size int64
	mod  time.Time
	ino  uint64
}

func stampBinary(path string) (binaryStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return binaryStamp{}, err
	}
	return binaryStamp{size: fi.Size(), mod: fi.ModTime(), ino: fileInode(fi)}, nil
}

// watchBinary calls changed once, when stamp reports something different from
// the baseline and keeps reporting that new value on the next tick, so a binary
// still being written is not picked up half-copied. The baseline is read at
// once; if that fails it is retried every tick, so a file that is briefly
// missing when the watch starts does not switch the watcher off. A stamp error
// later (the instant between rename and create) is not a change either.
func watchBinary(ctx context.Context, stamp func() (binaryStamp, error), ticks <-chan time.Time, changed func()) {
	var base, pending *binaryStamp
	if cur, err := stamp(); err == nil {
		base = &cur
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		cur, err := stamp()
		switch {
		case err != nil:
			pending = nil
		case base == nil:
			base = &cur
		case cur == *base:
			pending = nil
		case pending != nil && *pending == cur:
			changed()
			return
		default:
			pending = &cur
		}
	}
}

// watchBinaryFile is watchBinary on the executable at path, polled every
// interval.
func watchBinaryFile(ctx context.Context, path string, every time.Duration, changed func()) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	watchBinary(ctx, func() (binaryStamp, error) { return stampBinary(path) }, tick.C, changed)
}

// superviseBinary restarts the dashboard in place each time the executable is
// replaced. A replacement that does not pass handoff leaves the running build
// serving and is not retried until the file changes again.
func superviseBinary(ctx context.Context, exe string, ln net.Listener, out io.Writer) {
	for ctx.Err() == nil {
		changed := false
		watchBinaryFile(ctx, exe, dashboardBinaryPoll, func() { changed = true })
		if !changed {
			return
		}
		err := handoff(
			func() error { return verifyBinary(ctx, exe) },
			func() error { return reexecSelf(exe, ln) },
			out,
		)
		if err != nil {
			fmt.Fprintf(out, "gt dashboard: not restarting, still serving the running build: %v\n", err)
		}
	}
}

// handoff checks the new binary can start, then replaces this process with it.
// exec returns only on failure, and nothing has been torn down by then.
func handoff(verify, exec func() error, out io.Writer) error {
	if err := verify(); err != nil {
		return fmt.Errorf("the new binary does not run: %w", err)
	}
	fmt.Fprintln(out, "gt dashboard: binary changed, restarting")
	if err := exec(); err != nil {
		return fmt.Errorf("re-exec: %w", err)
	}
	return nil
}

// verifyBinary runs the new binary's version command: a half-written, unsigned
// or otherwise broken file fails here instead of replacing a working dashboard.
func verifyBinary(ctx context.Context, exe string) error {
	ctx, cancel := context.WithTimeout(ctx, dashboardVerifyTimeout)
	defer cancel()
	return exec.CommandContext(ctx, exe, "version").Run()
}
