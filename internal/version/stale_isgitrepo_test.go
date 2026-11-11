package version

import (
	"context"
	"errors"
	"testing"
)

// TestIsGitRepo_AnswersForWorktreesAndBareRepositories: a worktree answers
// --is-inside-work-tree, a bare repository such as a rig's .repo.git answers
// --is-bare-repository (the first is false for it), and a directory that is
// neither is no repository. The git is scripted: nothing here runs git.
func TestIsGitRepo_AnswersForWorktreesAndBareRepositories(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		inside   string
		bare     string
		gitFails bool
		want     bool
	}{
		{name: "a worktree", inside: "true", bare: "false", want: true},
		{name: "a bare repository", inside: "false", bare: "true", want: true},
		{name: "a plain directory", inside: "false", bare: "false", want: false},
		{name: "a directory git cannot read", gitFails: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			scripted := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if tc.gitFails {
					return nil, errors.New("fatal: not a git repository")
				}
				if len(args) != 2 || args[0] != "rev-parse" {
					t.Fatalf("git asked %q, want a rev-parse question", args)
				}
				asked = append(asked, args[1])
				switch args[1] {
				case "--is-inside-work-tree":
					return []byte(tc.inside + "\n"), nil
				case "--is-bare-repository":
					return []byte(tc.bare + "\n"), nil
				}
				t.Fatalf("git asked %q, want --is-inside-work-tree or --is-bare-repository", args[1])
				return nil, nil
			}
			got := checker{git: scripted}.isGitRepo("/town/gastown/.repo.git")
			if got != tc.want {
				t.Errorf("isGitRepo = %v, want %v (asked %q)", got, tc.want, asked)
			}
			if tc.name == "a bare repository" && len(asked) != 2 {
				t.Errorf("a bare repository is found by the second question; asked %q", asked)
			}
		})
	}
}
