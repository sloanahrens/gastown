package polecat

import "github.com/steveyegge/gastown/internal/beads"

// isHookActiveStatus reports whether a status means a bead is still the
// polecat's live work: Beads.GetAssignedIssue's precedence list, the set every
// dispatch and liveness path already reads as actively assigned.
//
// `open` stays in even though the rendered hook (`gt hook`,
// listAssignedActiveWork) counts only hooked/in_progress: a recovery gate that
// drops an existing refusal needs a reason of its own, not just a smaller
// sibling set (gt-eqiid).
func isHookActiveStatus(status string) bool {
	switch beads.IssueStatus(status) {
	case beads.StatusOpen, beads.StatusInProgress, beads.IssueStatusHooked:
		return true
	default:
		return false
	}
}

// HookBeadDisposition is what the bead a hook reference names actually is,
// told apart from what the reference claims. A hook reference is a recorded
// string (an agent bead's legacy hook_bead field, or an inferred issue), and
// reading it as if it were live state makes a reference that outlived its work
// look like a live hook (gt-eqiid).
type HookBeadDisposition struct {
	// Safe reports that the reference names no work at risk, so it must not
	// raise a hook-still-set blocker.
	Safe bool
	// Terminal reports that the referenced bead is closed or tombstoned.
	Terminal bool
	// Submitted reports that the referenced bead carries gt:ready-to-land and
	// is not yet terminal: the landing worker owns it (gt-obbx2).
	Submitted bool
	// Blocker names the predicate when Safe is false, in the wording the
	// recovery report uses.
	Blocker string
}

// ClassifyHookBead classifies hookBead from the result of showing it: err is
// the lookup error, issue the bead it resolved to (nil when the bead is gone).
// The caller must have attempted the lookup.
//
// An unreadable reference fails closed. Neither a missing bead nor a failed
// lookup proves there is no work, and a gate that clears on "could not tell"
// is the fail-open gt-7kr and gt-14a each closed in turn.
//
// A readable bead that is neither terminal, nor submitted, nor in an active
// assignment status is a stale reference: it outlived the work it named, and
// the rendered hook agrees there is nothing there (gt-eqiid).
func ClassifyHookBead(hookBead string, issue *beads.Issue, err error) HookBeadDisposition {
	if hookBead == "" {
		return HookBeadDisposition{Safe: true}
	}
	if err != nil {
		return HookBeadDisposition{Blocker: "hook_bead=" + hookBead + " status=lookup_error: " + err.Error()}
	}
	if issue == nil {
		return HookBeadDisposition{Blocker: "hook_bead=" + hookBead + " status=missing"}
	}
	// Submitted is tested before the active statuses on purpose: a submitted
	// bead IS status=hooked, so testing active first would report the landing
	// worker's work as an unrecovered hook.
	if IsSubmittedWork(issue) {
		return HookBeadDisposition{Safe: true, Submitted: true}
	}
	if beads.IssueStatus(issue.Status).IsTerminal() {
		return HookBeadDisposition{Safe: true, Terminal: true}
	}
	if isHookActiveStatus(issue.Status) {
		return HookBeadDisposition{Blocker: "hook_bead=" + hookBead + " status=" + issue.Status}
	}
	return HookBeadDisposition{Safe: true}
}
