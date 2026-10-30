package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bumpversion"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// runCapture runs the command over env and returns its exit code and everything it
// wrote to the error stream. A command line the parser refuses never reaches the run,
// so those callers pass a zero Env.
func runCapture(args []string, env bumpversion.Env) (int, string) {
	var errw bytes.Buffer
	code := run(args, env, &errw)
	return code, errw.String()
}

// memTree is the file seam for the command tests: an in-memory tree, so no test
// touches the working copy.
type memTree map[string]string

func (m memTree) ReadFile(name string) ([]byte, error) {
	text, ok := m[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return []byte(text), nil
}

func (m memTree) WriteFile(name string, data []byte) error {
	m[name] = string(data)
	return nil
}

// repoTree is a repository at 1.2.1 with the two version files the command always
// edits. It has no flake.nix and no CHANGELOG.md, so the run skips both.
func repoTree() memTree {
	return memTree{
		"internal/cmd/version.go":  "package cmd\n\nvar (\n\tVersion = \"1.2.1\"\n)\n",
		"npm-package/package.json": "{\n  \"version\": \"1.2.1\"\n}\n",
	}
}

// testEnv wires the command to tree, with git reporting status for every call (0 is a
// clean tree), no nix on PATH and no process to run.
func testEnv(tree memTree, status int, out io.Writer) bumpversion.Env {
	return bumpversion.Env{
		FS:  tree,
		Git: bumpversion.Git{Run: func(string, ...string) (int, error) { return status, nil }},
		Exec: bumpversion.Exec{
			Run:       func(string, []string, string, ...string) ([]byte, []byte, error) { return nil, nil, nil },
			LookPath:  func(string) (string, error) { return "", errors.New("not found") },
			MkdirTemp: func(string, string) (string, error) { return "", errors.New("no scratch directory") },
		},
		Root:       ".",
		Home:       "/home/op",
		Date:       "2026-10-30",
		Executable: func(string) bool { return false },
		Out:        out,
	}
}

// TestMainUsageAndUnknownOption: an empty command line prints Usage alone, and
// an unknown option names itself before the same Usage, both exiting 1.
func TestMainUsageAndUnknownOption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantHead string // first line of the error stream; Usage for the empty line
	}{
		{name: "no arguments", args: nil, wantHead: "Usage: bump-version <version> [--commit] [--tag] [--install] [--allow-dirty]"},
		{name: "unknown option", args: []string{"0.2.0", "--bogus"}, wantHead: "Unknown option '--bogus'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, got := runCapture(tc.args, bumpversion.Env{})
			if code != 1 {
				t.Errorf("run(%v) exit = %d, want 1", tc.args, code)
			}
			if head := strings.SplitN(got, "\n", 2)[0]; head != tc.wantHead {
				t.Errorf("run(%v) first line = %q, want %q", tc.args, head, tc.wantHead)
			}
			if !strings.HasSuffix(got, bumpversion.Usage) {
				t.Errorf("run(%v) did not end with Usage, got:\n%s", tc.args, got)
			}
		})
	}
}

// TestMainRefusals: a bad version and a bad flag combination each print their
// own explanation, with no Usage, and exit 1.
func TestMainRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string // the whole error stream
	}{
		{
			name: "invalid version",
			args: []string{"1.0"},
			want: "Invalid version format '1.0'\n" +
				"Expected semantic version format: MAJOR.MINOR.PATCH (e.g., 0.2.0)\n",
		},
		{
			name: "tag without commit",
			args: []string{"0.2.0", "--tag"},
			want: "--tag requires --commit\n",
		},
		{
			name: "push refused",
			args: []string{"0.2.0", "--push"},
			want: "--push is not offered: this command never runs git push, because main lands only through the landing worker and the tag is pushed by the release procedure\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, got := runCapture(tc.args, bumpversion.Env{})
			if code != 1 {
				t.Errorf("run(%v) exit = %d, want 1", tc.args, code)
			}
			if got != tc.want {
				t.Errorf("run(%v) wrote %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestMainRunsTheCommand: a valid command line bumps the tree and exits 0 with nothing
// on the error stream. With no flags it stops at the review hint. With --commit and
// --tag it makes the commit and the tag and prints the next steps, which name the push
// but never run it.
func TestMainRunsTheCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantOut string
	}{
		{name: "no flags", args: []string{"1.3.0"}, wantOut: "  bump-version 1.3.0 --commit --tag --install\n"},
		{name: "commit and tag", args: []string{"1.3.0", "--commit", "--tag"}, wantOut: "  git push origin main\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := repoTree()
			var out, errw bytes.Buffer
			if code := run(tc.args, testEnv(tree, 0, &out), &errw); code != 0 {
				t.Fatalf("run(%v) exit = %d, want 0; error stream:\n%s", tc.args, code, errw.String())
			}
			if errw.Len() != 0 {
				t.Errorf("run(%v) wrote %q to the error stream, want nothing", tc.args, errw.String())
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("run(%v) output lacks %q:\n%s", tc.args, tc.wantOut, out.String())
			}
			if got := tree["internal/cmd/version.go"]; !strings.Contains(got, `Version = "1.3.0"`) {
				t.Errorf("version.go after run(%v) = %q, want Version = \"1.3.0\"", tc.args, got)
			}
		})
	}
}

// missingProgram is a command constructor whose command carries a start error, as a
// program that is not on PATH does. Start returns that error before it creates a
// process, so a runner's start-failure branch runs without a subprocess.
func missingProgram(_ string, _ ...string) *exec.Cmd {
	return &exec.Cmd{Err: errors.New("program not found")}
}

// TestRunnersReportAStartFailure: a git runner whose program cannot start returns that
// error with no status, and so does a process runner, which returns no output.
func TestRunnersReportAStartFailure(t *testing.T) {
	t.Parallel()
	code, err := runGitWith(missingProgram, ".", "status")
	if err == nil || code != 0 {
		t.Errorf("runGitWith = (%d, %v), want (0, an error)", code, err)
	}
	stdout, stderr, err := runProcessWith(missingProgram, ".", nil, "make", "install")
	if err == nil || len(stdout) != 0 || len(stderr) != 0 {
		t.Errorf("runProcessWith = (%q, %q, %v), want no output and an error", stdout, stderr, err)
	}
}

// TestMainRefusesTheRun: a run the command refuses prints the refusal alone on the
// error stream and exits 1. Here that is a dirty tree under --commit, and a directory
// that is not the repository root.
func TestMainRefusesTheRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tree   memTree
		status int
		args   []string
		want   string
	}{
		{
			name:   "dirty tree under --commit",
			tree:   repoTree(),
			status: 1,
			args:   []string{"1.3.0", "--commit"},
			want:   "Error: Cannot auto-commit with existing uncommitted changes\n",
		},
		{
			name:   "not the repository root",
			tree:   memTree{},
			status: 0,
			args:   []string{"1.3.0"},
			want:   "Error: Must run from repository root\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			code, got := runCapture(tc.args, testEnv(tc.tree, tc.status, &out))
			if code != 1 {
				t.Errorf("run(%v) exit = %d, want 1", tc.args, code)
			}
			if got != tc.want {
				t.Errorf("run(%v) error stream = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
