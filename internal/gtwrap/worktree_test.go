package gtwrap

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeGitExit is a git run that exited with a status, as exec reports one.
type fakeGitExit int

func (e fakeGitExit) Error() string { return "exit status" }
func (e fakeGitExit) ExitCode() int { return int(e) }

// answers is a Git that answers each subcommand from its table.
func answers(out map[string]string, err map[string]error) Git {
	return func(_ string, args ...string) (string, error) {
		key := strings.Join(args, " ")
		if e := err[key]; e != nil {
			return "", e
		}
		return out[key], nil
	}
}

func TestReadWorktreeFacts(t *testing.T) {
	t.Parallel()
	const sha = "5f0c2a9e3b7d41c8a6e2f1d0b9c8a7f6e5d4c3b2"
	tests := []struct {
		name    string
		git     Git
		want    WorktreeFacts
		wantOK  bool
		wantErr bool
	}{
		{
			name: "branch and head",
			git: answers(map[string]string{
				"branch --show-current": "crew/a/gt-x\n",
				"rev-parse HEAD":        sha + "\n",
			}, nil),
			want:   WorktreeFacts{Branch: "crew/a/gt-x", Head: sha},
			wantOK: true,
		},
		{
			name: "detached HEAD adds nothing",
			git: answers(map[string]string{
				"branch --show-current": "\n",
				"rev-parse HEAD":        sha + "\n",
			}, nil),
		},
		{
			name: "not a worktree adds nothing",
			git: answers(nil, map[string]error{
				"branch --show-current": ErrNotWorktree,
			}),
		},
		{
			name: "branch read fails",
			git: answers(nil, map[string]error{
				"branch --show-current": errors.New("timed out"),
			}),
			wantErr: true,
		},
		{
			name: "HEAD read fails",
			git: answers(map[string]string{
				"branch --show-current": "crew/a/gt-x\n",
			}, map[string]error{
				"rev-parse HEAD": errors.New("timed out"),
			}),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := ReadWorktreeFacts(tt.git, t.TempDir())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want an error: %t", err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("facts = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestGitErrorTellsNotARepositoryFromOtherFatals: git exits 128 for many
// failures; only its not-a-repository answer is "not a worktree". A repository
// git refuses (dubious ownership), a corrupt one and every other fatal text is an
// error carrying git's first line, so a broken git is refused and never reads as
// a plain directory.
func TestGitErrorTellsNotARepositoryFromOtherFatals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		err          error
		stderr       string
		wantNotWorkt bool
		wantDetail   string
	}{
		{name: "fatal with the not-a-repository text", err: fakeGitExit(gitFatal),
			stderr: "fatal: not a git repository (or any of the parent directories): .git\n", wantNotWorkt: true},
		{name: "fatal with dubious ownership", err: fakeGitExit(gitFatal),
			stderr:     "fatal: detected dubious ownership in repository at '/x'\nTo add an exception run: git config --global --add safe.directory /x\n",
			wantDetail: "detected dubious ownership"},
		{name: "fatal on a corrupt repository", err: fakeGitExit(gitFatal),
			stderr: "fatal: bad object HEAD\n", wantDetail: "bad object HEAD"},
		{name: "fatal with no stderr", err: fakeGitExit(gitFatal)},
		{name: "other status", err: fakeGitExit(1), stderr: "not a git repository"},
		{name: "timeout", err: fakeGitExit(-1)},
		{name: "plain error", err: errors.New("git: not found")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := gitError(tt.err, tt.stderr, []string{"branch", "--show-current"})
			if got == nil {
				t.Fatal("gitError returned nil, which would read as a clean run")
			}
			if errors.Is(got, ErrNotWorktree) != tt.wantNotWorkt {
				t.Fatalf("gitError = %v, want not-a-worktree %t", got, tt.wantNotWorkt)
			}
			if tt.wantDetail != "" && !strings.Contains(got.Error(), tt.wantDetail) {
				t.Errorf("gitError = %q, want git's first line %q", got, tt.wantDetail)
			}
		})
	}
}

// TestReadWorktreeFactsRefusesABrokenGit: a fatal that is not the
// not-a-repository answer reaches the caller as an error, not as "add nothing".
func TestReadWorktreeFactsRefusesABrokenGit(t *testing.T) {
	t.Parallel()
	broken := func(string, ...string) (string, error) {
		return "", gitError(fakeGitExit(gitFatal), "fatal: detected dubious ownership in repository at '/x'\n", []string{"branch", "--show-current"})
	}
	_, ok, err := ReadWorktreeFacts(broken, t.TempDir())
	if err == nil || ok || !strings.Contains(err.Error(), "dubious ownership") {
		t.Fatalf("ReadWorktreeFacts = ok %t, err %v; want a refusal naming dubious ownership", ok, err)
	}
}

// TestGitEnvDropsOverridesAndForcesEnglish: the variables that point git at
// another repository are removed, an inherited LC_ALL is replaced by C, and the
// rest of the environment is kept.
func TestGitEnvDropsOverridesAndForcesEnglish(t *testing.T) {
	t.Parallel()
	got := gitEnv([]string{"PATH=/bin", "GIT_DIR=/elsewhere/.git", "GIT_WORK_TREE=/w", "GIT_INDEX_FILE=/i", "LC_ALL=de_DE.UTF-8", "HOME=/h"})
	want := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C"}
	if !slices.Equal(got, want) {
		t.Fatalf("gitEnv = %q, want %q", got, want)
	}
}
