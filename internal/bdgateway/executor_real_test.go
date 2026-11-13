package bdgateway

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
)

// TestExecutorRealProcess is the one test in this package that starts a
// process. The bd it runs is this test binary re-run as TestBDStandIn, which
// is how the real runner's exit status, its two streams and the timeout kill
// are shown on a process without another program being built. Every other test
// in the package drives fakeRunner.
func TestExecutorRealProcess(t *testing.T) {
	t.Parallel()
	self := testBinary(t)
	e, err := New(Options{
		Binary:        self,
		WorkspaceRoot: testWorkspace(t),
		Home:          t.TempDir(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Run("exit status and streams", func(t *testing.T) {
		var out bytes.Buffer
		if err := e.Exec(context.Background(), Request{Argv: bdArgv("3"), Rig: "gastown"}, nil, &out); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		frames := readFrames(t, out.Bytes())
		if len(frames) == 0 {
			t.Fatal("no frames")
		}
		if got, err := frames[len(frames)-1].ExitStatus(); err != nil || got != 3 {
			t.Errorf("exit status = %d (%v), want 3", got, err)
		}
		if got := streamOf(frames, FrameStdout); got != "out\n" {
			t.Errorf("stdout frames = %q, want %q", got, "out\n")
		}
		if got := streamOf(frames, FrameStderr); got != "err\n" {
			t.Errorf("stderr frames = %q, want %q", got, "err\n")
		}
	})

	t.Run("timeout kills the process", func(t *testing.T) {
		var out bytes.Buffer
		err := e.Exec(context.Background(), Request{Argv: bdArgv("hold"), Rig: "gastown", TimeoutMS: 100}, nil, &out)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("Exec = %v, want ErrTimeout", err)
		}
		frames := readFrames(t, out.Bytes())
		if len(frames) != 2 {
			t.Fatalf("frames = %d, want the error frame and the exit frame", len(frames))
		}
		refusal, err := frames[0].GatewayError()
		if err != nil || refusal.Code != CodeTimeout {
			t.Errorf("error frame = %+v (%v), want code %q", refusal, err, CodeTimeout)
		}
		if status, err := frames[1].ExitStatus(); err != nil || status != 124 {
			t.Errorf("exit status = %d (%v), want 124", status, err)
		}
	})

	t.Run("a pipe pair that fails", func(t *testing.T) {
		boom := errors.New("out of file descriptors")
		calls := 0
		r := realRunner{pipe: func() (*os.File, *os.File, error) {
			calls++
			if calls == 2 {
				return nil, nil, boom
			}
			return os.Pipe()
		}}
		// The second pair failing makes the start close the first pair.
		if _, err := r.Start(self, nil, []string{"PATH=/usr/bin:/bin"}, t.TempDir()); !errors.Is(err, boom) {
			t.Fatalf("Start = %v, want %v", err, boom)
		}
	})

	t.Run("a program that is not there", func(t *testing.T) {
		absent := filepath.Join(t.TempDir(), "no-such-bd")
		if _, err := (realRunner{}).Start(absent, nil, nil, t.TempDir()); err == nil {
			t.Fatal("Start of a missing program = nil error, want the start's failure")
		}
	})

	t.Run("a wait that fails and a kill after it", func(t *testing.T) {
		p, err := (realRunner{}).Start(self, bdArgv("0"), nil, t.TempDir())
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if status, err := p.Wait(); err != nil || status != 0 {
			t.Fatalf("Wait = %d (%v), want 0", status, err)
		}
		// A second Wait is the non-ExitError failure a real command reports:
		// the status is unknown, so the executor must not read it as bd's.
		if status, err := p.Wait(); err == nil || status != -1 {
			t.Errorf("second Wait = %d (%v), want -1 and an error", status, err)
		}
		if err := p.Kill(); !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("Kill after Wait = %v, want os.ErrProcessDone", err)
		}
	})
}

// TestBDStandIn is the bd stand-in TestExecutorRealProcess runs. It does
// nothing in an ordinary run, and when the executor starts this binary with
// bdArgv it acts as bd. The exit is syscall.Exit rather than os.Exit: a test
// binary built with coverage writes its GOCOVERDIR warning to stderr from an
// exit hook, and stderr here is bd's stream and an assertion.
//
//testpolicy:allow parallel — a child-process helper, inert unless its argv names a mode
func TestBDStandIn(t *testing.T) {
	if flag.NArg() == 0 {
		return
	}
	if flag.Arg(0) == "hold" {
		holdUntilKilled()
	}
	status, err := strconv.Atoi(flag.Arg(0))
	if err != nil {
		return // not a mode this helper acts on
	}
	fmt.Fprintln(os.Stdout, "out")
	fmt.Fprintln(os.Stderr, "err")
	syscall.Exit(status)
}

// holdUntilKilled blocks until the executor's kill reaches the process. A pipe
// whose write end stays open blocks the read without a timer over it, so the
// hold has no built-in deadline and the runtime has no deadlock to report.
func holdUntilKilled() {
	r, w, err := os.Pipe()
	if err != nil {
		syscall.Exit(2)
	}
	buf := make([]byte, 1)
	_, _ = r.Read(buf)
	// Without this, the finalizer on the unused write end closes it, the read
	// ends, and the process exits before the kill this mode exists to wait for.
	runtime.KeepAlive(w)
	syscall.Exit(0)
}

// bdArgv is the argv that runs the stand-in in mode: the mode follows the test
// flag, so the testing package parses the flag and leaves the mode as a
// positional argument.
func bdArgv(mode string) []string {
	return []string{"-test.run=^TestBDStandIn$", mode}
}

// testBinary is this test's own binary, which the real runner starts as bd.
func testBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("the test binary's path: %v", err)
	}
	return self
}
