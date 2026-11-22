package daemon

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// TestLimitedBufferKeepsTheFirstBytes: a buffer of 8 bytes written 5 then 10
// bytes holds the first 8 and says it truncated; every write reports its full
// length with no error, so a child never sees a short write.
func TestLimitedBufferKeepsTheFirstBytes(t *testing.T) {
	t.Parallel()
	b := &limitedBuffer{limit: 8}
	for _, w := range []string{"abcde", "fghijklmno"} {
		n, err := b.Write([]byte(w))
		if n != len(w) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil", w, n, err, len(w))
		}
	}
	if got := string(b.Bytes()); got != "abcdefgh" {
		t.Errorf("Bytes() = %q, want %q", got, "abcdefgh")
	}
	if !b.Truncated() {
		t.Error("Truncated() = false after writing past the limit")
	}
}

// TestLimitedBufferKeepsWritesUnderTheLimitWhole: writes that fit are kept
// whole and do not report truncation, including an empty write and a write that
// ends exactly at the limit.
func TestLimitedBufferKeepsWritesUnderTheLimitWhole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		limit  int
		writes []string
		want   string
	}{
		{"under the limit", 8, []string{"abc", "de"}, "abcde"},
		{"empty write", 8, []string{""}, ""},
		{"exactly the limit", 4, []string{"ab", "cd"}, "abcd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &limitedBuffer{limit: tt.limit}
			for _, w := range tt.writes {
				if n, err := b.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil", w, n, err, len(w))
				}
			}
			if got := string(b.Bytes()); got != tt.want {
				t.Errorf("Bytes() = %q, want %q", got, tt.want)
			}
			if b.Truncated() {
				t.Error("Truncated() = true for writes that fit")
			}
		})
	}
}

// fakeExitError is an error that carries a process exit code, as the one
// exec.Cmd.Run returns does, so the outcome table needs no process.
type fakeExitError struct{ code int }

func (e fakeExitError) Error() string { return "exit status" }
func (e fakeExitError) ExitCode() int { return e.code }

// TestDriftChildOutcome: the exit mapping of a bounded child. No run error is
// 0; a run error after the context ended is -1 with the context error; an
// error that carries an exit code is that code with no error (a non-zero exit
// is the child's answer); any other run error is -1 with that error.
func TestDriftChildOutcome(t *testing.T) {
	t.Parallel()
	other := errors.New("fork/exec: no such file")
	tests := []struct {
		name    string
		runErr  error
		ctxErr  error
		want    int
		wantErr error
	}{
		{"no run error", nil, nil, 0, nil},
		{"the context ended it", other, context.Canceled, -1, context.Canceled},
		{"a non-zero exit is the child's answer", fakeExitError{code: 3}, nil, 3, nil},
		{"an error with no exit code", other, nil, -1, other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := driftChildOutcome(tt.runErr, tt.ctxErr)
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Errorf("driftChildOutcome(%v, %v) = %d, %v; want %d, %v", tt.runErr, tt.ctxErr, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestStartDriftChildRefusesWhatCannotRun: a program that does not exist and a
// context that is already canceled each return exit -1 and an error, with no
// panic and nothing kept from a child that never ran.
func TestStartDriftChildRefusesWhatCannotRun(t *testing.T) {
	t.Parallel()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		exe  string
	}{
		{"missing program", context.Background(), filepath.Join(t.TempDir(), "no-such-program")},
		{"canceled context", canceled, filepath.Join(t.TempDir(), "never-run")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, exit, err := startDriftChild(tt.ctx, tt.exe, []string{"x"}, 16)
			if err == nil || exit != -1 {
				t.Errorf("startDriftChild = exit %d, err %v; want -1 and an error", exit, err)
			}
			if len(stdout) != 0 || len(stderr) != 0 {
				t.Errorf("startDriftChild kept output %q %q from a child that never ran", stdout, stderr)
			}
		})
	}
}

// TestDriftChildStandIn is the child TestStartDriftChildRealProcess runs. It
// does nothing in an ordinary run; when startDriftChild starts this test binary
// with a mode after "--", it acts on it: "exit N" exits with status N and
// "kill" kills itself with SIGKILL.
//
//testpolicy:allow parallel — a child-process helper, inert unless its argv names a mode
func TestDriftChildStandIn(t *testing.T) {
	switch flag.Arg(0) {
	case "exit":
		code, _ := strconv.Atoi(flag.Arg(1))
		syscall.Exit(code)
	case "kill":
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	}
}

// TestStartDriftChildRealProcess: what startDriftChild returns for a real
// child. A non-zero exit, including 255, is the child's answer with no error; a
// child killed by a signal is told apart from it by -1 and an error; a binary
// that does not exist is -1 and the start error.
func TestStartDriftChildRealProcess(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mode := func(args ...string) []string {
		return append([]string{"-test.run=^TestDriftChildStandIn$", "--"}, args...)
	}
	tests := []struct {
		name    string
		exe     string
		args    []string
		want    int
		wantErr bool
	}{
		{"exit 3", self, mode("exit", "3"), 3, false},
		{"exit 255", self, mode("exit", "255"), 255, false},
		{"killed by a signal", self, mode("kill"), -1, true},
		{"missing binary", filepath.Join(t.TempDir(), "no-such-binary"), nil, -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, exit, err := startDriftChild(context.Background(), tt.exe, tt.args, 64)
			if exit != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("startDriftChild = exit %d, err %v; want exit %d, error %v", exit, err, tt.want, tt.wantErr)
			}
		})
	}
}
