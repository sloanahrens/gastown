package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/rehearsal"
)

var runArgs = []string{"--set", "sets", "--worktree-1", "w1", "--worktree-2", "w2", "--worktree-3", "w3", "--agent", "deepseek", "--change-set", "cs-1", "--tag", "v1", "--cache-bytes", "4096", "--record", "run.json"}

// runRoot builds a town root with every input a good run line reads.
func runRoot(t *testing.T) string {
	root := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, d := range []string{"rehearsal", "w1", "w2", "w3", ".runtime/host-load", "sets"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(".runtime/host-load/"+time.Now().Format("2006-01-02")+".log", "2026-11-27T10:00:00Z load1=1.00 load5=1.00 load15=1.00 ncpu=24\n")
	for i := 1; i <= 16; i++ {
		write(fmt.Sprintf("sets/%02d.md", i), "# bead\n\nbody\n\n## Acceptance\n")
	}
	return root
}

// callRun runs runCommand and returns its code with what it wrote.
func callRun(args []string, getwd func() (string, error), execute func(context.Context, rehearsal.Options, io.Writer, io.Writer) int) (int, string, string) {
	var stdout, stderr strings.Builder
	code := runCommand(context.Background(), args, getwd, execute, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRehearsalCommandRun(t *testing.T) {
	t.Parallel()
	root := runRoot(t)
	for _, tc := range []struct {
		name  string
		args  []string
		getwd func() (string, error)
		code  int
		want  string
	}{
		{"refused command line", nil, func() (string, error) { return "", fmt.Errorf("unused") }, 2, "--set"},
		{"unreadable working directory", runArgs, func() (string, error) { return "", fmt.Errorf("gone") }, 2, "gone"},
		{"refused input", runArgs, func() (string, error) { return t.TempDir(), nil }, 2, "rehearsal"},
		{"good line", runArgs, func() (string, error) { return root, nil }, 7, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got rehearsal.Options
			calls := 0
			execute := func(_ context.Context, o rehearsal.Options, _, _ io.Writer) int { got, calls = o, calls+1; return 7 }
			code, stdout, stderr := callRun(tc.args, tc.getwd, execute)
			if code != tc.code || stdout != "" {
				t.Fatalf("runCommand = %d, stdout %q, want %d and empty stdout (stderr %q)", code, stdout, tc.code, stderr)
			}
			if tc.code == 2 {
				if calls != 0 || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, tc.want) {
					t.Errorf("calls %d, stderr %q; want no call and one line naming %q", calls, stderr, tc.want)
				}
			} else if calls != 1 || stderr != "" || got.Worktrees[0] != filepath.Join(root, "w1") || got.Path != filepath.Join(root, "run.json") || len(got.Tasks) != 16 {
				t.Errorf("execute got %+v after %d calls, stderr %q; want the run rooted at %s", got, calls, stderr, root)
			}
		})
	}
}
