//go:build integration

package rehearsal

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bareRepo makes an empty bare repository under the test's temp dir and returns its path.
func bareRepo(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src.git")
	if err := exec.Command("git", "init", "--bare", "-q", src).Run(); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}
	return src
}

func TestIntegrationRehearsalExecRunReturnsStdoutOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		argv  func(t *testing.T) []string
		check func(t *testing.T, out []byte)
	}{
		{
			name: "stdout of a successful command is returned",
			argv: func(*testing.T) []string { return []string{"git", "--version"} },
			check: func(t *testing.T, out []byte) {
				if !strings.HasPrefix(string(out), "git version ") {
					t.Errorf("ExecRun stdout = %q, want the git version line", out)
				}
			},
		},
		{
			// git clone of a bare repo exits 0 and writes "Cloning into" to stderr only.
			name: "stderr of a successful command is not returned",
			argv: func(t *testing.T) []string {
				return []string{"git", "clone", bareRepo(t), filepath.Join(t.TempDir(), "dst")}
			},
			check: func(t *testing.T, out []byte) {
				if len(out) != 0 {
					t.Errorf("ExecRun stdout = %q, want none: the clone writes only to stderr", out)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := ExecRun(context.Background(), tc.argv(t))
			if err != nil {
				t.Fatalf("ExecRun: %v", err)
			}
			tc.check(t, out)
		})
	}
}

func TestIntegrationRehearsalExecRunRefusesWithoutOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		argv     []string
		wantIn   string
		notIn    string
		wantExit bool
	}{
		{name: "empty argv", argv: nil, wantIn: "empty argv"},
		{
			name:     "failing command names its first two words",
			argv:     []string{"git", "no-such-subcommand"},
			wantIn:   "git no-such-subcommand",
			notIn:    "is not a git command",
			wantExit: true,
		},
		{
			name:     "failing command leaves out arguments past the second",
			argv:     []string{"git", "no-such-subcommand", "--token=sekret"},
			wantIn:   "git no-such-subcommand",
			notIn:    "sekret",
			wantExit: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := ExecRun(context.Background(), tc.argv)
			if err == nil {
				t.Fatalf("ExecRun(%q) = %q, want an error", tc.argv, out)
			}
			if out != nil {
				t.Errorf("ExecRun output on error = %q, want nil", out)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("ExecRun error = %q, want it to name %q", err, tc.wantIn)
			}
			if tc.notIn != "" && strings.Contains(err.Error(), tc.notIn) {
				t.Errorf("ExecRun error = %q, want no captured output (%q)", err, tc.notIn)
			}
			var exitErr *exec.ExitError
			if got := errors.As(err, &exitErr); got != tc.wantExit {
				t.Errorf("errors.As(*exec.ExitError) = %v, want %v: the cause is wrapped with %%w only for a failed command", got, tc.wantExit)
			}
		})
	}
}
