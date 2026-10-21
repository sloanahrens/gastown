package submission

import (
	"context"
)

// Clone is the bare clone the submission battery owns, opened for another
// battery's reads. Every command runs in the clone, never in a town worktree
// or the rig's repository.
type Clone struct {
	g *bareGit
}

// OpenClone opens the bare clone for the rig at townRoot. The clone is not
// created until Sync runs.
func OpenClone(townRoot string) (*Clone, error) {
	origin, err := rigOriginURL(townRoot)
	if err != nil {
		return nil, err
	}
	return &Clone{g: newBareGit(ClonePath(townRoot), origin)}, nil
}

// Sync creates the clone when it is missing and fetches its branches, so a
// read after it sees the rig's current heads.
func (c *Clone) Sync(ctx context.Context) error {
	return c.g.Sync(ctx)
}

// Run runs git with args in the clone and returns its standard output. The
// caller checks any ref it passes; a ref that starts with "-" must not reach
// git.
func (c *Clone) Run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.g.timeout)
	defer cancel()
	return c.g.run(ctx, c.g.dir, args...)
}
