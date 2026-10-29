package metrics

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/land"
)

// The author kinds an om review is attributed to: who wrote the branch the
// review judged, as the landing worker writes them on the review record
// (authorkind, gt-b9j3o.16).
const (
	AuthorKindPolecat  = authorkind.Polecat
	AuthorKindCrew     = authorkind.Crew
	AuthorKindOverseer = authorkind.Overseer
	AuthorKindUnknown  = authorkind.Unknown
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
	return authorkind.Seat(branch)
}

// AuthorKind returns the kind of seat that wrote branch: a polecat's branch is
// a polecat's, a crew branch is the crew's, and a crew branch of the overseer
// seat is the overseer's. A branch whose prefix this town does not use, or that
// names no seat, is unknown.
//
// It is the landing worker's rule (authorkind.Kind) rather than a second copy:
// a review's logged kind and a review attributed from notes must agree.
func AuthorKind(branch string) string {
	return authorkind.Kind(branch)
}

// AttributedRecord is one review record with the author kind the review
// attribution derived for it: the kind of the seat that wrote the branch its
// bead names. A table reads the kind from here and never from a branch of its
// own, so every table counts the one derived author_kind (gt-b9j3o.14).
type AttributedRecord struct {
	Record land.ReviewRecord
	// AuthorKind is the branch's seat kind, AuthorKindUnknown when the bead
	// names no branch this town uses.
	AuthorKind string
}

// AttributeRecords derives the author and author kind of each review record
// (AttributeReviews), reading the beads of the stores the records name once.
// Records keep the log's order within a store and the records of a store do
// not interleave with another's, so a caller reading the first review of a bead
// still sees the log's first.
//
// A read that fails, or a rig that is not one path element under the town, is
// an error: a kind derived from a read that broke would be printed as the
// unknown seat's rather than the read being reported. A bead the store no
// longer lists is the unknown author's, which is an answer.
func AttributeRecords(ctx context.Context, env Env, records []land.ReviewRecord) ([]AttributedRecord, error) {
	if len(records) == 0 {
		return nil, nil
	}
	groups := map[string][]land.ReviewRecord{}
	var rigs []string
	for _, rec := range records {
		if _, ok := groups[rec.Rig]; !ok {
			rigs = append(rigs, rec.Rig)
		}
		groups[rec.Rig] = append(groups[rec.Rig], rec)
	}
	sort.Strings(rigs)
	out := make([]AttributedRecord, 0, len(records))
	for _, rig := range rigs {
		// A record read from a file never names the directory bd runs in.
		dir, ok := reviewDir(env.Town, rig)
		if !ok {
			return nil, fmt.Errorf("review of %s names rig %q, which is not one path element under the town",
				groups[rig][0].BeadID, rig)
		}
		beads, err := storeBeads(ctx, env, dir, groups[rig])
		if err != nil {
			return nil, err
		}
		// Attribute each store's beads against that store's own rows: a bead id
		// alone is not unique across a town's stores.
		reviews := make([]Review, len(groups[rig]))
		for i, rec := range groups[rig] {
			reviews[i] = Review{Bead: rec.BeadID, Rig: rec.Rig}
		}
		attributed := AttributeReviews(reviews, beads)
		for i, rec := range groups[rig] {
			out = append(out, AttributedRecord{Record: rec, AuthorKind: attributed[i].AuthorKind})
		}
	}
	return out, nil
}

// storeBeads reads the notes of the beads records name from the store opened in
// dir, storeShowChunk ids per read, and returns them in id order so a reader's
// rows never depend on the store's map. An id that is not a bead id is never
// asked for, since bd reads an argument beginning with a dash as a flag. A read
// that fails is an error; a bead the store leaves out is not, because its
// branch is then unknown rather than unread.
func storeBeads(ctx context.Context, env Env, dir string, records []land.ReviewRecord) ([]BeadRow, error) {
	seen := make(map[string]bool, len(records))
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		if seen[rec.BeadID] {
			continue
		}
		seen[rec.BeadID] = true
		ids = append(ids, rec.BeadID)
	}
	sort.Strings(ids)
	shown, err := showBeads(ctx, env, dir, ids)
	if err != nil {
		return nil, fmt.Errorf("bd show in %s: %w", dir, err)
	}
	beads := make([]BeadRow, 0, len(shown))
	for _, issue := range shown {
		beads = append(beads, BeadRow{ID: issue.ID, Branch: BranchLine(issue.Notes)})
	}
	sort.Slice(beads, func(i, j int) bool { return beads[i].ID < beads[j].ID })
	return beads, nil
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
