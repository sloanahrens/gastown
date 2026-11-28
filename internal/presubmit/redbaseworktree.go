package presubmit

import (
	"errors"
	"fmt"
	"path/filepath"
)

// redBaseTreeName names the worktree directory RedBaseWorktree makes under its dir.
const redBaseTreeName = "red-base"

// RedBaseWorktree adds a git worktree detached at the merge base of base and HEAD, under dir, and returns its path with a remove func that removes it. An empty base or dir, a failed merge-base or a failed add is an error with a nil remove, so no caller removes a tree that was never made (gt-8h4cxu).
func RedBaseWorktree(run Runner, base, dir string) (tree string, remove func() error, err error) {
	if base == "" {
		return "", nil, errors.New("red-base worktree: empty base")
	}
	if dir == "" {
		return "", nil, errors.New("red-base worktree: empty dir")
	}
	sha, err := mergeBase(run, base)
	if err != nil {
		return "", nil, fmt.Errorf("red-base worktree: %w", err)
	}
	tree = filepath.Join(dir, redBaseTreeName)
	if _, err := run([]string{"worktree", "add", "--detach", "--end-of-options", tree, sha}); err != nil {
		return "", nil, fmt.Errorf("git worktree add %s: %w", tree, err)
	}
	remove = func() error {
		if _, err := run([]string{"worktree", "remove", "--force", "--end-of-options", tree}); err != nil {
			return fmt.Errorf("git worktree remove %s: %w", tree, err)
		}
		return nil
	}
	return tree, remove, nil
}
