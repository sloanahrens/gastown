package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/metrics"
)

// archiveRigNameRE is the archive directory name the rig may be: a database
// name, which is alphanumeric with underscores and hyphens (see the daemon's
// validDBName). It is refused otherwise before it reaches a path or a git path
// spec, so a name like "../x" can never read outside the archive.
var archiveRigNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// archiveCommitRE is what git log's commit id may be before it is spliced into
// a git path spec. git prints 40 lowercase hex digits; anything else is not a
// commit and never reaches argv, where a leading dash would read as a flag.
var archiveCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// archiveCommit is one commit that touched the archive's issues.jsonl: its id
// and when it was committed, in the committer's own zone.
type archiveCommit struct {
	SHA string
	At  time.Time
}

// archiveSampledCommits lists the commits that touched the rig's issues.jsonl
// and keeps one per local day, oldest first. The shape-outcome join and the
// first-filed import share it, so their sampling cannot drift apart; each
// caller wraps an error with its own command name.
//
// git log prints newest first, so the first commit seen for a day is that
// day's last export: the most complete version of the day, and the latest
// point at which a bead filed that day could already have been edited. The
// list is reversed before it is returned, because FirstFiledVersions reads
// successive commits oldest first.
func archiveSampledCommits(ctx context.Context, run metrics.Runner, repo, rig string, dropped *metrics.Dropped) ([]archiveCommit, error) {
	// The committer date travels beside the id in one call: pairing a second
	// log of dates with this one by position would be a second ordering to get
	// wrong.
	raw, err := run(ctx, []string{
		"git", "-C", repo, "log", "--first-parent", "--format=%H %cI", "--", rig + "/issues.jsonl",
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(repo, rig, "issues.jsonl"), err)
	}
	var sampled []archiveCommit
	seenDay := make(map[string]bool)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !archiveCommitRE.MatchString(fields[0]) {
			dropped.Add("git log", "line is not a commit id and a date")
			continue
		}
		at, err := time.Parse(time.RFC3339, fields[1])
		if err != nil {
			dropped.Add("git log", "unparseable committer date")
			continue
		}
		day := at.Format(time.DateOnly)
		if seenDay[day] {
			continue
		}
		seenDay[day] = true
		sampled = append(sampled, archiveCommit{SHA: fields[0], At: at})
	}
	for i, j := 0, len(sampled)-1; i < j; i, j = i+1, j-1 {
		sampled[i], sampled[j] = sampled[j], sampled[i]
	}
	return sampled, nil
}

// archiveFirstFiled reads each sampled commit's issues.jsonl and returns the
// first row the archive holds per bead. A commit whose file cannot be read
// fails the run: a silently skipped day is a bead read from a later edit
// without any sign of it.
func archiveFirstFiled(ctx context.Context, run metrics.Runner, repo, rig string, commits []archiveCommit, dropped *metrics.Dropped) ([]metrics.FirstFiled, error) {
	batches := make([]metrics.ArchiveCommit, 0, len(commits))
	for _, c := range commits {
		data, err := run(ctx, []string{
			"git", "-C", repo, "show", c.SHA + ":" + rig + "/issues.jsonl",
		})
		if err != nil {
			return nil, fmt.Errorf("reading %s/issues.jsonl at %s: %w", rig, c.SHA, err)
		}
		batches = append(batches, metrics.ArchiveCommit{
			At:   c.At,
			Rows: metrics.DecodeArchiveIssues(data, c.SHA, dropped),
		})
	}
	return metrics.FirstFiledVersions(batches), nil
}
