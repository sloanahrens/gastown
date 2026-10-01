package steward

import (
	"fmt"
	"strings"
)

// PromptFor is the instruction one job runs: what the event is, what the job
// may do, and the verdict file it must leave behind. gt-9bioi.2 owns the
// definitive wording of both jobs; this is the runner's contract with them,
// and the contract the tests hold: whatever the text becomes, the job writes
// ResultFile and acts only on a polecat's own submitted branch (gt-9bioi).
func PromptFor(ev Event) string {
	var b strings.Builder
	if ev.Kind == KindReview {
		fmt.Fprintf(&b, "A polecat submitted work for landing and no steward job has reviewed head %s yet.\n", shortHead(ev.Head))
	} else {
		fmt.Fprintf(&b, "The landing worker rejected this work (attempt %d, head %s): %s\n", ev.Attempt, shortHead(ev.Head), ev.RejectionDetail)
	}
	fmt.Fprintf(&b, "\nBead: %s\nRig: %s\nBranch: %s\nHead: %s\nTarget: %s\n",
		ev.Bead, ev.Rig, ev.Branch, ev.Head, ev.Target)
	if ev.Worker != "" {
		fmt.Fprintf(&b, "Author: %s\n", ev.Worker)
	}
	fmt.Fprintf(&b, `
You are running in a throwaway worktree of the rig repository, at HEAD %s
(detached: the submitted commit, which the branch's author pushed). The bead's
notes hold the READY TO LAND block and every MERGE REJECTION block; read them
with "gt show %s" or "bd show %s" before deciding.

Publishing a fix means pushing this commit to the branch the work came from:

  git push origin HEAD:refs/heads/%s

Do only what the event needs, and only on that branch: never commit to main,
never merge anything yourself, and never touch another bead's branch. If a
decision is not yours to make, escalate to the operator and record that as
your outcome rather than guessing.

When you are done, write %s in your working directory:

  {"outcome": "<one of %s>", "summary": "<one line saying what you did>"}

The outcome is the only thing the runner records, and a job that writes no
verdict is recorded as an error.`, shortHead(ev.Head), ev.Bead, ev.Bead, ev.Branch, ResultFile, outcomeList())
	return b.String()
}
