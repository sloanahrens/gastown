package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// 0; a run error after the context ended is -1 with the context error; a
// successful exit whose pipes a grandchild held past WaitDelay (exec's
// ErrWaitDelay) is 0 with no error; an error that carries an exit code is that
// code with no error (a non-zero exit is the child's answer); any other run
// error is -1 with that error.
func TestDriftChildOutcome(t *testing.T) {
	t.Parallel()
	other := errors.New("fork/exec: no such file")
	tests := []struct {
		name       string
		runErr     error
		ctxErr     error
		exitedZero bool
		want       int
		wantErr    error
	}{
		{"no run error", nil, nil, true, 0, nil},
		{"the context ended it", other, context.Canceled, false, -1, context.Canceled},
		{"a non-zero exit is the child's answer", fakeExitError{code: 3}, nil, false, 3, nil},
		{"an error with no exit code", other, nil, false, -1, other},
		{"a clean exit whose pipes a grandchild held past WaitDelay", exec.ErrWaitDelay, nil, true, 0, nil},
		{"WaitDelay with a non-zero exit stays an error", exec.ErrWaitDelay, nil, false, -1, exec.ErrWaitDelay},
		{"the context wins over WaitDelay", exec.ErrWaitDelay, context.DeadlineExceeded, true, -1, context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := driftChildOutcome(tt.runErr, tt.ctxErr, tt.exitedZero)
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Errorf("driftChildOutcome(%v, %v, %v) = %d, %v; want %d, %v", tt.runErr, tt.ctxErr, tt.exitedZero, got, err, tt.want, tt.wantErr)
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

// TestDriftChildCancel: the cancel the command installs ends the whole process
// group. A group signal that succeeds is one call with a negative pid and
// SIGKILL, and the child alone is never killed; a group signal that fails falls
// back to killing the child alone once and returns that result, so a sandbox
// that refuses the group still ends the child.
func TestDriftChildCancel(t *testing.T) {
	t.Parallel()
	const pid = 4242
	groupErr := errors.New("no such process group")
	killErr := errors.New("kill refused")
	tests := []struct {
		name     string
		groupErr error
		killErr  error
		wantKill int
		wantErr  error
	}{
		{"the group signal succeeds", nil, killErr, 0, nil},
		{"the group signal fails, so the child is killed", groupErr, killErr, 1, killErr},
		{"the group signal fails and the kill succeeds", groupErr, nil, 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var (
				signalPids []int
				signalSigs []syscall.Signal
				killCalls  int
			)
			signal := func(p int, sig syscall.Signal) error {
				signalPids = append(signalPids, p)
				signalSigs = append(signalSigs, sig)
				return tt.groupErr
			}
			kill := func() error {
				killCalls++
				return tt.killErr
			}
			err := driftChildCancel(pid, signal, kill)
			if len(signalPids) != 1 || signalPids[0] != -pid || signalSigs[0] != syscall.SIGKILL {
				t.Errorf("signal calls = %v %v, want one call with (%d, SIGKILL)", signalPids, signalSigs, -pid)
			}
			if killCalls != tt.wantKill {
				t.Errorf("kill called %d times, want %d", killCalls, tt.wantKill)
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("driftChildCancel = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("driftChildCancel = %v, want the kill result %v", err, tt.wantErr)
			}
			if errors.Is(err, groupErr) {
				t.Errorf("driftChildCancel = %v, want the kill result rather than the group error", err)
			}
		})
	}
}

// TestDriftChildEnv: the drift child's environment keeps the variables it needs
// to run and drops every variable whose name the town's secret-name rule names
// as a credential, without a second name list (gt-66luy0). An entry with no "="
// is not an assignment and is dropped; an empty environment gives an empty
// slice.
func TestDriftChildEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		environ []string
		want    []string
	}{
		{"DEEPSEEK_API_KEY is dropped", []string{"DEEPSEEK_API_KEY=x"}, nil},
		{"GH_TOKEN is dropped", []string{"GH_TOKEN=x"}, nil},
		{"DB_PASSWORD is dropped", []string{"DB_PASSWORD=x"}, nil},
		{"SOME_SECRET is dropped", []string{"SOME_SECRET=x"}, nil},
		{"PATH, HOME and the town root are kept", []string{"PATH=/usr/bin:/bin", "HOME=/home/agent", "GT_TOWN_ROOT=/town"}, []string{"PATH=/usr/bin:/bin", "HOME=/home/agent", "GT_TOWN_ROOT=/town"}},
		{"a mixed environment keeps only the non-credential names", []string{"PATH=/usr/bin", "GH_TOKEN=x", "GT_TOWN_ROOT=/town", "SOME_SECRET=y"}, []string{"PATH=/usr/bin", "GT_TOWN_ROOT=/town"}},
		{"an entry with no equals sign is dropped", []string{"NOEQUALSENTRY", "PATH=/usr/bin"}, []string{"PATH=/usr/bin"}},
		{"an empty environment is empty", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := driftChildEnv(tt.environ)
			if !slices.Equal(got, tt.want) {
				t.Errorf("driftChildEnv(%v) = %v, want %v", tt.environ, got, tt.want)
			}
		})
	}
}

// TestDriftChildCmd: the command startDriftChild runs leads its own process
// group, installs a cancel for it, and bounds the wait for output pipes a
// grandchild may hold past the child's exit by the named constant. The cancel
// on a command that never started has no process to end, so it reports the
// process already done rather than dereferencing the nil Process exec's
// default cancel would.
func TestDriftChildCmd(t *testing.T) {
	t.Parallel()
	cmd := driftChildCmd(&exec.Cmd{})
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Errorf("driftChildCmd SysProcAttr = %#v, want Setpgid true", cmd.SysProcAttr)
	}
	if cmd.WaitDelay != driftChildWaitDelay {
		t.Errorf("driftChildCmd WaitDelay = %v, want the driftChildWaitDelay constant %v", cmd.WaitDelay, driftChildWaitDelay)
	}
	if cmd.Cancel == nil {
		t.Fatal("driftChildCmd Cancel = nil, want the group-kill cancel")
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("driftChildCmd Cancel on an unstarted command = %v, want os.ErrProcessDone", err)
	}
}
