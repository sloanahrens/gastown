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

// AuthorKind returns the kind of seat that wrote branch: polecat or crew from
// the branch prefix, overseer for a crew branch whose bead the overseer took,
// and unknown for a branch prefix this town does not use. assignees is the
// bead's assignee history, any entry of which marks the work as the overseer's.
func AuthorKind(branch string, assignees ...string) string {
	switch {
	case strings.HasPrefix(branch, "polecat/"):
		return AuthorKindPolecat
	case strings.HasPrefix(branch, "crew/"):
		for _, assignee := range assignees {
			if isOverseerSeat(assignee) {
				return AuthorKindOverseer
			}
		}
		return AuthorKindCrew
	default:
		return AuthorKindUnknown
	}
}

// isOverseerSeat reports whether assignee names the overseer: the crew seat
// "overseer" in a rig (town/crew/overseer), or the bare "overseer" an older
// bead carries.
func isOverseerSeat(assignee string) bool {
	parts := strings.Split(strings.TrimSpace(assignee), "/")
	if parts[len(parts)-1] != "overseer" {
		return false
	}
	return len(parts) == 1 || parts[len(parts)-2] == "crew"
}

// AttributeReviews returns the reviews with Author and AuthorKind filled from
// the bead each review names: the seat and kind of the branch on that bead's
// Branch line. A review whose bead is missing, or names no branch, is
// attributed to an unknown author; a missing branch is never read as a
// polecat's. The reviews passed in are not modified.
func AttributeReviews(reviews []Review, beads []BeadRow) []Review {
	byID := make(map[string]BeadRow, len(beads))
	for _, bead := range beads {
		byID[bead.ID] = bead
	}
	out := make([]Review, len(reviews))
	for i, review := range reviews {
		bead := byID[review.Bead]
		review.Author = AuthorSeat(bead.Branch)
		review.AuthorKind = AuthorKind(bead.Branch, bead.Assignee)
		out[i] = review
	}
	return out
}
