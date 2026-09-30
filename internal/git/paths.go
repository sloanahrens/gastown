package git

import (
	"strings"
)

// Path and repository-state queries doctor's checks make, and the repairs
// they apply. Paths are relative to the working directory, as git takes them.

// RemoveRemote deletes a remote and its remote-tracking refs.
func (g *Git) RemoveRemote(name string) error {
	_, err := g.run("remote", "remove", name)
	return err
}

// PullRebase rebases the current branch onto its upstream (git pull --rebase).
func (g *Git) PullRebase() error {
	_, err := g.run("pull", "--rebase")
	return err
}

// IsBareRepository reports whether the repository is bare.
func (g *Git) IsBareRepository() (bool, error) {
	out, err := g.run("rev-parse", "--is-bare-repository")
	if err != nil {
		return false, err
	}
	return out == "true", nil
}

// IsTracked reports whether path is in the index.
func (g *Git) IsTracked(path string) (bool, error) {
	out, err := g.run("ls-files", "--", path)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// IsIgnored reports whether git ignores path (git check-ignore -q). Exit
// status 1 is "not ignored"; any other failure is an error.
func (g *Git) IsIgnored(path string) (bool, error) {
	_, err := g.run("check-ignore", "-q", "--", path)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// PathChanged reports whether path differs from HEAD, unstaged or staged
// (git diff --quiet, then git diff --cached --quiet).
func (g *Git) PathChanged(path string) (bool, error) {
	for _, args := range [][]string{{"diff", "--quiet", "--", path}, {"diff", "--cached", "--quiet", "--", path}} {
		if _, err := g.run(args...); err != nil {
			if exitCode(err) == 1 {
				return true, nil
			}
			return false, err
		}
	}
	return false, nil
}

// UntrackedPaths returns the untracked, unignored paths under pathspec as
// git status --porcelain --ignored lists them ("??" entries; a wholly
// untracked directory is one entry ending in "/").
func (g *Git) UntrackedPaths(pathspec string) ([]string, error) {
	out, err := g.runOutput("status", "--porcelain", "--ignored", "--", pathspec)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "?? "); ok {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// DisableSparseCheckout turns sparse checkout off and restores every file
// it hid (git sparse-checkout disable).
func (g *Git) DisableSparseCheckout() error {
	_, err := g.run("sparse-checkout", "disable")
	return err
}
