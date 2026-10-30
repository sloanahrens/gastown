package repoguard

import (
	"errors"
	"slices"
	"testing"
)

// unreadableGit is the Git a guard sees when git cannot run. Its IsTracked
// answers true, as CLIGit does on every error but exit status 1 (pinned by
// TestCLIGitIsTrackedTable), and its listings fail.
type unreadableGit struct{}

func (unreadableGit) LsFiles(root string, nul bool, pathspec ...string) ([]string, error) {
	return nil, errors.New("git: not found")
}

func (unreadableGit) LsDeleted(root string, pathspec ...string) ([]string, error) {
	return nil, errors.New("git: not found")
}

func (unreadableGit) IsTracked(root, path string) bool { return true }

// TestIssuesGuardFailsClosedWhenGitCannotRun holds the issues guard to its
// fail-closed contract: when git cannot answer whether .beads/issues.jsonl is
// tracked, the guard reports the problem and the run fails, it does not pass.
func TestIssuesGuardFailsClosedWhenGitCannotRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		git  Git
	}{
		{name: "a Git whose IsTracked reports tracked", git: unreadableGit{}},
		{
			name: "the CLI seam when git cannot start",
			git: CLIGit{ex: &fakeExec{run: func([]string) (string, error) {
				return "", errors.New("exec: \"git\": executable file not found in $PATH")
			}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := Env{
				Git:      tc.git,
				Exec:     &fakeExec{},
				LookPath: lookPathFound,
				ReadFile: func(string) ([]byte, error) { return []byte("module example.com/x\n"), nil },
			}
			rep := Check("/repo", env)
			if !rep.Failed() {
				t.Error("Failed() = false, want true: a git that cannot run must fail the run")
			}
			if !slices.Contains(rep.Problems, IssuesJSONLProblem) {
				t.Errorf("problems = %q, want the issues problem %q", rep.Problems, IssuesJSONLProblem)
			}
		})
	}
}
