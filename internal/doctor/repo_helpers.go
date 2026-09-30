package doctor

import (
	"errors"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// currentBranch is the branch g's HEAD is on, "" when HEAD is detached (git
// branch --show-current).
func currentBranch(g Repo) (string, error) {
	branch, err := g.CurrentBranch()
	if err != nil {
		return "", err
	}
	if branch == "HEAD" {
		return "", nil
	}
	return branch, nil
}

// gitOutput is what a failed git call printed: its stderr, or its stdout
// when stderr is empty, as CombinedOutput reported it.
func gitOutput(err error) string {
	var gerr *git.GitError
	if errors.As(err, &gerr) {
		if s := strings.TrimSpace(gerr.Stderr); s != "" {
			return s
		}
		if s := strings.TrimSpace(gerr.Stdout); s != "" {
			return s
		}
	}
	return err.Error()
}
