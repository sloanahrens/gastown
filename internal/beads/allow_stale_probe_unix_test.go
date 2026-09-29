//go:build !windows

package beads

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// allowStaleProbeBackstop bounds the waits in
// TestProbeAllowStale_CancelKillsProcessGroup that exist only so a broken
// probe fails the test instead of hanging it. Nothing in the test races it:
// each wait ends on an event (a FIFO opening, a line, the probe returning, the
// FIFO's last writer exiting), so it is set far above any loaded host.
const allowStaleProbeBackstop = 2 * time.Minute

// TestProbeAllowStale_CancelKillsProcessGroup checks that a probe whose
// context ends answers "unsupported, not definitive" and kills bd's whole
// process group: the stub bd leaves a grandchild (a subshell's `sleep`) that
// the default exec.CommandContext kill, which signals bd alone, would leak.
//
// The test used to race the real probe timer (100 ms) against a grandchild
// that wrote a marker file after 200 ms, and failed whenever a loaded host
// fired the timer late. Now the test ends the probe itself, once the
// grandchild has said it is running, and sees the grandchild's death as EOF
// on a FIFO the grandchild holds open for writing: a FIFO reads EOF only
// when every writer is gone.
func TestProbeAllowStale_CancelKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "grandchild.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	bd := filepath.Join(dir, "bd")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then
  ( exec 3>%q; echo ready >&3; exec sleep 1000 ) &
  wait
fi
exit 0
`, fifo)
	if err := os.WriteFile(bd, []byte(script), 0o755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type answer struct{ supported, definitive bool }
	answered := make(chan answer, 1)
	go func() {
		s, d := probeAllowStale(ctx, bd, nil)
		answered <- answer{s, d}
	}()

	// Opening the FIFO for reading blocks until the grandchild opens it for
	// writing.
	opened := make(chan *os.File, 1)
	openErr := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
		if err != nil {
			openErr <- err
			return
		}
		opened <- f
	}()
	var f *os.File
	select {
	case f = <-opened:
	case err := <-openErr:
		t.Fatalf("open fifo: %v", err)
	case a := <-answered:
		t.Fatalf("probe answered %+v before its grandchild started", a)
	case <-time.After(allowStaleProbeBackstop):
		t.Fatal("the stub's grandchild never opened the fifo")
	}
	defer f.Close()
	// The grandchild writes its line as soon as it has the FIFO open.
	r := bufio.NewReader(f)
	if line, err := r.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("grandchild's first line = %q, %v; want \"ready\\n\"", line, err)
	}

	cancel()
	select {
	case a := <-answered:
		if a.supported || a.definitive {
			t.Fatalf("cancelled probe answered supported=%v definitive=%v, want false, false", a.supported, a.definitive)
		}
	case <-time.After(allowStaleProbeBackstop):
		t.Fatal("probe did not return after its context was cancelled")
	}

	// The grandchild writes nothing more, so the read ends at EOF when it
	// exits. (A FIFO opened blocking takes no read deadline, hence the
	// goroutine.)
	type rest struct {
		b   []byte
		err error
	}
	drained := make(chan rest, 1)
	go func() {
		b, err := io.ReadAll(r)
		drained <- rest{b, err}
	}()
	select {
	case got := <-drained:
		if got.err != nil || len(got.b) != 0 {
			t.Fatalf("reading the fifo to EOF = %q, %v; want no more output and EOF", got.b, got.err)
		}
	case <-time.After(allowStaleProbeBackstop):
		t.Fatal("the grandchild still holds the fifo after the probe was cancelled: the kill did not reach bd's process group")
	}
}
