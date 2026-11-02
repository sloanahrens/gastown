package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// writeRun writes a sample run directory under t.TempDir and returns it.
func writeRun(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// cleanFiles is a run that reads NO-KNOWN-FAILURE.
func cleanFiles() map[string]string {
	return map[string]string{
		"create.status": "0\n",
		"list.txt":      "NAME              STATUS\nisolation-probe   Ready\n",
		"placement.txt": "isolation-probe   node-a\n",
		"log.txt":       "sandbox admission started\n",
	}
}

// runCommand runs the command and returns stdout, stderr and the exit code.
func runCommand(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestCleanRunPrintsNoKnownFailureAndExitsZero(t *testing.T) {
	t.Parallel()
	dir := writeRun(t, cleanFiles())
	stdout, stderr, code := runCommand(t, dir, "node-a")

	want := "probe=landlock result=no-known-failure\n" +
		"probe=seccomp-user-notify result=no-known-failure\n" +
		"probe=task-memory result=no-known-failure\n" +
		"VERDICT NO-KNOWN-FAILURE\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestFailPrintsTheFailingProbesAndExitsNonZero(t *testing.T) {
	t.Parallel()
	files := cleanFiles()
	delete(files, "log.txt")
	stdout, stderr, code := runCommand(t, writeRun(t, files), "node-a")

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout, "VERDICT FAIL probe=landlock,seccomp-user-notify,task-memory\n") {
		t.Errorf("stdout = %q, want the VERDICT FAIL line naming every probe", stdout)
	}
	if !strings.Contains(stderr, "log.txt could not be read") {
		t.Errorf("stderr = %q, want the unusable input named", stderr)
	}
}

func TestInconclusiveExitsNonZero(t *testing.T) {
	t.Parallel()
	files := cleanFiles()
	files["log.txt"] = "task-memory probe fell back to /proc/42/mem\n"
	stdout, stderr, code := runCommand(t, writeRun(t, files), "node-a")

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout, "VERDICT INCONCLUSIVE probe=task-memory\n") {
		t.Errorf("stdout = %q, want the VERDICT INCONCLUSIVE line", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
}

func TestRefusedCommandLineExitsTwo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"one argument", []string{"run-dir"}},
		{"three arguments", []string{"run-dir", "node-a", "extra"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := runCommand(t, tc.args...)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
			if !strings.Contains(stderr, "usage: isolation-verdict DIR NODE") {
				t.Errorf("stderr = %q, want the usage line", stderr)
			}
		})
	}
}
