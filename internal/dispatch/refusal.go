package dispatch

import (
	"regexp"
	"strings"
)

// SlingRefusalMarker is the leading text of every refusal `gt sling` prints for
// a bead that is fine and simply not the sling's to take: the merge queue over
// merge_queue.max_ready_for_dispatch (internal/cmd/sling_backpressure.go,
// gt-xidg), the polecat pool with every seat at its cap
// (internal/cmd/sling_pool.go, gt-jzr1), and a bead reserved for the human
// operator (dispatch.OperatorReservation, gt-21pl0). None of them is a broken
// sling: capacity refusals wait for the town to drain, and an
// operator-reserved bead is never an automatic dispatcher's to take at all.
//
// Automatic dispatchers run sling as a subprocess, so this string — not the
// typed error — is the contract between the guards and their callers. It lives
// here, below both internal/daemon and internal/deacon, because daemon imports
// deacon and the deacon's redispatch needs the same parse (gt-xdaq).
const SlingRefusalMarker = "sling refused:"

// SlingRefusalReason extracts a refusal from a failed sling's stderr, reporting
// false for every other failure. A refusal means "not this sling's to take",
// not "this bead failed": callers defer the bead instead of counting a failure.
//
// The line is returned from the marker on, so cobra's "Error: " and the spawn
// path's "spawning polecat: " wrapping are dropped and the reason is the
// guard's own sentence.
func SlingRefusalReason(stderr string) (string, bool) {
	return firstRefusal(stderr, func(line string) int {
		return strings.Index(line, SlingRefusalMarker)
	})
}

// ReslingRefusalMarker leads sling's refusal to re-sling a bead whose dead
// holder's work survives on a branch, or whose survival cannot be verified
// (internal/cmd reslingSurvivingWorkGuard, gt-vm5g4). Unlike
// SlingRefusalMarker it is not the town at capacity: the work is preserved,
// and an operator resumes it with --branch or discards it with --force. For
// an automatic dispatcher it is still a deferral, never a failed attempt.
const ReslingRefusalMarker = "refusing to re-sling"

// RigUnavailableMarker leads the line sling prints when an e-stop, park or dock
// refuses a dispatch to a rig (sling.Blocked): `cannot sling to rig "x": …`,
// `cannot sling to parked rig "x"`, `cannot sling to docked rig "x"`. Like
// ReslingRefusalMarker it is not the town at capacity, and it carries no
// SlingRefusalMarker — the rig, not the bead, is the reason — so an automatic
// dispatcher reading a subprocess's output finds it by this text. The bead is
// fine: the dispatcher defers until the operator clears the state.
const RigUnavailableMarker = "cannot sling to "

// rigUnavailableLine matches the sentence sling.Blocked prints, with the
// sentence captured. The sentence must lead the line or follow a wrapper's
// ": " (cobra's "Error: ", the spawn path's "spawning polecat: ", the dry-run's
// "gt sling --dry-run: exit status 1: "), and the rig's quoted name must end it
// (a colon, or the end of the line). So a bead title or a log line that quotes
// the sentence mid-line is not a refusal, and the wrapped forms still are.
var rigUnavailableLine = regexp.MustCompile(
	`(?:^|: )(` + regexp.QuoteMeta(RigUnavailableMarker) + `(?:(?:parked|docked) )?rig "(?:[^"\\]|\\.)*"(?::|$))`,
)

// duplicateRefusalLine matches the closing line of the duplicate-content refusal
// (sling.DecideDuplicates). The report's first line leads with "✗" and gives no
// count, so the closing line is the one an automatic dispatcher can find by its
// text: "Refusing to sling <bead>: N overlapping bead(s) share the test(s) above;
// re-sling with --force …". It carries no SlingRefusalMarker, so without this
// match a scheduled run refused for overlapping content reads as a failed run
// (gt-g1xiy). Only the blocking report prints this line: an overlap that only
// warns proceeds and is not a refusal.
var duplicateRefusalLine = regexp.MustCompile(
	`(?:^|: )(Refusing to sling \S+: \d+ overlapping bead\(s\) share the test\(s\) above)`,
)

// SlingRefusalLine extracts a refusal of any kind from a failed sling's output
// or error text, reporting false for every other failure. It is the one
// predicate an automatic dispatcher uses to tell "not this sling's to take"
// from "this sling failed": the SlingRefusalMarker guards, the re-sling guard
// (ReslingRefusalMarker), a rig that is e-stopped, parked or docked
// (rigUnavailableLine), and the duplicate-content guard (duplicateRefusalLine).
// The line is returned from the marker on, as SlingRefusalReason does, so it is
// the guard's own sentence.
//
// internal/cmd also recognizes two typed refusals through errors.Is
// (sling.ErrRigUnavailable, the duplicate-content guard). A subprocess's caller
// cannot see a type, so the rig and duplicate ones are matched here by their
// text.
func SlingRefusalLine(text string) (string, bool) {
	return firstRefusal(text, refusalIndex)
}

// firstRefusal returns the refusal on the first line of text that start finds
// one on, from where it starts, trimmed. It reports false when no line has one.
func firstRefusal(text string, start func(line string) int) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if i := start(line); i >= 0 {
			return strings.TrimSpace(line[i:]), true
		}
	}
	return "", false
}

// refusalIndex is where the earliest refusal marker starts in line, or -1.
func refusalIndex(line string) int {
	first := -1
	note := func(i int) {
		if i >= 0 && (first < 0 || i < first) {
			first = i
		}
	}
	note(strings.Index(line, SlingRefusalMarker))
	note(strings.Index(line, ReslingRefusalMarker))
	if m := rigUnavailableLine.FindStringSubmatchIndex(line); m != nil {
		note(m[2])
	}
	if m := duplicateRefusalLine.FindStringSubmatchIndex(line); m != nil {
		note(m[2])
	}
	return first
}
