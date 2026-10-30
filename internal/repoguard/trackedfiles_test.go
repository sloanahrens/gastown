package repoguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeGit answers the Git seam from fixed data. It records nothing: each case
// states the index it expects the guard to read.
type fakeGit struct {
	tracked map[string]bool
	files   []string
	lsErr   error
}

func (f *fakeGit) LsFiles(root string, nul bool, pathspec ...string) ([]string, error) {
	return f.files, f.lsErr
}

func (f *fakeGit) IsTracked(root, path string) bool {
	return f.tracked[path]
}

// TestCheckIssuesJSONLTable pins the guard to the index. The untracked cases
// write a real file, so a guard that looks at the disk instead of the index
// returns the wrong answer for them.
func TestCheckIssuesJSONLTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tracked bool
		onDisk  bool
		want    bool
	}{
		{name: "tracked export fails", tracked: true, onDisk: true, want: true},
		{name: "tracked export deleted from disk still fails", tracked: true, onDisk: false, want: true},
		{name: "untracked local export passes", tracked: false, onDisk: true, want: false},
		{name: "absent export passes", tracked: false, onDisk: false, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.onDisk {
				if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".beads", "issues.jsonl"), []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			g := &fakeGit{tracked: map[string]bool{}}
			if tc.tracked {
				g.tracked[".beads/issues.jsonl"] = true
			}
			if got := CheckIssuesJSONL(g, root); got != tc.want {
				t.Errorf("CheckIssuesJSONL() = %v, want %v", got, tc.want)
			}
		})
	}
}

// exitStatus is a failed command's error: it carries the exit code the way
// os/exec's *ExitError does, without a process behind it.
type exitStatus int

func (s exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(s)) }

func (s exitStatus) ExitCode() int { return int(s) }

// TestCLIGitLsFilesTable checks the argv git gets, and that its output is split
// on the separator the call asked for. A failed listing is returned, not
// swallowed.
func TestCLIGitLsFilesTable(t *testing.T) {
	t.Parallel()
	failure := errors.New("exit status 128")
	tests := []struct {
		name string
		nul  bool
		out  string
		err  error
		want []string
		args []string
	}{
		{
			name: "NUL-separated names split on NUL",
			nul:  true,
			out:  "a.go\x00dir/b.go\x00",
			want: []string{"a.go", "dir/b.go"},
			args: []string{"ls-files", "-z", "--", "*.go"},
		},
		{
			name: "newline-separated names split on newline",
			out:  "a.go\nb.go\n",
			want: []string{"a.go", "b.go"},
			args: []string{"ls-files", "--", "*.go"},
		},
		{
			name: "a failed listing is an error",
			nul:  true,
			err:  failure,
			args: []string{"ls-files", "-z", "--", "*.go"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &fakeExec{run: func([]string) (string, error) { return tc.out, tc.err }}
			got, err := CLIGit{ex: e}.LsFiles("/repo", tc.nul, "*.go")
			if !errors.Is(err, tc.err) {
				t.Errorf("err = %v, want %v", err, tc.err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("paths = %q, want %q", got, tc.want)
			}
			if len(e.calls) != 1 || e.calls[0].dir != "/repo" || e.calls[0].name != "git" || !slices.Equal(e.calls[0].args, tc.args) {
				t.Errorf("calls = %+v, want one git %q in /repo", e.calls, tc.args)
			}
		})
	}
}

// TestCLIGitIsTrackedTable pins what the exit status means. Only status 1, the
// path not being in the index, reads as untracked. Every other failure reads
// as tracked, so a broken git cannot make the guard pass.
func TestCLIGitIsTrackedTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "listed in the index is tracked", err: nil, want: true},
		{name: "exit status 1 is untracked", err: exitStatus(1), want: false},
		{name: "any other exit status is tracked", err: exitStatus(128), want: true},
		{name: "a failure without an exit status is tracked", err: errors.New("git: not found"), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &fakeExec{run: func([]string) (string, error) { return "", tc.err }}
			if got := (CLIGit{ex: e}).IsTracked("/repo", ".beads/issues.jsonl"); got != tc.want {
				t.Errorf("IsTracked() = %v, want %v", got, tc.want)
			}
			wantArgs := []string{"ls-files", "--error-unmatch", "--", ".beads/issues.jsonl"}
			if len(e.calls) != 1 || e.calls[0].dir != "/repo" || e.calls[0].name != "git" || !slices.Equal(e.calls[0].args, wantArgs) {
				t.Errorf("calls = %+v, want one git %q in /repo", e.calls, wantArgs)
			}
		})
	}
}

// TestCheckIssuesJSONLProblemText pins the guard's message to the script's
// line byte for byte, since operators grep for it.
func TestCheckIssuesJSONLProblemText(t *testing.T) {
	t.Parallel()
	const want = "repo-guards: .beads/issues.jsonl is tracked; the Dolt server is the only beads backend (git rm --cached it)"
	if IssuesJSONLProblem != want {
		t.Errorf("IssuesJSONLProblem = %q, want %q", IssuesJSONLProblem, want)
	}
}
