//go:build integration

package shelltier

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestIntegrationBudgetKillTakesTheScriptsChildren(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := OSExec(ctx, Command{
		Dir:  dir,
		Argv: []string{"sh", "-c", `sleep 30 & echo $! > "$PIDFILE"; wait`},
		Env:  []string{"PIDFILE=" + pidFile},
		Log:  filepath.Join(dir, "logs", "run.log"),
	})
	if err == nil {
		t.Fatal("error = nil, want the budget kill reported")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the script did not record its child's pid: %v", err)
	}
	pid := strings.TrimSpace(string(raw))
	deadline := time.Now().Add(10 * time.Second)
	for {
		state := processState(t, pid)
		if state == "" || strings.HasPrefix(state, "Z") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %s still running (state %q) after the budget killed its script", pid, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// processState is the ps state of pid, or empty when no such process exists.
func processState(t *testing.T, pid string) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "" // ps exits 1 when no process has the pid
	}
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	return strings.TrimSpace(string(out))
}
