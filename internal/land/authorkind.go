package land

import "strings"

// The author kinds a review record attributes its branch to: who wrote the
// work the review judged. Unknown is a real answer, so a branch this town does
// not name by prefix is never guessed into a seat kind (gt-b9j3o.16).
const (
	AuthorKindPolecat  = "polecat"
	AuthorKindCrew     = "crew"
	AuthorKindOverseer = "overseer"
	AuthorKindUnknown  = "unknown"
)

// crewBranchPrefix opens a crew member's branch, crew/<name>/<bead>; a polecat's
// is PolecatBranchPrefix.
const crewBranchPrefix = "crew/"

// AuthorSeat returns the seat a branch names: the second path segment of
// polecat/<seat>/... or crew/<seat>/..., and "" for a branch of any other
// shape.
func AuthorSeat(branch string) string {
	for _, prefix := range []string{PolecatBranchPrefix, crewBranchPrefix} {
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

// AuthorKind returns the kind of seat that wrote branch: a polecat's branch is
// a polecat's, a crew branch is the crew's, and the overseer's own crew branch
// is the overseer's. A branch whose prefix this town does not use, or that
// names no seat, is unknown.
//
// The branch is the whole input because it is written once, when the seat
// starts the work, so a review keeps the kind it was judged under. A bead's
// assignee is reassigned by dispatch, rework and takeover, so reading it would
// reclassify past reviews whenever the bead changed hands (gt-b9j3o.14).
func AuthorKind(branch string) string {
	seat := AuthorSeat(branch)
	if seat == "" {
		return AuthorKindUnknown
	}
	if strings.HasPrefix(branch, PolecatBranchPrefix) {
		return AuthorKindPolecat
	}
	if seat == "overseer" {
		return AuthorKindOverseer
	}
	return AuthorKindCrew
}
