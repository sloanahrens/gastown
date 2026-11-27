package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/metrics"
)

// archiveIssuesAt reads a rig's whole issues export at one archive commit: the
// shards archiveIssueShards picks from its tree, joined in shard order, each
// ending on a line. A listing with no issues file, an unreadable shard and an
// unusable rig or commit return no bytes, so a partial export never reads whole.
func archiveIssuesAt(ctx context.Context, run metrics.Runner, repo, rig, sha string) ([]byte, error) {
	// Both become git argv; archiveRigNameRE allows a leading dash, which git reads as a flag.
	if !archiveRigNameRE.MatchString(rig) || strings.HasPrefix(rig, "-") {
		return nil, fmt.Errorf("archive issues: rig %q is not a database name", rig)
	}
	if !archiveCommitRE.MatchString(sha) {
		return nil, fmt.Errorf("archive issues: %q is not a commit id", sha)
	}
	listing, err := run(ctx, []string{"git", "-C", repo, "ls-tree", "--name-only", sha, rig + "/"})
	if err != nil {
		return nil, fmt.Errorf("listing %s at %s: %w", rig, sha, err)
	}
	shards := archiveIssueShards(string(listing))
	if len(shards) == 0 {
		return nil, fmt.Errorf("%s at %s holds no issues file", rig, sha)
	}
	var all []byte
	for _, name := range shards {
		data, err := run(ctx, []string{"git", "-C", repo, "show", sha + ":" + name})
		if err != nil {
			return nil, fmt.Errorf("reading %s at %s: %w", name, sha, err)
		}
		all = append(all, data...)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			all = append(all, '\n') // every shard ends on its own line
		}
	}
	return all, nil
}
