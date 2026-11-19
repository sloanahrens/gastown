package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// TestGitOpenerCarriesExecAndTimeout: an opener that carries a runner and a
// deadline opens a handle whose calls go through that runner under that
// deadline; a zero deadline sends none.
func TestGitOpenerCarriesExecAndTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{"deadline set", 7 * time.Second},
		{"zero deadline", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var calls []git.ExecCall
			rec := func(c git.ExecCall) (string, string, error) {
				calls = append(calls, c)
				if got := strings.Join(c.Args(), " "); got == "rev-parse --abbrev-ref HEAD" {
					return "main\n", "", nil
				}
				return "", "", errors.New("unscripted git call")
			}

			branch, err := gitOpener{exec: rec, timeout: tc.timeout}.Open(dir).CurrentBranch()
			if err != nil {
				t.Fatalf("CurrentBranch: %v", err)
			}
			if branch != "main" {
				t.Fatalf("CurrentBranch = %q, want main", branch)
			}
			if len(calls) != 1 {
				t.Fatalf("runner saw %d calls, want 1", len(calls))
			}
			if got := strings.Join(calls[0].Args(), " "); got != "rev-parse --abbrev-ref HEAD" {
				t.Errorf("call args = %q, want %q", got, "rev-parse --abbrev-ref HEAD")
			}
			if got := calls[0].Timeout(); got != tc.timeout {
				t.Errorf("call timeout = %v, want %v", got, tc.timeout)
			}
		})
	}
}

// TestGitOpenerExecAnswersWorktreeRoot: an opener that carries a runner asks
// that runner for the worktree top level instead of reading the file-system
// shortcut, which here would answer on its own.
func TestGitOpenerExecAnswersWorktreeRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	for _, tc := range []struct {
		name string
		top  string
		want bool
	}{
		{"runner reports the directory itself", dir, true},
		{"runner reports another tree", filepath.Join(dir, "elsewhere"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []git.ExecCall
			rec := func(c git.ExecCall) (string, string, error) {
				calls = append(calls, c)
				return tc.top + "\n", "", nil
			}

			if got := isWorktreeRoot(gitOpener{exec: rec}, dir); got != tc.want {
				t.Fatalf("isWorktreeRoot = %v, want %v", got, tc.want)
			}
			if len(calls) != 1 || strings.Join(calls[0].Args(), " ") != "rev-parse --show-toplevel" {
				t.Fatalf("runner saw %v, want one rev-parse --show-toplevel call", calls)
			}
		})
	}
}
