package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// fakeGit answers each git call from its table, keyed by the subcommand, and
// records the directory and arguments of every call.
type fakeGit struct {
	out   map[string]string
	err   map[string]error
	dirs  []string
	calls []string
}

func (f *fakeGit) Git(dir string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	f.dirs = append(f.dirs, dir)
	f.calls = append(f.calls, key)
	if err := f.err[key]; err != nil {
		return "", err
	}
	return f.out[key], nil
}

const headSHA = "5f0c2a9e3b7d41c8a6e2f1d0b9c8a7f6e5d4c3b2"

// worktreeOn is a git that answers as a worktree on branch at headSHA.
func worktreeOn(branch string) *fakeGit {
	return &fakeGit{out: map[string]string{
		"branch --show-current": branch + "\n",
		"rev-parse HEAD":        headSHA + "\n",
	}}
}

// withGit points the harness at git and the caller's directory at dir. The town
// comes from GT_TOWN_ROOT, so the target finds it whatever dir is.
func withGit(h *harness, git *fakeGit, dir string) {
	h.surface.git = git.Git
	h.surface.getwd = func() (string, error) { return dir, nil }
	getenv := h.surface.getenv
	h.surface.getenv = func(name string) string {
		if name == "GT_TOWN_ROOT" {
			return h.town
		}
		return getenv(name)
	}
}

func TestRunDoneAddsWorktreeFacts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	caller := t.TempDir()
	git := worktreeOn("crew/a/gt-x")
	withGit(h, git, caller)
	if exit := h.run("gt", "done", "--bead", "gt-x"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !h.fake.launched {
		t.Fatal("gt done did not reach the target")
	}
	want := []string{"gt", "done", "--bead", "gt-x", "--branch", "crew/a/gt-x", "--head", headSHA}
	if !slices.Equal(h.fake.call.Argv, want) {
		t.Fatalf("target argv = %q, want %q", h.fake.call.Argv, want)
	}
	if !slices.Equal(git.calls, []string{"branch --show-current", "rev-parse HEAD"}) {
		t.Fatalf("git calls = %q, want branch then rev-parse", git.calls)
	}
	for _, dir := range git.dirs {
		if dir != caller {
			t.Fatalf("git ran in %q, want the caller's directory %q", dir, caller)
		}
	}
}

func TestRunDoneKeepsCallerFacts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "caller branch kept, head added",
			args: []string{"--bead", "gt-x", "--branch", "mine"},
			want: []string{"--bead", "gt-x", "--branch", "mine", "--head", headSHA},
		},
		{
			name: "caller head kept, branch added",
			args: []string{"--bead", "gt-x", "--head=abc"},
			want: []string{"--bead", "gt-x", "--head=abc", "--branch", "crew/a/gt-x"},
		},
		{
			name: "both kept, nothing added",
			args: []string{"--branch=mine", "--head", "abc"},
			want: []string{"--branch=mine", "--head", "abc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			withGit(h, worktreeOn("crew/a/gt-x"), t.TempDir())
			if exit := h.run(append([]string{"gt", "done"}, tt.args...)...); exit != 0 {
				t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
			}
			want := append([]string{"gt", "done"}, tt.want...)
			if !slices.Equal(h.fake.call.Argv, want) {
				t.Fatalf("target argv = %q, want %q", h.fake.call.Argv, want)
			}
		})
	}
}

func TestRunDoneNoFactsOutsideWorktree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		git  *fakeGit
	}{
		{
			name: "not a worktree",
			git: &fakeGit{err: map[string]error{
				"branch --show-current": gtwrap.ErrNotWorktree,
			}},
		},
		{
			name: "detached HEAD",
			git: &fakeGit{out: map[string]string{
				"branch --show-current": "\n",
				"rev-parse HEAD":        headSHA + "\n",
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			withGit(h, tt.git, t.TempDir())
			args := []string{"gt", "done", "--bead", "gt-x"}
			if exit := h.run(args...); exit != 0 {
				t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
			}
			if !slices.Equal(h.fake.call.Argv, args) {
				t.Fatalf("target argv = %q, want %q unchanged", h.fake.call.Argv, args)
			}
		})
	}
}

func TestRunDoneRefusesWhenGitCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		git     *fakeGit
		wantMsg string
	}{
		{
			name: "branch read fails",
			git: &fakeGit{err: map[string]error{
				"branch --show-current": errors.New("git timed out"),
			}},
			wantMsg: "read the branch",
		},
		{
			name: "HEAD read fails",
			git: &fakeGit{
				out: map[string]string{"branch --show-current": "crew/a/gt-x\n"},
				err: map[string]error{"rev-parse HEAD": errors.New("git timed out")},
			},
			wantMsg: "read the HEAD commit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			withGit(h, tt.git, t.TempDir())
			if exit := h.run("gt", "done", "--bead", "gt-x"); exit != 1 {
				t.Fatalf("run = %d, want 1", exit)
			}
			if h.fake.launched {
				t.Fatal("gt done reached the target with worktree facts it could not read")
			}
			if !strings.Contains(h.stderr.String(), tt.wantMsg) {
				t.Fatalf("stderr %q does not name %q", h.stderr, tt.wantMsg)
			}
		})
	}
}

func TestRunDoneRefusesWithoutGitRunner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt", "done", "--bead", "gt-x"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if h.fake.launched {
		t.Fatal("gt done reached the target with no git runner wired")
	}
}

func TestRunOtherToolsUnchanged(t *testing.T) {
	t.Parallel()
	tests := [][]string{
		{"bd", "done", "--bead", "gt-x"},
		{"gt", "status", "--line"},
		{"om", "done", "-m", "a b"},
	}
	for _, argv := range tests {
		t.Run(argv[0]+" "+argv[1], func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			git := worktreeOn("crew/a/gt-x")
			withGit(h, git, t.TempDir())
			if exit := h.run(argv...); exit != 0 {
				t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
			}
			if !slices.Equal(h.fake.call.Argv, argv) {
				t.Fatalf("target argv = %q, want %q unchanged", h.fake.call.Argv, argv)
			}
			if len(git.calls) != 0 {
				t.Fatalf("git ran for %q: %q", argv, git.calls)
			}
		})
	}
}
