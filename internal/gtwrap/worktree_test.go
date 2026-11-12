package gtwrap

import (
	"errors"
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

func TestGitErrorMapsFatalStatusToNotWorktree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		err          error
		wantNotWorkt bool
	}{
		{name: "fatal status is not a worktree", err: fakeGitExit(gitFatal), wantNotWorkt: true},
		{name: "other status is an error", err: fakeGitExit(1)},
		{name: "timeout is an error", err: fakeGitExit(-1)},
		{name: "plain error is an error", err: errors.New("git: not found")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := gitError(tt.err, []string{"branch", "--show-current"})
			if errors.Is(got, ErrNotWorktree) != tt.wantNotWorkt {
				t.Fatalf("gitError = %v, want not-a-worktree %t", got, tt.wantNotWorkt)
			}
			if got == nil {
				t.Fatal("gitError returned nil, which would read as a clean run")
			}
		})
	}
}
