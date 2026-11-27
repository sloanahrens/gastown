//go:build integration

package rehearsal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestIntegrationRehearsalSideRunCancelEndsTheChild pins that a canceled run ends the child its command started, not only the shell ExecSideRun started: the child holds a FIFO's write end, so the read end reaches end-of-file exactly when that child exits.
func TestIntegrationRehearsalSideRunCancelEndsTheChild(t *testing.T) {
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "child.alive")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	bound := boundedCtx(t)
	ctx, cancel := context.WithCancel(bound)
	eof := make(chan struct{})
	go func() {
		f, err := os.Open(fifo) // blocks until the child opens the write end
		if err != nil {
			t.Errorf("open the child's FIFO: %v", err)
			cancel()
		} else {
			defer f.Close()
			cancel() // the child is up: end the run
			if _, err := io.Copy(io.Discard, f); err != nil {
				t.Errorf("wait for the child's end-of-file: %v", err)
			}
		}
		close(eof)
	}()
	// The shell backgrounds a child that holds the FIFO's write end before becoming a sleep.
	code, err := ExecSideRun(ctx, t.TempDir(), nil, []string{"sh", "-c", `sh -c 'exec 3>"$1"; exec sleep 60' sh "$1" & wait`, "sh", fifo})
	if code != -1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecSideRun = %d, %v; want -1 and context.Canceled", code, err)
	}
	select {
	case <-eof:
	case <-bound.Done():
		t.Fatalf("the child outlived the canceled run: %v", bound.Err())
	}
}

// TestIntegrationRehearsalSideRunExitStatus pins how ExecSideRun reports how its command ended: a non-zero status is the run's own code with a nil error, while a signal gives no code.
func TestIntegrationRehearsalSideRunExitStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		argv    []string
		want    int
		wantErr bool
	}{
		{name: "non-zero status is the run's code", argv: []string{"sh", "-c", "exit 3"}, want: 3},
		{name: "a signaled command has no code", argv: []string{"sh", "-c", "kill -TERM $$"}, want: -1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, err := ExecSideRun(boundedCtx(t), t.TempDir(), nil, tc.argv)
			if code != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("ExecSideRun(%q) = %d, %v; want %d, error %v", tc.argv, code, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestIntegrationRehearsalSideRunSucceeds pins that a command exiting 0 returns 0 and a nil error, with dir as its working directory and env as its whole environment.
func TestIntegrationRehearsalSideRunSucceeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	code, err := ExecSideRun(boundedCtx(t), dir, []string{"GREETING=hello"}, []string{"sh", "-c", `printf '%s' "$GREETING" > seen`})
	if code != 0 || err != nil {
		t.Fatalf("ExecSideRun = %d, %v; want 0, nil", code, err)
	}
	if seen, err := os.ReadFile(filepath.Join(dir, "seen")); err != nil {
		t.Fatalf("the command's relative write missed dir: %v", err)
	} else if string(seen) != "hello" {
		t.Errorf("the command wrote %q, want %q", seen, "hello")
	}
}
