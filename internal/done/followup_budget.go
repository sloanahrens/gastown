package done

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
)

// followupBudgetCheck reads the branch's numstat diff over the merge base —
// `git diff --numstat <baseRef>...HEAD`, rename detection off so a rename
// counts both paths — and refuses a follow-up bead over the rig's budget
// (gt-c9cp26); a bead without land.FollowUpLabel never runs the diff, and a
// failed read refuses naming it. A binary file's "-" adds 0, still a file.
func followupBudgetCheck(g *git.Git, issue *beads.Issue, baseRef string, cfg *rig.RigConfig) error {
	if issue == nil || !beads.HasLabel(issue, land.FollowUpLabel) {
		return nil
	}
	base, err := g.MergeBase(baseRef, "HEAD")
	if err != nil {
		return fmt.Errorf("cannot check the follow-up bead's diff budget: finding the merge base of %s and HEAD: %w", baseRef, err)
	}
	out, err := g.DiffNumstat(base, "HEAD")
	if err != nil {
		return fmt.Errorf("cannot check the follow-up bead's diff budget: git diff --numstat: %w", err)
	}
	paths, err := numstatPaths(out)
	if err != nil {
		return fmt.Errorf("cannot check the follow-up bead's diff budget: %w", err)
	}
	added, files, err := git.NumstatSize(out)
	if err != nil {
		return fmt.Errorf("cannot check the follow-up bead's diff budget: %w", err)
	}
	maxAdded, maxFiles := land.DefaultFollowupMaxAddedLines, land.DefaultFollowupMaxFiles
	if cfg != nil {
		maxAdded, maxFiles = cfg.MergeQueue.GetFollowupMaxAddedLines(maxAdded), cfg.MergeQueue.GetFollowupMaxFiles(maxFiles)
	}
	return followupBudgetError(issue, added, files, paths, maxAdded, maxFiles)
}

// numstatPaths returns the path column of `git diff --numstat` output, one
// per line in the order git printed them. A line that is not three
// tab-separated fields is an error, so a path is never left out of the list
// the refusal names.
func numstatPaths(out string) ([]string, error) {
	var paths []string
	for i, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("parsing git diff --numstat line %d for its path: %d tab-separated fields, want 3", i+1, len(fields))
		}
		paths = append(paths, fields[2])
	}
	return paths, nil
}

// followupBudgetError refuses a follow-up bead's submission whose diff is over
// the follow-up budget: a nit filed by the post-landing feed stays a nit
// instead of growing into a rework (gt-3der6d). The refusal names each path
// the change touches, because the overseer splits an over-budget bead by file
// and there is no waiver. A nil issue, a bead without land.FollowUpLabel, and
// a diff within both limits are not over budget.
func followupBudgetError(issue *beads.Issue, added, files int, paths []string, maxAdded, maxFiles int) error {
	if issue == nil || !beads.HasLabel(issue, land.FollowUpLabel) {
		return nil
	}
	if added <= maxAdded && files <= maxFiles {
		return nil
	}
	var touched strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&touched, "  %s\n", p)
	}
	return fmt.Errorf("refusing to submit: this change adds %d lines in %d file(s), "+
		"over the diff budget of a follow-up bead (%d added lines, %d files)\n\n"+
		"Files this change touches:\n%s\n"+
		"A follow-up bead fixes one finding from a passing review, and a change past "+
		"its budget is a rework the finding did not ask for. Escalate the finding you "+
		"could not fix in budget instead of expanding the change:\n"+
		"  gt done --status ESCALATED\n"+
		"Put that file list and the finding in the bead's notes. The overseer splits "+
		"the bead by file; there is no waiver, so this change is not submitted whole.",
		added, files, maxAdded, maxFiles, touched.String())
}
