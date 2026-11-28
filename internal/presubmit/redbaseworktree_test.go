package presubmit

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// worktreeGit answers the git commands RedBaseWorktree runs from canned results, and records each call, so the unit tier runs no process and touches no tree.
type worktreeGit struct {
	mergeBase string
	mergeErr  error
	addErr    error
	removeErr error
	calls     [][]string
}

func (g *worktreeGit) run(args []string) ([]byte, error) {
	g.calls = append(g.calls, args)
	switch args[0] {
	case "merge-base":
		if g.mergeErr != nil {
			return nil, g.mergeErr
		}
		return []byte(g.mergeBase + "\n"), nil
	case "worktree":
		if args[1] == "add" {
			return nil, g.addErr
		}
		return nil, g.removeErr
	}
	return nil, fmt.Errorf("unexpected git %v", args)
}

// TestRedBaseWorktree: the tree is added detached at the merge base under dir, and its remove runs git worktree remove on that path (gt-8h4cxu).
func TestRedBaseWorktree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tree := filepath.Join(dir, "red-base")
	g := &worktreeGit{mergeBase: "abc123"}

	got, remove, err := RedBaseWorktree(g.run, "main", dir)
	if err != nil {
		t.Fatalf("RedBaseWorktree: %v", err)
	}
	if got != tree {
		t.Errorf("tree = %q, want %q", got, tree)
	}
	wantAdd := []string{"worktree", "add", "--detach", "--end-of-options", tree, "abc123"}
	if !slices.ContainsFunc(g.calls, func(c []string) bool { return slices.Equal(c, wantAdd) }) {
		t.Errorf("calls = %v, want a call %v", g.calls, wantAdd)
	}
	if err := remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	wantRemove := []string{"worktree", "remove", "--force", "--end-of-options", tree}
	if last := g.calls[len(g.calls)-1]; !slices.Equal(last, wantRemove) {
		t.Errorf("last call = %v, want %v", last, wantRemove)
	}
}

// TestRedBaseWorktreeRefusals holds one row per refusal: each returns an error, an empty tree and a nil remove (gt-8h4cxu).
func TestRedBaseWorktreeRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		base    string
		dir     string
		git     worktreeGit
		wantErr string
	}{
		{"an empty base is refused", "", "/d", worktreeGit{mergeBase: "abc"}, "empty base"},
		{"an empty dir is refused", "main", "", worktreeGit{mergeBase: "abc"}, "empty dir"},
		{"a failed merge-base is refused", "main", "/d", worktreeGit{mergeErr: errors.New("no base")}, "merge-base"},
		{"a failed worktree add is refused", "main", "/d", worktreeGit{mergeBase: "abc", addErr: errors.New("exists")}, "worktree add"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.git
			tree, remove, err := RedBaseWorktree(g.run, tt.base, tt.dir)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want one naming %q", err, tt.wantErr)
			}
			if tree != "" {
				t.Errorf("tree = %q, want empty", tree)
			}
			if remove != nil {
				t.Errorf("remove is non-nil, want nil on refusal")
			}
		})
	}
}

// TestRedBaseWorktreeRemoveFailure: a failed git worktree remove returns its git error from remove (gt-8h4cxu).
func TestRedBaseWorktreeRemoveFailure(t *testing.T) {
	t.Parallel()
	gitErr := errors.New("locked")
	g := &worktreeGit{mergeBase: "abc", removeErr: gitErr}
	_, remove, err := RedBaseWorktree(g.run, "main", "/d")
	if err != nil {
		t.Fatalf("RedBaseWorktree: %v", err)
	}
	if err := remove(); !errors.Is(err, gitErr) {
		t.Errorf("remove error = %v, want one wrapping %v", err, gitErr)
	}
}
