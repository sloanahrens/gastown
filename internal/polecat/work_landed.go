package polecat

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// SeatBeadID is the bead a seat's leftover commits belong to: its hook while it
// has one, and the bead its branch name encodes once the hook is cleared.
func SeatBeadID(hookBead, branch string) string {
	if id := strings.TrimSpace(hookBead); id != "" {
		return id
	}
	if meta, ok := ParseBranchName(branch); ok {
		return meta.Issue
	}
	return ""
}

// ApplyWorkLanded records on facts whether the commits the worktree at g holds
// are already landed (gt-jol0i). It is the one place a seat's bead is chosen
// and read and land.WorkLandedOnTarget asked, shared by check-recovery, gt
// polecat list and the reuse gate, so the three cannot disagree about what
// "landed" means.
//
// A seat with no leftover commits has nothing landing evidence could clear, and
// the probe costs a bead read per seat, so it is skipped. store may be nil: the
// refs/landed ancestry arm answers from git alone. rigPath is the seat's rig
// root, whose config names the branch its landings target.
func ApplyWorkLanded(facts *WorkstateFacts, g land.WorkLandedRepo, store IssueReader, rigPath, hookBead, branch string) {
	if g == nil || facts.UnpushedCommits == 0 {
		return
	}
	beadID := SeatBeadID(hookBead, branch)
	if beadID == "" {
		return
	}
	var issue *beads.Issue
	var readErr error
	if store != nil {
		issue, readErr = store.Show(beadID)
	}
	v := land.WorkLandedOnTarget(g, "", "", rigPath, beadID, issue)
	if !v.Landed && v.Reason == land.WorkUnpushed && readErr != nil {
		// The LANDING RECORD arm could not run, so "nothing proves it" would
		// overstate what was checked.
		v = land.WorkLandedVerdict{Reason: land.WorkProbeFailed, Detail: fmt.Sprintf("read bead %s: %v", beadID, readErr)}
	}
	facts.WorkLanded = v.Landed
	facts.WorkLandedReason = v.Reason
	facts.WorkLandedDetail = v.Detail
}
