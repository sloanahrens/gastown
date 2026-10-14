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

	"github.com/steveyegge/gastown/internal/procutil"
)

const (
	dashboardPoll          = 3 * time.Second
	dashboardVerifyTimeout = 10 * time.Second

	// A restart waits dashboardDrainBound for the polls in flight to finish on
	// their own (most are a bd or git read that ends in under a second; the bound
	// is for the one that hangs), then kills their children and waits
	// dashboardKillBound for the polls to return.
	dashboardDrainBound = 10 * time.Second
	dashboardKillBound  = 5 * time.Second

	// dashboardListenFDEnv names the inherited listening socket a restarted
	// dashboard serves on, so the port never closes and the rebind cannot fail.
	dashboardListenFDEnv = "GT_DASHBOARD_LISTEN_FD"
)

var dashboardQuiesceBounds = procutil.QuiesceBounds{Drain: dashboardDrainBound, Kill: dashboardKillBound}

// dashboardLogf writes lines to out under the dashboard's log prefix.
func dashboardLogf(out io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(out, "gt dashboard: "+format+"\n", args...)
	}
}

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

// fileStamp identifies one version of a file on disk: a new inode, size or mtime
// means the file was replaced. make install replaces the executable that way, a
// rig added or removed rewrites the town registry that way, and the landing
// panels re-read a file only once its stamp moves.
type fileStamp struct {
	size int64
	mod  time.Time
	ino  uint64
}

// stampFile reads the stamp of the file at path; a file that cannot be read has
// the zero stamp and its error.
func stampFile(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{size: fi.Size(), mod: fi.ModTime(), ino: fileInode(fi)}, nil
}

// watchedFile is one file a restart follows: how to stamp it, and the name the
// restart log line calls it.
type watchedFile struct {
	name  string
	stamp func() (fileStamp, error)
}

// watchPath is a watchedFile on the file at path.
func watchPath(name, path string) watchedFile {
	return watchedFile{name: name, stamp: func() (fileStamp, error) { return stampFile(path) }}
}

// watchFiles calls changed with the name of the first watched file whose stamp
// reports something different from its baseline and keeps reporting that new
// value on the next tick, so a file still being written is not picked up
// half-written. Each file is tracked on its own, so one file mid-write does not
// hide a settled change to another. A baseline is read at once; if that fails it
// is retried every tick, so a file that is briefly missing when the watch starts
// does not switch the watch off. A stamp error later (the instant between rename
// and create) is not a change either.
func watchFiles(ctx context.Context, files []watchedFile, ticks <-chan time.Time, changed func(name string)) {
	base := make([]*fileStamp, len(files))
	pending := make([]*fileStamp, len(files))
	for i, f := range files {
		if cur, err := f.stamp(); err == nil {
			base[i] = &cur
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		for i, f := range files {
			cur, err := f.stamp()
			switch {
			case err != nil:
				pending[i] = nil
			case base[i] == nil:
				base[i] = &cur
			case cur == *base[i]:
				pending[i] = nil
			case pending[i] != nil && *pending[i] == cur:
				changed(f.name)
				return
			default:
				pending[i] = &cur
			}
		}
	}
}

// watchFilesAt is watchFiles on the files named, polled every interval.
func watchFilesAt(ctx context.Context, files []watchedFile, every time.Duration, changed func(name string)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	watchFiles(ctx, files, tick.C, changed)
}

// restartWatches are the files a dashboard restart follows. The registry is
// read once at process start (session.InitRegistry), so a dashboard that
// predates a rig computes session names for it from an empty prefix (gt-cslma).
func restartWatches(exe, registry string) []watchedFile {
	return []watchedFile{
		watchPath("binary", exe),
		watchPath("town registry", registry),
	}
}

// superviseRestart restarts the dashboard in place each time a watched file is
// replaced. A change that does not pass handoff leaves the running build serving
// and is not retried until a watched file changes again. hub is the run of the
// poll workers, stopped before each exec so no poll child is left behind.
func superviseRestart(ctx context.Context, exe, registry string, ln net.Listener, hub *procutil.Stoppable, out io.Writer) {
	files := restartWatches(exe, registry)
	for ctx.Err() == nil {
		what := ""
		watchFilesAt(ctx, files, dashboardPoll, func(name string) { what = name })
		if what == "" {
			return
		}
		err := handoff(
			what,
			func() error { return verifyBinary(ctx, exe) },
			func() func() { return hub.Quiesce(dashboardQuiesceBounds, procutil.KillChildren, dashboardLogf(out)) },
			func() error { return reexecSelf(exe, ln) },
			out,
		)
		if err != nil {
			fmt.Fprintf(out, "gt dashboard: not restarting, still serving the running build: %v\n", err)
		}
	}
}

// handoff checks the binary still runs, announces the restart, quiesces the hub
// and replaces this process with the new binary. what names the change that
// triggered it in the log line. quiesce stops the hub's polls and waits out the
// children they have in flight, so exec does not strand them as defunct; it
// returns the func that starts the hub again. exec returns only on failure, and
// the hub is resumed then, so a failed restart leaves this process serving with
// its polls running.
func handoff(what string, verify func() error, quiesce func() (resume func()), exec func() error, out io.Writer) error {
	if err := verify(); err != nil {
		return fmt.Errorf("the gt binary does not run: %w", err)
	}
	fmt.Fprintf(out, "gt dashboard: %s changed, restarting\n", what)
	resume := quiesce()
	if err := exec(); err != nil {
		resume()
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
