package convoy

import (
	"context"
	"strings"
	"unicode"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/util"
)

// A dispatch hold is a decision recorded on a bead's own record that takes it
// off the generic convoy dispatch path, so the feeder's default-agent sling
// does not override it. Both feeders share this rule: the daemon's stranded
// scan and the event-driven continuation feed.
//
// These markers are machine-read, so before writing one read
// docs/concepts/convoy.md ("Dispatch holds"), which states the write form for
// each and how to release a hold.

// dispatchHoldLabels are the routing decisions recorded as labels: needs-pro
// wants a specific runtime, needs-mayor-review wants the mayor's eyes before
// any work starts. Matched case-insensitively, since a label is typed by hand.
// The operator reservation (dispatch.OperatorReservation) is the third decision
// a label records, and the one that also reaches through the assignee; it is
// applied in DispatchHoldFields rather than listed here so gt sling reads the
// same rule before it spends a polecat seat.
var dispatchHoldLabels = []string{"needs-pro", "needs-mayor-review"}

// dispatchHoldStatuses are the statuses beads calls CategoryFrozen, "excluded
// from bd ready": a convoy that fed one would dispatch work the tracker says is
// not ready. The category test itself is not exported by the SDK, and neither
// is a pinned constant, hence the literal.
var dispatchHoldStatuses = []beadsdk.Status{beadsdk.StatusDeferred, "pinned"}

// dispatchHoldProse are the keep-off decisions recorded in a bead's prose,
// listed as they are written so the reason can quote them back.
var dispatchHoldProse = []string{"MAYOR DESIGN DECISION", "do not redispatch"}

// dispatchHoldRelease clears a decision recorded in a comment, the one field
// that cannot be edited or withdrawn: beads comments are append-only, so a hold
// written as a comment needs a later comment to lift it.
var dispatchHoldRelease = "HOLD RELEASED"

// DispatchHoldReason reports why issueID's record keeps it off the generic
// convoy dispatch path, or "" when it may be dispatched; a record that cannot
// be read reports a reason too, so an unreadable bead is never mistaken for an
// unheld one (gt-tq6l).
//
// This is the rule every automatic dispatcher shares. The convoy feeders apply
// FeedHold, which also reports a merge rejection on record.
func DispatchHoldReason(ctx context.Context, store beadsdk.Storage, issueID string, resolver *StoreResolver) string {
	return readHold(ctx, store, issueID, resolver).Reason
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
func FeedHold(ctx context.Context, store beadsdk.Storage, issueID string, resolver *StoreResolver) Hold {
	return readHold(ctx, store, issueID, resolver)
}

// readHold applies the hold rule to issueID's record and notes whether it
// carries a merge rejection.
func readHold(ctx context.Context, store beadsdk.Storage, issueID string, resolver *StoreResolver) Hold {
	// store is the town store the caller already holds; the resolver redirects
	// a rig bead to its own store, which is where its record lives.
	owner := store
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

	issue, err := owner.GetIssue(ctx, issueID)
	if err != nil {
		return Hold{Reason: "record unreadable (" + util.FirstLine(err.Error()) + ")", Unreadable: true}
	}
	if issue == nil {
		return Hold{Reason: "no record for " + issueID, Unreadable: true}
	}

	rejected := strings.Contains(issue.Notes, dispatch.MergeRejectionNoteMarker)

	// The fields decide most holds; only a record that gets past them pays for
	// its comment history.
	if reason := dispatchHoldInFields(issue); reason != "" {
		return Hold{Reason: reason, MergeRejection: rejected}
	}

	comments, err := owner.GetIssueComments(ctx, issueID)
	if err != nil {
		return Hold{Reason: "comments unreadable (" + util.FirstLine(err.Error()) + ")", Unreadable: true}
	}
	return Hold{Reason: dispatchHoldInComments(comments), MergeRejection: rejected}
}

// dispatchHoldInFields applies the hold rule to the fields GetIssue returns.
func dispatchHoldInFields(issue *beadsdk.Issue) string {
	return DispatchHoldFields(string(issue.Status), issue.Labels, issue.Assignee, issue.Design, issue.Notes)
}

// DispatchHoldFields reports the hold a bead's own fields assert, or "" when
// they assert none.
//
// It is the field half of readHold, exported so a caller that has a bead's
// fields but no beadsdk.Storage reaches the same verdict instead of a second
// copy of the rule — the witness reads a polecat's hooked bead as JSON to
// decide whether a restart may raise it (gt-n38c6). Comments are not part of
// this: bd show --json omits them, which makes such a caller narrower than
// readHold and never wider. status is a plain string so that JSON-reading
// caller needs no SDK type; dispatchHoldInFields does the one conversion.
func DispatchHoldFields(status string, labels []string, assignee, design, notes string) string {
	for _, held := range dispatchHoldStatuses {
		if status == string(held) {
			return "status " + status
		}
	}
	// The operator reservation is checked before the routing labels: a bead the
	// operator owns is not the town's to route anywhere, and its reason is the
	// one an operator reading the log needs named (gt-21pl0).
	if reason := dispatch.OperatorReservation(labels, assignee); reason != "" {
		return reason
	}
	for _, label := range labels {
		for _, held := range dispatchHoldLabels {
			if strings.EqualFold(label, held) {
				return "label " + label
			}
		}
	}
	if decision := holdDecisionIn(design); decision != "" {
		return decision + " in design"
	}
	if decision := holdDecisionIn(notes); decision != "" {
		return decision + " in notes"
	}
	return ""
}

// dispatchHoldInComments applies the comment half of the rule. Comments arrive
// oldest first, and the newest decision is the live one: a later release lifts
// an earlier hold, which is what makes a comment-recorded hold releasable
// (gt-tq6l).
func dispatchHoldInComments(comments []*beadsdk.Comment) string {
	held := ""
	for _, comment := range comments {
		if comment == nil {
			continue
		}
		if decisionOnLine(comment.Text, []string{dispatchHoldRelease}) != "" {
			held = ""
			continue
		}
		if decision := holdDecisionIn(comment.Text); decision != "" {
			held = decision + " in comment"
		}
	}
	return held
}

// holdDecisionIn returns the keep-off decision text asserts, or "". The
// decision has to be asserted, not merely mentioned, so a note that quotes the
// wording back — as this feature's own beads do — is not held by it (gt-tq6l).
func holdDecisionIn(text string) string {
	return decisionOnLine(text, dispatchHoldProse)
}

// decisionOnLine returns the first marker of markers that begins a line of
// text, past any list, heading, or quote decoration, or "".
func decisionOnLine(text string, markers []string) string {
	for _, line := range strings.Split(text, "\n") {
		folded := foldDecisionText(stripLineDecoration(line))
		for _, marker := range markers {
			if strings.HasPrefix(folded, foldDecisionText(marker)) {
				return marker
			}
		}
	}
	return ""
}

// stripLineDecoration drops the leading decoration a decision may be written
// behind: "- do not redispatch", "> **HOLD RELEASED**", "## MAYOR DESIGN
// DECISION".
func stripLineDecoration(line string) string {
	return strings.TrimLeft(strings.TrimSpace(line), "#*->+` \t")
}

// foldDecisionText drops case, hyphens, underscores, and whitespace, so a
// decision matches however it was typed: "do-not-redispatch" and
// "do not re-dispatch" fold onto the same phrase.
func foldDecisionText(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.ToLower(text))
}
