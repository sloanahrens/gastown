//go:build integration

package shelltier

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

func TestIntegrationOSExecReportsTheExitCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
		want int
	}{
		{name: "passing", argv: []string{"true"}, want: 0},
		{name: "failing", argv: []string{"false"}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			code, err := OSExec(t.Context(), Command{
				Dir:  dir,
				Argv: tc.argv,
				Log:  filepath.Join(dir, "logs", "run.log"),
			})
			if err != nil {
				t.Fatalf("OSExec error: %v", err)
			}
			if code != tc.want {
				t.Fatalf("exit code = %d, want %d", code, tc.want)
			}
		})
	}
}

func TestIntegrationRunnerEndToEndOnATwoScriptFixture(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scripts := map[string]string{
		"pass_test.sh": "exit 0\n",
		"fail_test.sh": "echo boom\nexit 1\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logDir := t.TempDir()

	r := Run(t.Context(), Config{
		Entries:     entriesAt("pass_test.sh", "fail_test.sh"),
		Root:        root,
		LogDir:      logDir,
		Budget:      time.Minute,
		Exec:        OSExec,
		Now:         time.Now,
		WithTimeout: context.WithTimeout,
	})

	want := "tier-sweep: shell RED passed=1 failed=1 skipped=0 failed: fail_test.sh (logs " + logDir + ")"
	if line := Summary(r, logDir); !strings.HasPrefix(line, want) {
		t.Fatalf("summary = %q, want prefix %q", line, want)
	}
	if got := r.Exit(); got != ExitRed {
		t.Fatalf("exit = %d, want ExitRed", got)
	}
	if _, err := os.Stat(filepath.Join(logDir, "shell-fail_test.sh.log")); err != nil {
		t.Fatalf("failing script's log not written: %v", err)
	}
}

func TestIntegrationOSExecPassesTheEnvAndMakesTheTMPDIR(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	code, err := OSExec(t.Context(), Command{
		Dir:  dir,
		Argv: []string{"sh", "-c", `test -d "$TMPDIR" && test "$SHELLTIER_PROBE" = yes`},
		Env:  []string{"SHELLTIER_PROBE=yes", "TMPDIR=" + filepath.Join(dir, "tmp", "a")},
		Log:  filepath.Join(dir, "logs", "run.log"),
	})
	if err != nil || code != 0 {
		t.Fatalf("exit code = %d, error = %v, want 0 and nil: the env or TMPDIR did not reach the script", code, err)
	}
}

func TestIntegrationOSExecReportsABudgetKillAsAnErrorNotACode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	code, err := OSExec(ctx, Command{
		Dir:  dir,
		Argv: []string{"sleep", "30"},
		Log:  filepath.Join(dir, "logs", "run.log"),
	})
	if err == nil {
		t.Fatalf("error = nil, want the budget kill reported (exit code %d)", code)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for a command the budget killed", code)
	}
}

// TestIntegrationBudgetKillTakesTheScriptsChildren runs a script whose child
// holds a FIFO's write end open. The test's open of the read end returns only
// once that child has the FIFO, so it is the ready signal: no sleep, no poll.
// After the budget ends the command, EOF on the read means every process that
// held the write end is gone. A child the kill missed keeps it open, and the
// read times out.
func TestIntegrationBudgetKillTakesTheScriptsChildren(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "child.fifo")
	pidFile := filepath.Join(dir, "group.pid")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// The script leads its own process group, so the pid it records is the
	// group id. Cleanup kills that group if the run failed before the budget did.
	t.Cleanup(func() { killRecordedGroup(t, pidFile) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := OSExec(ctx, Command{
			Dir:  dir,
			Argv: []string{"sh", "-c", `echo $$ > "$PIDFILE"; sleep 60 3>"$FIFO" & wait`},
			Env:  []string{"PIDFILE=" + pidFile, "FIFO=" + fifo},
			Log:  filepath.Join(dir, "logs", "run.log"),
		})
		done <- err
	}()

	type opened struct {
		f   *os.File
		err error
	}
	ready := make(chan opened, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
		ready <- opened{f, err}
	}()
	var reader *os.File
	select {
	case o := <-ready:
		if o.err != nil {
			t.Fatalf("open the FIFO's read end: %v", o.err)
		}
		reader = o.f
	case err := <-done:
		t.Fatalf("the script ended before its child opened the FIFO: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the child never opened the FIFO")
	}
	defer reader.Close()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want one wrapping context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("OSExec did not return after the budget ended the command")
	}

	// The read runs in its own goroutine so a surviving child cannot hang the test.
	eof := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 1))
		eof <- err
	}()
	select {
	case err := <-eof:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read after the budget kill = %v, want EOF: a child still holds the FIFO open", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a child still holds the FIFO open after the budget killed its script")
	}
}

// killRecordedGroup kills the process group whose id a script recorded in
// pidFile. A missing file means the script never ran, and an empty group means
// the kill already happened: both are no-ops.
func killRecordedGroup(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Logf("pid file %s does not hold a pid: %v", pidFile, err)
		return
	}
	if err := util.KillProcessGroupID(pgid); err != nil {
		t.Logf("kill process group %d: %v", pgid, err)
	}
}
