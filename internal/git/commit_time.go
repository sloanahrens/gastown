package git

import (
	"fmt"
	"strings"
	"time"
)

// CommitTime returns the committer time of rev (git show -s --format=%cI),
// in the zone git recorded.
func (g *Git) CommitTime(rev string) (time.Time, error) {
	out, err := g.run("show", "-s", "--format=%cI", rev)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(out))
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing committer time of %s: %w", rev, err)
	}
	return t, nil
}
