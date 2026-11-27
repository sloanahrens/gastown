//go:build integration

package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests in this file start real children, so they are in the integration
// tier: the unit tier uses fakes (cluster_drift_child_test.go). The children are
// /bin/sh one-liners, not this test binary re-executed: a re-executed test
// binary would run the package TestMain first, which starts a Dolt container
// before the stand-in body runs (om finding on gt-e8ol9r).

// TestIntegrationStartDriftChildRealProcess: what startDriftChild returns for a real
// child. A non-zero exit, including 255, is the child's answer with no error; a
// child killed by a signal is told apart from it by -1 and an error; a binary
// that does not exist is -1 and the start error.
func TestIntegrationStartDriftChildRealProcess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		exe     string
		args    []string
		want    int
		wantErr bool
	}{
		{"exit 3", "/bin/sh", []string{"-c", "exit 3"}, 3, false},
		{"exit 255", "/bin/sh", []string{"-c", "exit 255"}, 255, false},
		{"killed by a signal", "/bin/sh", []string{"-c", "kill -9 $$"}, -1, true},
		{"missing binary", filepath.Join(t.TempDir(), "no-such-binary"), nil, -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A generous bound so a child that never exits fails the run instead of
			// hanging it; the deadline is the context's, never read as the child's exit.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _, exit, err := startDriftChild(ctx, tt.exe, tt.args, 64)
			if exit != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("startDriftChild = exit %d, err %v; want exit %d, error %v", exit, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestIntegrationDriftChildEnvIsFiltered: the child startDriftChild runs
// inherits PATH but not a credential-named variable this process holds. The
// filter runs inside startDriftChild, so passing os.Environ() whole makes this
// row fail (gt-66luy0).
func TestIntegrationDriftChildEnvIsFiltered(t *testing.T) {
	// Not parallel: t.Setenv changes the process environment the child reads.
	const token = "drift-child-test-token-0123456789"
	t.Setenv("DEEPSEEK_API_KEY", token)
	// A generous bound so a child that never exits fails the run instead of
	// hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, _, exit, err := startDriftChild(ctx, "/usr/bin/env", nil, 1<<20)
	if err != nil || exit != 0 {
		t.Fatalf("startDriftChild /usr/bin/env = exit %d, err %v; want exit 0 and no error", exit, err)
	}
	var sawPath bool
	for _, entry := range strings.Split(string(stdout), "\n") {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case "PATH":
			sawPath = true
		case "DEEPSEEK_API_KEY":
			t.Errorf("the child environment carries DEEPSEEK_API_KEY, which the filter must drop")
		}
	}
	if !sawPath {
		t.Errorf("the child environment has no PATH, so the child could not find what it runs")
	}
}

// TestIntegrationStartDriftChildDeadlineEndsAChildThatIgnoresSIGTERM: a child
// that ignores SIGTERM and blocks is ended at a real deadline with exit -1 and
// the context error, so the timeout mapping is pinned by a process rather than
// the outcome table's fake (gt-mk9uh3).
func TestIntegrationStartDriftChildDeadlineEndsAChildThatIgnoresSIGTERM(t *testing.T) {
	t.Parallel()
	// The child waits in read(2) on a FIFO with no data and, because the test
	// holds the write end open, no end-of-file: it blocks until killed. The
	// read end opens non-blocking first so the write end's open finds a reader
	// instead of waiting for one.
	fifo := filepath.Join(t.TempDir(), "blocked")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("make the FIFO the child blocks on: %v", err)
	}
	rd, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open the FIFO for reading: %v", err)
	}
	defer rd.Close()
	wr, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open the FIFO for writing: %v", err)
	}
	defer wr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	stdout, stderr, exit, err := startDriftChild(ctx, "/bin/sh",
		[]string{"-c", `trap '' TERM; read x < "$1"`, "drift-child", fifo}, 64)
	if exit != -1 {
		t.Errorf("startDriftChild = exit %d, want -1 for a child the deadline ended", exit)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("startDriftChild error = %v, want the context deadline error", err)
	}
	if len(stdout) != 0 || len(stderr) != 0 {
		t.Errorf("startDriftChild kept %q %q from a child that wrote nothing", stdout, stderr)
	}
}

// TestIntegrationStartDriftChildKeepsTheFirstBytesOfAFlood: a child writing
// twenty megabytes to a small limit is neither blocked nor failed, the bytes
// kept are the first limit written, and its stderr is still reported
// (gt-mk9uh3).
func TestIntegrationStartDriftChildKeepsTheFirstBytesOfAFlood(t *testing.T) {
	t.Parallel()
	const (
		limit = 4096
		note  = "still here"
		// The head is distinct and the 20 MiB behind it repeats one chunk, so
		// "the first limit bytes" is tellable from the flood's last bytes.
		head = "first-bytes-kept:"
		reps = 20480 // 20480 * 1024 bytes is 20 MiB
	)
	fill := strings.Repeat("0123456789abcdef", 64) // 1024 bytes
	script := `printf '%s' "$1"; i=0; while [ "$i" -lt "$3" ]; do printf '%s' "$2"; i=$((i+1)); done; printf '%s' "$4" >&2`
	// A generous bound so a child that blocked fails instead of hanging the
	// run; the deadline is the context's, never read as the child's exit.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, stderr, exit, err := startDriftChild(ctx, "/bin/sh",
		[]string{"-c", script, "drift-child", head, fill, strconv.Itoa(reps), note}, limit)
	if err != nil || exit != 0 {
		t.Fatalf("startDriftChild = exit %d, err %v; want exit 0 and no error for a child writing %d bytes", exit, err, len(fill)*reps)
	}
	want := []byte(head + strings.Repeat(fill, (limit-len(head))/len(fill)+1))[:limit]
	if !bytes.Equal(stdout, want) {
		t.Errorf("startDriftChild kept %d stdout bytes, want exactly the first %d bytes of the flood", len(stdout), len(want))
	}
	if string(stderr) != note {
		t.Errorf("stderr = %q, want %q", stderr, note)
	}
}

// TestIntegrationDriftChildGrandchildDeadline: a child whose grandchild holds
// the output pipes is ended at a real deadline with exit -1 and the context
// error, and the call returns promptly. Only the group kill (driftChildCmd)
// reaches the grandchild; a deadline that killed the child alone would leave
// Run blocked on the pipes until the grandchild exits, 30 seconds here (gt-tduwd3).
func TestIntegrationDriftChildGrandchildDeadline(t *testing.T) {
	t.Parallel()
	// The background sleep is the grandchild: it inherits the child's stdout and
	// stderr. The foreground sleep is the child, blocking. Neither ends before
	// the deadline, so only the group kill can return the call promptly.
	const promptly = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, exit, err := startDriftChild(ctx, "/bin/sh",
		[]string{"-c", "sleep 30 & sleep 30"}, 64)
	elapsed := time.Since(start)
	if exit != -1 {
		t.Errorf("startDriftChild = exit %d, want -1 for a child the deadline ended", exit)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("startDriftChild error = %v, want the context deadline error", err)
	}
	if elapsed > promptly {
		t.Errorf("startDriftChild returned after %v, want it back within %v of the 300ms deadline: a grandchild held the pipes", elapsed, promptly)
	}
}

// TestIntegrationDriftChildExitZeroWithLingeringGrandchild: a child that exits
// 0 while its grandchild keeps the output pipes open returns at WaitDelay with
// exit 0, the bytes it wrote, and no error, rather than blocking until the
// grandchild ends (gt-i9dyt0). The group is never killed here, so the test
// kills the grandchild in its own cleanup.
func TestIntegrationDriftChildExitZeroWithLingeringGrandchild(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "grandchild-pid")
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	const (
		wrote = "child done\n"
		// The grandchild outlives the deadline below, so without WaitDelay the
		// call blocks until the test's own context ends it.
		grandchildSleepSeconds = 60
	)
	// The deadline is only a hang bound: the call must return at WaitDelay,
	// well before it, because the child itself exits at once.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	stdout, stderr, exit, err := startDriftChild(ctx, "/bin/sh",
		[]string{"-c", `printf 'child done\n'; sleep "$2" & echo $! > "$1"`, "drift-child", pidFile, strconv.Itoa(grandchildSleepSeconds)}, 256)
	elapsed := time.Since(start)
	if exit != 0 || err != nil {
		t.Fatalf("startDriftChild = exit %d, err %v; want exit 0 and no error for a clean exit", exit, err)
	}
	if string(stdout) != wrote {
		t.Errorf("stdout = %q, want %q kept from the child", stdout, wrote)
	}
	if len(stderr) != 0 {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	if margin := driftChildWaitDelay + 5*time.Second; elapsed > margin {
		t.Errorf("startDriftChild returned after %v, want it within %v (WaitDelay plus a margin): a grandchild held the pipes", elapsed, margin)
	}
}

// TestIntegrationDriftChildGroupKillEndsTheGrandchild: after the deadline ends
// a child whose grandchild holds the output pipes, the grandchild's pid is
// gone, so the group kill reached it. A cancel that signalled only the child
// would leave the grandchild alive in its sleep. The child reports the
// grandchild's pid on a FIFO the test reads before the deadline (gt-tduwd3,
// gt-orcopo).
func TestIntegrationDriftChildGroupKillEndsTheGrandchild(t *testing.T) {
	t.Parallel()
	// The FIFO is the pipe the child reports the grandchild's pid on. The test
	// holds it open for reading and writing: the open does not wait for the
	// child, the read waits for the report rather than seeing end-of-file, and
	// the pipe outlives the killed writers. The read is a blocking one, since
	// darwin cannot poll a FIFO and a non-blocking read there returns EAGAIN.
	fifo := filepath.Join(t.TempDir(), "grandchild-pid")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("make the FIFO the child reports the grandchild's pid on: %v", err)
	}
	rd, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open the FIFO: %v", err)
	}
	defer rd.Close()

	type report struct {
		pid int
		err error
	}
	reports := make(chan report, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := rd.Read(buf)
		if err != nil {
			reports <- report{err: err}
			return
		}
		raw := strings.TrimSpace(string(buf[:n]))
		pid, err := strconv.Atoi(raw)
		if err != nil || pid <= 1 {
			// Never probe pid 0 or 1: signal 0 to 0 names this process's own
			// group, which always exists.
			reports <- report{pid: 0, err: errors.New("the child reported " + strconv.Quote(raw) + ", not a pid")}
			return
		}
		reports <- report{pid: pid}
	}()

	// The deadline outlasts the child's start, when it writes the pid. The
	// background sleep is the grandchild: it inherits the child's stdout and
	// stderr, so it holds the output pipes after the child is gone.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type outcome struct {
		exit int
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		_, _, exit, err := startDriftChild(ctx, "/bin/sh",
			[]string{"-c", `sleep 30 & echo $! > "$1"; sleep 30`, "drift-child", fifo}, 64)
		done <- outcome{exit: exit, err: err}
	}()

	var pid int
	select {
	case got := <-reports:
		if got.err != nil {
			t.Fatalf("read the grandchild's pid from the FIFO: %v", got.err)
		}
		pid = got.pid
	case <-time.After(10 * time.Second):
		t.Fatal("the child never reported its grandchild's pid on the FIFO")
	}

	// After the deadline the grandchild must be no such process. The poll is
	// the package's bounded helper: a child-only kill leaves the grandchild
	// sleeping out its 30 seconds, and the probe never sees ESRCH.
	<-ctx.Done()
	gone := waitFor(5*time.Second, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
	if !gone {
		t.Fatalf("grandchild %d still exists after the deadline: the group kill did not reach it", pid)
	}

	select {
	case got := <-done:
		if got.exit != -1 {
			t.Errorf("startDriftChild = exit %d, want -1 for a child the deadline ended", got.exit)
		}
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Errorf("startDriftChild error = %v, want the context deadline error", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startDriftChild did not return after the deadline: a grandchild held the pipes")
	}
}
