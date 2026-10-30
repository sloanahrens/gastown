package shelltier

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

// errDenied is the failure a fake file system or starter returns.
var errDenied = errors.New("denied")

// fakeFiles is the file system a case runs against: it fails the directories
// and the create it is told to, and records every call in order.
type fakeFiles struct {
	failDir   map[string]error
	createErr error
	closeErr  error
	calls     []string
}

func (f *fakeFiles) MkdirAll(dir string) error {
	f.calls = append(f.calls, "mkdir "+dir)
	return f.failDir[dir]
}

func (f *fakeFiles) Create(name string) (io.WriteCloser, error) {
	f.calls = append(f.calls, "create "+name)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return logFile{Writer: io.Discard, closeErr: f.closeErr}, nil
}

// logFile is a log that discards its output and closes with closeErr.
type logFile struct {
	io.Writer
	closeErr error
}

func (l logFile) Close() error { return l.closeErr }

// testCommand writes its log and TMPDIR under /logs, so the fake's directory
// names are fixed.
func testCommand() Command {
	return Command{
		Dir:  "/repo",
		Argv: []string{"bash", "scripts/a_test.sh"},
		Env:  []string{"GT_TEST_DOCKER=0", "TMPDIR=/logs/tmp/a"},
		Log:  "/logs/shell-a.log",
	}
}

// fakeStart returns a starter that records the start in f's call list and
// returns err.
func fakeStart(f *fakeFiles, err error) starter {
	return func(_ context.Context, _ Command, _ io.Writer) error {
		f.calls = append(f.calls, "start")
		return err
	}
}

func TestExecWithMakesTheLogDirAndTMPDIRBeforeItStarts(t *testing.T) {
	t.Parallel()
	f := &fakeFiles{}
	execWith(t.Context(), testCommand(), f, fakeStart(f, nil))
	want := []string{"mkdir /logs", "mkdir /logs/tmp/a", "create /logs/shell-a.log", "start"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
}

func TestExecWithRefusesToStartWhenItCannotMakeItsDirectories(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		f    fakeFiles
	}{
		{name: "log dir", f: fakeFiles{failDir: map[string]error{"/logs": errDenied}}},
		{name: "TMPDIR", f: fakeFiles{failDir: map[string]error{"/logs/tmp/a": errDenied}}},
		{name: "log file", f: fakeFiles{createErr: errDenied}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.f
			code, err := execWith(t.Context(), testCommand(), &f, fakeStart(&f, nil))
			if !errors.Is(err, errDenied) {
				t.Fatalf("error = %v, want one wrapping the denial", err)
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 when it did not start", code)
			}
			if slices.Contains(f.calls, "start") {
				t.Fatalf("started the command after the failure: calls %q", f.calls)
			}
		})
	}
}

func TestExecWithReturnsTheStartError(t *testing.T) {
	t.Parallel()
	f := &fakeFiles{}
	cannotStart := errors.New("cannot start")
	code, err := execWith(t.Context(), testCommand(), f, fakeStart(f, cannotStart))
	if !errors.Is(err, cannotStart) {
		t.Fatalf("error = %v, want the start error", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 when it did not start", code)
	}
}

func TestExecWithFailsAPassingCommandWhoseLogWillNotClose(t *testing.T) {
	t.Parallel()
	f := &fakeFiles{closeErr: errDenied}
	_, err := execWith(t.Context(), testCommand(), f, fakeStart(f, nil))
	if !errors.Is(err, errDenied) {
		t.Fatalf("error = %v, want the log's close error", err)
	}
}

func TestExecWithReportsASuccessfulStartAsCodeZero(t *testing.T) {
	t.Parallel()
	f := &fakeFiles{}
	code, err := execWith(t.Context(), testCommand(), f, fakeStart(f, nil))
	if err != nil || code != 0 {
		t.Fatalf("exit code = %d, error = %v, want 0 and nil", code, err)
	}
}

func TestExecWithRefusesACommandWithNoArgvBeforeItTouchesTheFileSystem(t *testing.T) {
	t.Parallel()
	f := &fakeFiles{}
	c := testCommand()
	c.Argv = nil
	code, err := execWith(t.Context(), c, f, fakeStart(f, nil))
	if !errors.Is(err, errNoArgv) {
		t.Fatalf("error = %v, want errNoArgv", err)
	}
	if code != 0 || len(f.calls) != 0 {
		t.Fatalf("exit code = %d, calls = %q, want 0 and no calls", code, f.calls)
	}
}

func TestExecWithReportsACommandWhoseContextEndedAsAnErrorNotACode(t *testing.T) {
	t.Parallel()
	f := &fakeFiles{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The starter stands in for a command the budget killed: the context ends
	// and the process reports no failure of its own.
	start := func(context.Context, Command, io.Writer) error {
		cancel()
		return nil
	}
	code, err := execWith(ctx, testCommand(), f, start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want one wrapping context.Canceled", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 when the context ended", code)
	}
}

func TestExecWithKeepsTheStartErrorWhenTheLogAlsoFailsToClose(t *testing.T) {
	t.Parallel()
	f := &fakeFiles{closeErr: errDenied}
	cannotStart := errors.New("cannot start")
	_, err := execWith(t.Context(), testCommand(), f, fakeStart(f, cannotStart))
	if !errors.Is(err, cannotStart) {
		t.Fatalf("error = %v, want the start error", err)
	}
}
