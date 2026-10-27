package metrics

import (
	"regexp"
	"strings"
)

// The author kinds an om review is attributed to: who wrote the branch the
// review judged. Unknown is a real answer, so a branch this town does not name
// by prefix is never guessed into a seat kind.
const (
	AuthorKindPolecat  = "polecat"
	AuthorKindCrew     = "crew"
	AuthorKindOverseer = "overseer"
	AuthorKindUnknown  = "unknown"
)

// branchLineRE matches a Branch line the bead itself carries. It is anchored,
// so the "  | " quoted gate tail of a rejection note cannot name a branch; a
// CRLF line break from a hand-written note is tolerated.
var branchLineRE = regexp.MustCompile(`(?m)^Branch:[ \t]*(\S+)[ \t\r]*$`)

// BranchLine returns the branch the bead's notes name on their last Branch
// line, "" when they name none. A rework appends the new request under the
// last note, so the last line is the bead's live branch.
func BranchLine(notes string) string {
	matches := branchLineRE.FindAllStringSubmatch(notes, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// AuthorSeat returns the seat a branch names: the second path segment of
// polecat/<seat>/... or crew/<seat>/..., and "" for a branch of any other
// shape.
func AuthorSeat(branch string) string {
	for _, prefix := range []string{"polecat/", "crew/"} {
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
// a polecat's, a crew branch is the crew's, and a crew branch of the overseer
// seat is the overseer's. A branch whose prefix this town does not use, or that
// names no seat, is unknown.
//
// The branch is the whole input because it is written once, when the seat
// starts the work, so a review keeps the kind it was judged under. A bead's
// assignee is reassigned by dispatch, rework and takeover, so reading it would
// reclassify past reviews whenever the bead changed hands.
func AuthorKind(branch string) string {
	seat := AuthorSeat(branch)
	if seat == "" {
		return AuthorKindUnknown
	}
	if strings.HasPrefix(branch, "polecat/") {
		return AuthorKindPolecat
	}
	if seat == "overseer" {
		return AuthorKindOverseer
	}
	return AuthorKindCrew
}

// AttributeReviews returns the reviews with Author and AuthorKind filled from
// the branch on the last Branch line of the bead each review names: the seat
// that wrote it and that seat's kind. A review whose bead is missing, or whose
// bead names no branch, is attributed to an unknown author rather than guessed
// into a seat kind. The reviews passed in are not modified.
func AttributeReviews(reviews []Review, beads []BeadRow) []Review {
	byID := make(map[string]BeadRow, len(beads))
	for _, bead := range beads {
		byID[bead.ID] = bead
	}
	out := make([]Review, len(reviews))
	for i, review := range reviews {
		branch := byID[review.Bead].Branch
		review.Author = AuthorSeat(branch)
		review.AuthorKind = AuthorKind(branch)
		out[i] = review
	}
	return out
}
