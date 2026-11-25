package daemon

import (
	"math"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// pathTouchedMark is how many commits the feed marks a replayed finding with:
// enough to see that the file moved under it, few enough for one bead body
// line (gt-vn8dhf).
const pathTouchedMark = 5

// pathTouchedWalk is the commit count the range read asks git for. The feed
// needs the answer for the whole range — a file touched by a commit older than
// a smaller bound would be reported unchanged — so the read is told to walk
// all of it; the range runs from a queued landing to the base this landing
// builds on, which is short.
const pathTouchedWalk = math.MaxInt32

// pathTouchedGit is the read PathTouched drives: the rig repository handle the
// landing worker's other reads use (landingRemoteGitAt), narrowed to the one
// method the mark needs.
type pathTouchedGit interface {
	CommitFileChanges(rev string, limit int) ([]git.CommitFileChange, error)
}

// pathTouchedHook is the Lander.PathTouched the daemon wires for one rig: the
// short commits that touched a finding's file in landed..base, newest first,
// at most pathTouchedMark. The feed asks it about a finding replayed from the
// queue, so its file changing since the landing can be named in the filed body
// (gt-vn8dhf). It runs in the feed path, so it reads the range once; any git
// failure answers nil, which leaves the finding unmarked rather than failing
// the landing (gt-h80mxo).
func (d *Daemon) pathTouchedHook(repo string) func(landed, base, path string) []string {
	g := d.landingRemoteGitAt(repo)
	return func(landed, base, path string) []string {
		return pathTouchedCommits(g, landed, base, path)
	}
}

// pathTouchedCommits reads the range once and keeps the commits whose change
// list names path. CommitFileChanges lists the range newest first, so the first
// pathTouchedMark matches are the newest ones; a merge commit contributes no
// change to that read, so a touch that exists only as a merge's own conflict
// resolution is not reported. An empty range or path, an end that would read
// as a git option, or a git error answers nil.
func pathTouchedCommits(g pathTouchedGit, landed, base, path string) []string {
	if landed == "" || base == "" || strings.HasPrefix(landed, "-") || strings.HasPrefix(base, "-") || strings.TrimSpace(path) == "" {
		return nil
	}
	changes, err := g.CommitFileChanges(landed+".."+base, pathTouchedWalk)
	if err != nil {
		return nil
	}
	var touched []string
	for _, change := range changes {
		if change.Path != path {
			continue
		}
		touched = append(touched, shortSHA(change.Commit))
		if len(touched) == pathTouchedMark {
			break
		}
	}
	return touched
}
