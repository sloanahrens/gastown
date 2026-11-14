package daemon

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// overrunSandboxGitCall is one call the fake runner saw.
type overrunSandboxGitCall struct {
	dir  string
	args []string
}

// overrunSandboxGitRecorder is a fake in-sandbox runner: it records each call
// and answers every one with out and err.
func overrunSandboxGitRecorder(calls *[]overrunSandboxGitCall, out []byte, err error) func(dir string, args ...string) ([]byte, error) {
	return func(dir string, args ...string) ([]byte, error) {
		*calls = append(*calls, overrunSandboxGitCall{dir: dir, args: append([]string(nil), args...)})
		return out, err
	}
}

// TestOverrunSandboxGitExecForwardsTheCallToTheRunner: a Git built over the
// adapter runs a call through the runner, which receives the call's directory
// and arguments, and the runner's output and error come back from the Git
// method.
func TestOverrunSandboxGitExecForwardsTheCallToTheRunner(t *testing.T) {
	t.Parallel()
	cause := errors.New("sandbox is gone")
	tests := []struct {
		name    string
		answer  []byte
		runErr  error
		wantOut string
		wantErr error
	}{
		{name: "the runner's output comes back", answer: []byte("0f9c2a\n"), wantOut: "0f9c2a"},
		{name: "the runner's error comes back", answer: nil, runErr: cause, wantErr: cause},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var calls []overrunSandboxGitCall
			g := git.NewGitWithExec(dir, sandboxGitExec(overrunSandboxGitRecorder(&calls, tt.answer, tt.runErr)))

			got, err := g.Rev("refs/heads/main")

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Rev error = %v, want it to wrap %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Rev error = %v, want none", err)
			}
			if got != tt.wantOut {
				t.Errorf("Rev = %q, want %q", got, tt.wantOut)
			}
			want := []overrunSandboxGitCall{{dir: dir, args: []string{"rev-parse", "refs/heads/main"}}}
			if !reflect.DeepEqual(calls, want) {
				t.Errorf("runner calls = %+v, want %+v", calls, want)
			}
		})
	}
}

// TestOverrunSandboxGitExecRefusesStdinBeforeTheRunner: a call whose stdin the
// runner cannot carry is refused, and the runner never sees it. PatchIDs runs
// git log first and then feeds its output to git patch-id on stdin, so the
// runner's one call is the log and the error names stdin.
func TestOverrunSandboxGitExecRefusesStdinBeforeTheRunner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var calls []overrunSandboxGitCall
	g := git.NewGitWithExec(dir, sandboxGitExec(overrunSandboxGitRecorder(&calls, []byte("a diff\n"), nil)))

	if _, err := g.PatchIDs("main", "HEAD"); !errors.Is(err, errSandboxGitStdin) {
		t.Fatalf("PatchIDs error = %v, want %v", err, errSandboxGitStdin)
	}
	want := []overrunSandboxGitCall{{dir: dir, args: []string{"log", "--no-merges", "-p", "--no-color", "main..HEAD"}}}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("runner calls = %+v, want only the log call %+v: the stdin call must be refused before it reaches the runner", calls, want)
	}
}
