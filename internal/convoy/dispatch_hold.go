package convoy

import (
	"context"
	"errors"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/util"
)

// DispatchHoldReason reports why issueID's record keeps it off the generic
// convoy dispatch path, or "" when it may be dispatched; a record that cannot
// be read reports a reason too, so an unreadable bead is never mistaken for an
// unheld one (gt-tq6l).
//
// This is the rule every automatic dispatcher shares. The convoy feeders apply
// FeedHold, which also reports a merge rejection on record. The markers are in
// dispatch.DispatchHoldFields; before writing or releasing one read
// docs/concepts/convoy.md ("Dispatch holds").
func DispatchHoldReason(ctx context.Context, source IssueSource, issueID string, resolver *StoreResolver) string {
	return readHold(source, issueID, resolver).Reason
}

// Hold is a convoy feeder's verdict on one bead's record. The zero value is
// "may be fed".
type Hold struct {
	// Reason says why the bead is held, for the feeder's log; "" when it is not.
	Reason string

	// MergeRejection is set when the landing worker's merge-rejection marker
	// is on record: the bead was rejected and reopened as rework. It is not a
	// hold: a rejected bead is ready work, and its preserved branch is the
	// work to redo, not work to protect (gt-et7ho).
	MergeRejection bool

	// Unreadable is set when the record could not be read. The bead is held
	// because a rejection, like any other hold, cannot be ruled out.
	Unreadable bool
}

// FeedHold is the hold rule for the convoy feeders — the daemon's stranded
// scan and the event-driven continuation feed. It is DispatchHoldReason plus
// whether the record carries a merge rejection. Like DispatchHoldReason it
// fails closed on a record it cannot read, and reports no hold when there is
// no store to read from at all — a record whose rig has no store open
// included, which the town store would otherwise answer "no record" for
// (gt-2ppfg).
func FeedHold(ctx context.Context, source IssueSource, issueID string, resolver *StoreResolver) Hold {
	return readHold(source, issueID, resolver)
}

// readHold applies the hold rule to issueID's record and notes whether it
// carries a merge rejection.
func readHold(source IssueSource, issueID string, resolver *StoreResolver) Hold {
	// source is the town store the caller already holds; the resolver redirects
	// a rig bead to its own store, which is where its record lives.
	owner := source
	if resolver != nil {
		resolved, gap := resolver.owningStoreOrGap(issueID)
		if gap {
			// The rig that holds this bead's record has no store open, so the
			// caller's town store would answer "no record" about a bead that
			// has one. That is a gap in the store, not an answer from it, and
			// the town-level store alert already owns it — holding the bead
			// for it would stall every convoy feeding a rig that is down
			// (gt-2ppfg).
			return Hold{}
		}
		if resolved != nil {
			owner = resolved
		}
	}
	if owner == nil {
		// Nothing to read from at all is the town-level gap the store alert
		// reports, not a record that failed to answer.
		return Hold{}
	}

	issue, err := owner.Show(issueID)
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return Hold{Reason: "record unreadable (" + util.FirstLine(err.Error()) + ")", Unreadable: true}
	}
	if issue == nil {
		return Hold{Reason: "no record for " + issueID, Unreadable: true}
	}

	rejected := strings.Contains(issue.Notes, dispatch.MergeRejectionNoteMarker)

	// The fields decide most holds; only a record that gets past them pays for
	// its comment history.
	if reason := dispatch.DispatchHoldFields(issue.Status, issue.Labels, issue.Assignee, issue.Design, issue.Notes); reason != "" {
		return Hold{Reason: reason, MergeRejection: rejected}
	}

	comments, err := owner.Comments(issueID)
	if err != nil {
		return Hold{Reason: "comments unreadable (" + util.FirstLine(err.Error()) + ")", Unreadable: true}
	}
	return Hold{Reason: dispatch.HoldInComments(comments), MergeRejection: rejected}
}
