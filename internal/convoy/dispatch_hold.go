package convoy

import (
	"context"
	"strings"
	"unicode"

	beadsdk "github.com/steveyegge/beads"
)

// dispatchHoldLabels are the routing decisions recorded as labels: needs-sonnet
// wants a specific runtime, needs-mayor-review wants the mayor's eyes before any
// work starts. Matched case-insensitively, since a label is typed by hand
// (gt-tq6l).
var dispatchHoldLabels = []string{"needs-sonnet", "needs-mayor-review"}

// dispatchHoldStatuses are the statuses beads calls CategoryFrozen, "excluded
// from bd ready": a convoy that fed one would dispatch work the tracker says is
// not ready. The category test itself is not exported by the SDK, and neither
// is a pinned constant, hence the literal (gt-tq6l).
var dispatchHoldStatuses = []beadsdk.Status{beadsdk.StatusDeferred, "pinned"}

// dispatchHoldProse are the keep-off decisions recorded in a bead's prose,
// listed as they are written so the reason can quote them back (gt-tq6l).
var dispatchHoldProse = []string{"MAYOR DESIGN DECISION", "do not redispatch"}

// DispatchHoldReason reports why issueID's own record takes it off the generic
// convoy dispatch path, or "" when the bead may be dispatched: the feeder slings
// with the rig default agent, which is what a routing label or a keep-off
// decision forbids (gt-tq6l). Both feeders share the rule — the daemon's
// stranded scan and the event-driven continuation feed.
//
// Every read failure reports "" (fail open): a bead the town cannot read must
// not stall a convoy. store holds the rig that owns issueID; a non-nil resolver
// redirects a cross-rig bead to its own store.
func DispatchHoldReason(ctx context.Context, store beadsdk.Storage, issueID string, resolver *StoreResolver) string {
	owner := store
	if resolver != nil {
		if name := resolver.storeForID(issueID); name != "" && resolver.stores[name] != nil {
			owner = resolver.stores[name]
		}
	}
	if owner == nil {
		return ""
	}

	issue, err := owner.GetIssue(ctx, issueID)
	if err != nil || issue == nil {
		return ""
	}

	// The cheap fields decide most holds; only a record that reaches the comment
	// check pays for its comment history (gt-tq6l).
	if reason := dispatchHoldInFields(issue); reason != "" {
		return reason
	}

	// A decision written as a comment never reaches the fields above, so the
	// record's comments are part of the check.
	comments, err := owner.GetIssueComments(ctx, issueID)
	if err != nil {
		return ""
	}
	for _, comment := range comments {
		if decision := holdDecisionIn(comment.Text); decision != "" {
			return decision + " in comment"
		}
	}
	return ""
}

// dispatchHoldInFields applies the hold rule to the fields GetIssue returns.
func dispatchHoldInFields(issue *beadsdk.Issue) string {
	for _, status := range dispatchHoldStatuses {
		if issue.Status == status {
			return "status " + string(status)
		}
	}
	for _, label := range issue.Labels {
		for _, held := range dispatchHoldLabels {
			if strings.EqualFold(label, held) {
				return "label " + label
			}
		}
	}
	if decision := holdDecisionIn(issue.Design); decision != "" {
		return decision + " in design"
	}
	if decision := holdDecisionIn(issue.Notes); decision != "" {
		return decision + " in notes"
	}
	return ""
}

// holdDecisionIn returns the first keep-off decision recorded in text, or "".
func holdDecisionIn(text string) string {
	folded := foldDecisionText(text)
	for _, decision := range dispatchHoldProse {
		if strings.Contains(folded, foldDecisionText(decision)) {
			return decision
		}
	}
	return ""
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
