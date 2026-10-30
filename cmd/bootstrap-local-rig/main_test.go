package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/rigbootstrap"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// recordingExec answers every program with exit 0 and records its argv, so no
// test starts a program. Each entry is the program name followed by its arguments.
type recordingExec struct {
	argv [][]string
}

func (r *recordingExec) exec(_ context.Context, _, name string, args []string, _, _ io.Writer) (int, error) {
	r.argv = append(r.argv, append([]string{name}, args...))
	return 0, nil
}

// TestExitStatusReportsStartFailure: a program that could not start is an error
// for the run to report, not an exit code that would read as a gt failure.
func TestExitStatusReportsStartFailure(t *testing.T) {
	t.Parallel()
	startErr := errors.New("fork/exec gt: no such file or directory")
	code, err := exitStatus(startErr)
	if !errors.Is(err, startErr) {
		t.Fatalf("exitStatus(start failure) error = %v, want %v", err, startErr)
	}
	if code != 0 {
		t.Fatalf("exitStatus(start failure) code = %d, want 0", code)
	}
}

func TestMainWiresRun(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	town := filepath.Join(root, "town")
	repo := filepath.Join(root, "repo")
	for _, dir := range []string{town, repo} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
		wantRuns   int // programs started: git and gt for a run that reaches gt
	}{
		{
			name:       "help prints the usage to stdout and exits 0",
			args:       []string{"-h"},
			wantCode:   0,
			wantStdout: rigbootstrap.Usage,
		},
		{
			name:       "success prints the rig path",
			args:       []string{"--town-root", town, "--rig", "nightrider", "--local-repo", repo},
			wantCode:   0,
			wantStdout: "Rig bootstrapped at " + town + "/nightrider\n",
			wantRuns:   2,
		},
		{
			name:       "missing argument prints the message and usage to stderr and exits 2",
			args:       nil,
			wantCode:   2,
			wantStderr: "Missing required argument: TOWN_ROOT\n" + rigbootstrap.Usage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingExec{}
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}

			if code := run(context.Background(), tt.args, stdout, stderr, rec.exec); code != tt.wantCode {
				t.Fatalf("run() = %d, want %d", code, tt.wantCode)
			}
			if stdout.String() != tt.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if stderr.String() != tt.wantStderr {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
			if len(rec.argv) != tt.wantRuns {
				t.Fatalf("programs started = %q, want %d", rec.argv, tt.wantRuns)
			}
		})
	}
}
