package dispatch

import (
	"strings"
	"unicode"
)

// A dispatch hold is a decision recorded on a bead's own record that takes it
// off the generic dispatch path, so an automatic dispatcher's default sling
// does not override it. The daemon's patrol_scan restart reads it through
// DispatchHoldFields.
//
// These markers are machine-read: before writing or releasing one, read
// docs/concepts/dispatch-holds.md, which states the write form of each and the
// paths that read it. dispatchHoldLabels, dispatchHoldStatuses and
// dispatchHoldProse below are that vocabulary.

// routingHoldLabels and humanHoldLabels are the two kinds of decision
// dispatchHoldLabels records, kept apart because a reader that has already
// routed the bead reads only the second one.
//
// routingHoldLabels ask for a seat rather than a hold: needs-pro wants a
// specific runtime, and a dispatcher whose seats include the one that label
// selects (polecat_pool.pro_label) routes the bead to it instead of reading
// the label as a hold (gt-lxxo4). A reader with no seat to match must drop
// these rather than refuse the bead for wearing its own seat's selector
// (SlingHoldFields).
//
// humanHoldLabels are the landings the worker left for a person (om gave no
// verdict, a stage timed out, a policy refusal, or land.MaxReworkAttempts
// rejections): unlike rework, no polecat can settle one (gt-hpca9, gt-28ibg).
// No seat claims them, so they hold wherever they are read.
//
// Both are matched case-insensitively, since a label is typed by hand.
var (
	routingHoldLabels = []string{"needs-pro"}
	humanHoldLabels   = []string{"gt:needs-human", "needs-human"}
	// dispatchHoldLabels is the two lists in the order the rule reports them.
	// The operator reservation (OperatorReservation) is the third decision a
	// label records, and the one that also reaches through the assignee; it is
	// applied in DispatchHoldFields rather than listed here so a caller reads
	// the same rule before it spends a polecat seat.
	dispatchHoldLabels = append(append([]string(nil), routingHoldLabels...), humanHoldLabels...)
)

// dispatchHoldStatuses are the statuses beads calls CategoryFrozen, "excluded
// from bd ready": a dispatcher that fed one would take on work the tracker
// says is not ready. The category test itself is not exported by the SDK, and
// neither is a pinned constant, hence the literals.
var dispatchHoldStatuses = []string{"deferred", "pinned"}

// dispatchHoldProse are the keep-off decisions recorded in a bead's prose,
// listed as they are written so the reason can quote them back.
var dispatchHoldProse = []string{"do not redispatch"}

// DispatchHoldFields reports the hold a bead's own fields assert, or "" when
// they assert none.
//
// It is the shared hold rule for a bead's fields, exported so a caller that has
// a bead's fields but no IssueSource reaches the same verdict instead of a
// second copy of the rule — the witness reads a polecat's hooked bead as JSON
// to decide whether a restart may raise it (gt-n38c6). Comments are not part of
// the rule: bd show --json omits them. status is a plain string so that
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
	return holdFieldReason(labels, assignee, design, notes)
}

// holdFieldReason is DispatchHoldFields' body past its status arm: the hold a
// bead's labels, assignee and prose assert, whatever its status.
func holdFieldReason(labels []string, assignee, design, notes string) string {
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

// SlingHoldFields reports the hold a sling must not take a bead over, or ""
// when the bead asserts none: the fields a dispatch starting mid-flight can
// find changed, read with routingHoldLabels dropped.
//
// It is the shared rule for the read a dispatch makes at the moment it takes a
// bead — after the guards that ran when the dispatch started, by which time
// the bead's fields may have changed. The status is not part of it: the guards
// at the start read that one, with --force's own semantics (a pinned or hooked
// bead is re-slingable under --force, and without it when its holder is dead),
// and reading it again here would refuse the re-slings those guards admit. The
// routing labels are dropped because a sling is already given its target: a
// bead wearing the selector of the seat it is being sent to is being routed,
// not held, and refusing it there would break the pro seat's own dispatch.
// Every other marker holds, the human labels included — which no guard at the
// start reads at all, so this is the read that keeps a hold written
// mid-dispatch from being overwritten by the dispatch's own hook write
// (gt-0k7kb).
func SlingHoldFields(labels []string, assignee, design, notes string) string {
	return holdFieldReason(withoutLabels(labels, routingHoldLabels), assignee, design, notes)
}

// withoutLabels returns labels with every entry of drop removed, matching as
// the rule does (case- and space-insensitive). The input is never modified.
func withoutLabels(labels, drop []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		kept := true
		for _, d := range drop {
			if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(d)) {
				kept = false
				break
			}
		}
		if kept {
			out = append(out, label)
		}
	}
	return out
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
// behind: "- do not redispatch", "> **DO NOT REDISPATCH**", "## DO NOT
// REDISPATCH".
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
