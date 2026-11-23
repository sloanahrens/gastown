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
