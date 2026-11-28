package landworker

import (
	"slices"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// nextGateAhead names the bead the worker may gate ahead while front lands:
// the bead immediately after front in landing order, when the ready board
// holds at least minReady beads counting front, reviewWorks would offer that
// bead, and its recorded submitter passes actorRefusal (gt-xlmcx2).
//
// Only that one bead is considered: a bead held, parked or refused just ahead
// is a reason to gate nothing, never to gate a later bead, whose gate would be
// built on a tree missing the bead due before it.
//
// It reads the board and writes nothing; a list, review or submission record
// it cannot read answers false, since gating nothing is always safe.
func (w *Worker) nextGateAhead(front land.Work, minReady int) (land.Work, bool) {
	if front.BeadID == "" {
		return land.Work{}, false
	}
	issues, err := w.Beads.List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1})
	if err != nil || len(issues) < minReady {
		return land.Work{}, false
	}
	sortByLandingOrder(issues)
	at := slices.IndexFunc(issues, func(is *beads.Issue) bool { return is != nil && is.ID == front.BeadID })
	if at < 0 || at+1 >= len(issues) || issues[at+1] == nil {
		return land.Work{}, false
	}
	next := issues[at+1]
	works, err := w.reviewWorks([]*beads.Issue{next}, "")
	if err != nil || len(works) != 1 {
		return land.Work{}, false
	}
	actor, err := w.submissionActor(next, works[0])
	if err != nil {
		return land.Work{}, false
	}
	if key, _ := w.actorRefusal(next.ID, actor); key != "" {
		return land.Work{}, false
	}
	return works[0], true
}
