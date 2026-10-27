// Package authorkind is the one rule for the seat that wrote a branch, as the
// landing worker records it on a review (gt-b9j3o.16) and the metrics report
// attributes a review from a bead's notes with (gt-b9j3o.14). It is a leaf
// package so both read the same rule without the report importing the landing
// worker.
package authorkind

import "strings"

// The author kinds a review is attributed to: who wrote the work the review
// judged. Unknown is a real answer, so a branch this town does not name by
// prefix is never guessed into a seat kind.
const (
	Polecat  = "polecat"
	Crew     = "crew"
	Overseer = "overseer"
	Unknown  = "unknown"
)

// The branch prefixes the town names a seat's work by: a polecat's branch is
// polecat/<seat>/<bead>, a crew member's is crew/<seat>/<bead>.
const (
	PolecatPrefix = "polecat/"
	CrewPrefix    = "crew/"
)

// Seat returns the seat a branch names: the second path segment of
// polecat/<seat>/... or crew/<seat>/..., and "" for a branch of any other
// shape.
func Seat(branch string) string {
	for _, prefix := range []string{PolecatPrefix, CrewPrefix} {
		rest, ok := strings.CutPrefix(branch, prefix)
		if !ok {
			continue
		}
		seat, _, ok := strings.Cut(rest, "/")
		if !ok || seat == "" {
			return ""
		}
		return seat
	}
	return ""
}

// Kind returns the kind of seat that wrote branch: a polecat's branch is a
// polecat's, a crew branch is the crew's, and the overseer's own crew branch
// is the overseer's. A branch whose prefix this town does not use, or that
// names no seat, is unknown.
//
// The branch is the whole input because it is written once, when the seat
// starts the work, so a review keeps the kind it was judged under. A bead's
// assignee is reassigned by dispatch, rework and takeover, so reading it would
// reclassify past reviews whenever the bead changed hands.
func Kind(branch string) string {
	seat := Seat(branch)
	if seat == "" {
		return Unknown
	}
	if strings.HasPrefix(branch, PolecatPrefix) {
		return Polecat
	}
	if seat == "overseer" {
		return Overseer
	}
	return Crew
}
