package bdskew

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

var (
	// ErrNoRunner reports a GitLister whose Run is nil: a lister that cannot run
	// git returns an error, never a listing that MaxMigration would read as
	// level zero.
	ErrNoRunner = errors.New("bdskew: GitLister has no git runner")
	// ErrNoRepo reports a GitLister whose Repo is empty, which would let git
	// read whichever repository the caller's directory holds.
	ErrNoRepo = errors.New("bdskew: GitLister has no repository")
	// ErrBadDir reports a listing directory that cannot reach git's argv as a
	// pathspec: it is empty, absolute, or shaped like a git option.
	ErrBadDir = errors.New("bdskew: listing dir is not a relative tree path")
)

// GitLister lists the entries of a bare clone's git tree. ListTree starts no
// process of its own: Run is the seam, supplied as real git by the caller and
// as a fake by a test.
type GitLister struct {
	// Run runs git with args in dir and returns its standard output; an error
	// means git did not run or exited non-zero.
	Run func(ctx context.Context, dir string, args ...string) ([]byte, error)
	// Repo is the bare clone every listing reads.
	Repo string
}

var _ Lister = GitLister{}

// ListTree returns the base names of the entries directly under dir at commit,
// asking git for `-C Repo ls-tree --name-only -z commit dir/` in Repo; -z makes
// git print each entry NUL-terminated and unquoted, so a name that holds a
// newline, tab or non-ASCII byte comes back as itself.
//
// Both commit and dir reach git's argv, so a commit that is not a full
// lowercase sha and a dir that is not a relative tree path are refused first. A
// failed git is an error, and a listing with no entry is ErrEmptyListing: a
// commit that does not carry dir says nothing about its schema level, so it is
// never level zero.
func (g GitLister) ListTree(ctx context.Context, commit, dir string) ([]string, error) {
	if g.Run == nil {
		return nil, ErrNoRunner
	}
	if g.Repo == "" {
		return nil, ErrNoRepo
	}
	if !commitForm.MatchString(commit) {
		return nil, fmt.Errorf("%w: %q", ErrBadCommit, commit)
	}
	if !treeDirOK(dir) {
		return nil, fmt.Errorf("%w: %q", ErrBadDir, dir)
	}
	out, err := g.Run(ctx, g.Repo, "-C", g.Repo, "ls-tree", "--name-only", "-z", commit, dir+"/")
	if err != nil {
		return nil, fmt.Errorf("git ls-tree --name-only -z %s %s/: %w", commit, dir, err)
	}
	var names []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" {
			continue
		}
		names = append(names, path.Base(entry))
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("git ls-tree --name-only -z %s %s/ printed no entry: %w", commit, dir, ErrEmptyListing)
	}
	return names, nil
}

// treeDirOK reports whether dir is a relative tree path this lister may hand
// git as a pathspec: not empty, not absolute, not shaped like an option, and
// without a `..` segment, which would walk the listing out of the tree.
func treeDirOK(dir string) bool {
	if dir == "" || path.IsAbs(dir) || strings.HasPrefix(dir, "-") {
		return false
	}
	for _, segment := range strings.Split(dir, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

// BeadsRigRepo returns the bare clone of the beads rig under townRoot, the
// repository whose commits a bd pin names.
func BeadsRigRepo(townRoot string) string {
	return constants.RigBareRepoPath(filepath.Join(townRoot, "beads"))
}
