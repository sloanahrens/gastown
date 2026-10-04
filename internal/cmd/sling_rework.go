package cmd

import (
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// A rejected bead goes back to the polecat that built its branch (gt-lid6d).
//
// The landing worker swaps gt:ready-to-land for rework and hands the bead
// back open and unassigned. Today's dispatcher then slings it to whichever
// seat is free, which starts that seat on a fresh branch and leaves the
// author's branch — and, for a while, the author's seat — as orphaned
// recovery work. When the bead names its author and branch, the sling
// prefers that seat and resumes that branch, so the context and the
// half-reviewed commits stay together.
//
// The preference is soft. It is only a hint about which seat to try first:
// anything that makes the seat unavailable falls back to the pool, so a
// rework never waits on a specific polecat.

// reworkSeat is the polecat a rework bead came back from, and the branch its
// rejected attempt is on.
type reworkSeat struct {
	PolecatName string
	Branch      string
}

// submittedBranchRE matches the comment gt done writes on submission:
// "Submitted for landing: <branch> @ <sha> onto <target> (attempt N)". The
// shape is internal/landworker's submittedRE; the branch is all this reads.
var submittedBranchRE = regexp.MustCompile(`Submitted for landing: (\S+) @ [0-9a-fA-F]{4,40}`)

// reworkSeatFor reads the polecat and branch a rework bead goes back to. It
// answers ok=false for a bead that is not rework, that names no submission,
// or whose submission names a branch no polecat can be read out of — every
// one of which dispatches exactly as it did before.
func reworkSeatFor(townRoot, beadID string) (*reworkSeat, bool) {
	if beadID == "" {
		return nil, false
	}
	store := slingStores{}
	issue, err := store.show(townRoot, beadID)
	if err != nil || issue == nil {
		return nil, false
	}
	comments, err := store.comments(townRoot, beadID)
	if err != nil {
		comments = nil
	}
	return reworkSeatFrom(issue, comments)
}

// reworkSeatFrom is reworkSeatFor's decision over the bead and its comments,
// separated from the reads so it is testable without a store.
func reworkSeatFrom(issue *beads.Issue, comments []beads.Comment) (*reworkSeat, bool) {
	if issue == nil || !beads.HasLabel(issue, land.LabelRework) {
		return nil, false
	}
	if branch := lastSubmittedBranch(comments); branch != "" {
		if name, ok := polecatFromBranch(branch); ok {
			return &reworkSeat{PolecatName: name, Branch: branch}, true
		}
	}
	// Fall back to the READY TO LAND block, the other place a submission
	// names what to land. ParseReadyNote reads the last block, which is the
	// live request when a rework resubmitted.
	if w, ok := land.ParseReadyNote(issue.Notes); ok {
		if name, ok := polecatFromBranch(w.Branch); ok {
			return &reworkSeat{PolecatName: name, Branch: w.Branch}, true
		}
	}
	return nil, false
}

// lastSubmittedBranch is the branch named by the newest "Submitted for
// landing" comment. A bead resubmitted after a rework carries an older
// comment naming the branch that was rejected and a newer one naming the
// branch that came back, so the newest is the one to read.
func lastSubmittedBranch(comments []beads.Comment) string {
	for i := len(comments) - 1; i >= 0; i-- {
		if m := submittedBranchRE.FindStringSubmatch(comments[i].Text); m != nil {
			return m[1]
		}
	}
	return ""
}

// polecatFromBranch reads the polecat name out of a polecat branch
// (polecat/<name>/<bead>+<nonce>). Any other branch shape names no polecat,
// and a submission on one dispatches as it did before.
func polecatFromBranch(branch string) (string, bool) {
	parts := strings.Split(branch, "/")
	if len(parts) < 3 || parts[0] != "polecat" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}
