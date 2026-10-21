package done

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

// This file is gt done's half of the re-queue path (gt-3e1z4). A work bead Land
// rejected comes back to dispatch labeled rework with a MERGE REJECTION note
// naming the head it refused. If its branch still sits on that head, a
// resubmission hands the gate the identical commit: the gate re-runs, reaches
// the same verdict, and the polecat is no closer to landing.
//
// gt done stands down instead of spending that run. The rejection may have had
// nothing to do with the diff (a runner with no init process left zombies; a
// target that moved), and then the sanctioned move is the operator re-queueing
// the unchanged head with gt land requeue. Otherwise the author has to change
// the content. Either way the run falls through to the close path, which
// records this message on the bead instead of a bare refusal (gt-6hmz's
// close-time invariant is unchanged: the bead stays open).

// ReworkUnchangedHeadMessage returns the message gt done prints and records on
// a bead that is in the landing-rework state and still sits on the head its
// rejection names. ok is false when any leg does not hold: the bead is not in
// rework, its live rejection names no head or a different one, or the rejection
// is one the diff did not cause (rejectionKindsNotCausedByDiff) — for those a
// resubmission that rebases is the ordinary, legitimate move, and standing down
// would strand a conflict the author can settle.
func ReworkUnchangedHeadMessage(issue *beads.Issue, head string) (string, bool) {
	if issue == nil || strings.TrimSpace(head) == "" || !beads.HasLabel(issue, land.LabelRework) {
		return "", false
	}
	note, ok := land.ParseRejectionNote(issue.Notes)
	if !ok {
		return "", false
	}
	rejected := strings.TrimSpace(note.Head)
	if rejected == "" || !land.HeadsEqual(rejected, head) {
		return "", false
	}
	if rejectionKindsNotCausedByDiff[strings.ToLower(strings.TrimSpace(note.Kind))] {
		return "", false
	}
	return reworkUnchangedHeadRefusal(issue.ID, rejected), true
}

// reworkUnchangedHeadRefusal is the actionable message: what is unchanged, that
// nothing was resubmitted, and the two ways forward — an operator re-queuing the
// unchanged head, or a new commit. It names no bypass flag: an agent that reads
// a bypass reaches for it, and the point is the operator's decision.
func reworkUnchangedHeadRefusal(issueID, head string) string {
	var b strings.Builder
	b.WriteString("head unchanged since the rejection — nothing was resubmitted\n\n")
	fmt.Fprintf(&b, "HEAD is %s, the commit the landing attempt already submitted and the\n", ShortSHA(head))
	b.WriteString("refinery refused. Submitting it again would send the gate the same commit\n")
	b.WriteString("to reach the same verdict, so gt done did not resubmit it.\n\n")
	if issueID != "" {
		b.WriteString("If the rejection was environmental or unrelated to this change, an operator\n")
		b.WriteString("can re-queue the unchanged head with:\n")
		fmt.Fprintf(&b, "  gt land requeue %s --reason \"<why the rejection did not come from the diff>\"\n\n", issueID)
	}
	b.WriteString("Otherwise a new commit is needed: change the content so it answers the\n")
	b.WriteString("findings, then re-run gt done.")
	return b.String()
}

// reworkPush is the rejected-landing context a push needs to tell a rework from
// divergence. The zero value means the bead is not in rework, or its live
// rejection names no head, and recoverDivergedPush keeps its plain rules.
type reworkPush struct {
	// rejectedHead is the head the live MERGE REJECTION note names: the commit
	// the landing worker refused.
	rejectedHead string
	// seatAuthor and seatEmail are the git identity this seat's commits carry:
	// the agent name the session sets as GIT_AUTHOR_NAME, and the worktree's
	// user.email. Both must match, so a commit under the same name from another
	// mailbox is not read as this seat's. Git author fields are self-asserted,
	// so this rules out accidents rather than a deliberate forgery; the lease
	// pinned to the rejected head is what limits the overwrite.
	seatAuthor string
	seatEmail  string
}

// reworkPushFor reads the rework context off a work bead. It is empty unless
// the bead carries the rework label and its rejection names a head, and it is
// empty for a seat with no author name or email, because nothing can then be
// matched as this seat's own.
func reworkPushFor(issue *beads.Issue, seatAuthor, seatEmail string) reworkPush {
	seatAuthor, seatEmail = strings.TrimSpace(seatAuthor), strings.TrimSpace(seatEmail)
	if issue == nil || seatAuthor == "" || seatEmail == "" || !beads.HasLabel(issue, land.LabelRework) {
		return reworkPush{}
	}
	head, ok := land.RejectedHead(issue.Notes)
	if !ok {
		return reworkPush{}
	}
	return reworkPush{rejectedHead: head, seatAuthor: seatAuthor, seatEmail: seatEmail}
}

// allows reports whether origin's tip may be overwritten under the lease: the
// tip is the rejected head, and every commit in origin's range is this seat's
// by both name and email. origin must list the full range, merged-in commits
// included; a first-parent listing would miss another author's commit on a
// merge's second parent. Origin's range must hold at least one commit, so an
// empty range is never read as "all of it is this seat's".
func (r reworkPush) allows(originSHA string, origin []git.CommitSummary) bool {
	if r.rejectedHead == "" || r.seatAuthor == "" || r.seatEmail == "" || !land.HeadsEqual(originSHA, r.rejectedHead) {
		return false
	}
	if len(origin) == 0 {
		return false
	}
	for _, c := range origin {
		if c.Author != r.seatAuthor || c.Email != r.seatEmail {
			return false
		}
	}
	return true
}
