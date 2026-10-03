package dispatch

import (
	"strings"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
)

// A dispatch hold is a decision recorded on a bead's own record that takes it
// off the generic dispatch path, so an automatic dispatcher's default sling
// does not override it. The daemon's patrol_scan restart reads it through
// DispatchHoldFields.
//
// These markers are machine-read, so write one exactly as the tables below
// list it: dispatchHoldLabels, dispatchHoldStatuses and dispatchHoldProse are
// the write form each dispatcher reads back, and a field the read cannot
// parse holds the bead rather than releasing it.

// dispatchHoldLabels are the routing decisions recorded as labels: needs-pro
// wants a specific runtime, needs-mayor-review wants the mayor's eyes before
// any work starts, and gt:needs-human (needs-human by hand) is a landing the
// worker left for a person (om gave no verdict, a stage timed out, a policy
// refusal, or land.MaxReworkAttempts rejections): unlike rework, no polecat
// can settle it (gt-hpca9, gt-28ibg). Matched case-insensitively, since a
// label is typed by hand.
// The operator reservation (OperatorReservation) is the third decision a label
// records, and the one that also reaches through the assignee; it is applied
// in DispatchHoldFields rather than listed here so a caller reads the same
// rule before it spends a polecat seat.
var dispatchHoldLabels = []string{"needs-pro", "needs-mayor-review", "gt:needs-human", "needs-human"}

// dispatchHoldStatuses are the statuses beads calls CategoryFrozen, "excluded
// from bd ready": a dispatcher that fed one would take on work the tracker
// says is not ready. The category test itself is not exported by the SDK, and
// neither is a pinned constant, hence the literals.
var dispatchHoldStatuses = []string{"deferred", "pinned"}

// dispatchHoldProse are the keep-off decisions recorded in a bead's prose,
// listed as they are written so the reason can quote them back.
var dispatchHoldProse = []string{"MAYOR DESIGN DECISION", "do not redispatch"}

// dispatchHoldRelease clears a decision recorded in a comment, the one field
// that cannot be edited or withdrawn: beads comments are append-only, so a hold
// written as a comment needs a later comment to lift it.
var dispatchHoldRelease = "HOLD RELEASED"

// DispatchHoldFields reports the hold a bead's own fields assert, or "" when
// they assert none.
//
// It is the field half of the shared hold rule, exported so a caller that has a
// bead's fields but no IssueSource reaches the same verdict instead of a second
// copy of the rule — the witness reads a polecat's hooked bead as JSON to
// decide whether a restart may raise it (gt-n38c6). Comments are not part of
// this: bd show --json omits them, which makes such a caller narrower than
// HoldInComments and never wider. status is a plain string so that
// JSON-reading caller needs no SDK type.
func DispatchHoldFields(status string, labels []string, assignee, design, notes string) string {
	for _, held := range dispatchHoldStatuses {
		if status == held {
			return "status " + status
		}
	}
	// The operator reservation is checked before the routing labels: a bead the
	// operator owns is not the town's to route anywhere, and its reason is the
	// one an operator reading the log needs named (gt-21pl0).
	if reason := OperatorReservation(labels, assignee); reason != "" {
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

// HoldInComments reports the hold a bead's comment history asserts, or "".
//
// It is the comment half of the rule DispatchHoldFields applies to a bead's
// fields. Comments arrive oldest first, and the newest decision is the live
// one: a later release lifts an earlier hold, which is what makes a
// comment-recorded hold releasable (gt-tq6l).
func HoldInComments(comments []beads.Comment) string {
	held := ""
	for _, comment := range comments {
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
