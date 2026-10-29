package metrics

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/beads"
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
// dir and returns them in id order so a reader's rows never depend on the
// store's map. A store naming more than storeShowChunk distinct beads is listed
// whole in one read and the rows taken from it by id, because a show of every id
// a chunk at a time was 50 commands and most of a mango run's time (gt-kcuug). A
// smaller store keeps the show, which asks for exactly the ids it wants. A record
// whose id is not a bead id is an error before any store is opened: bd reads an
// argument beginning with a dash as a flag, so such a record would otherwise be
// dropped from the read and printed as an unknown author's, as a bead nobody
// could read. A read that fails is an error; a bead the store leaves out is not,
// because its branch is then unknown rather than unread.
func storeBeads(ctx context.Context, env Env, dir string, records []land.ReviewRecord) ([]BeadRow, error) {
	seen := make(map[string]bool, len(records))
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		if !isBeadID(rec.BeadID) {
			return nil, fmt.Errorf("review names bead id %q, which is not a bead id", rec.BeadID)
		}
		if seen[rec.BeadID] {
			continue
		}
		seen[rec.BeadID] = true
		ids = append(ids, rec.BeadID)
	}
	sort.Strings(ids)
	var issues []*beads.Issue
	if len(ids) > storeShowChunk {
		listed, err := listBeads(ctx, env, dir)
		if err != nil {
			return nil, fmt.Errorf("bd list in %s: %w", dir, err)
		}
		issues = namedBeads(listed, ids)
	} else {
		shown, err := showBeads(ctx, env, dir, ids)
		if err != nil {
			return nil, fmt.Errorf("bd show in %s: %w", dir, err)
		}
		issues = make([]*beads.Issue, 0, len(shown))
		for _, issue := range shown {
			issues = append(issues, issue)
		}
	}
	rows := make([]BeadRow, 0, len(issues))
	for _, issue := range issues {
		rows = append(rows, BeadRow{ID: issue.ID, Branch: BranchLine(issue.Notes)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

// listBeads returns every bead the store in dir holds: one bd list --all with
// Limit 0, since bd's default of 50 rows would drop every bead past it. A
// missing seam or a failed read is an error.
func listBeads(ctx context.Context, env Env, dir string) ([]*beads.Issue, error) {
	if env.Beads == nil {
		return nil, fmt.Errorf("no bead store seam for %s", dir)
	}
	store := env.Beads(ctx, dir)
	if store == nil {
		return nil, fmt.Errorf("no bead store for %s", dir)
	}
	return store.List(beads.ListOptions{Status: "all"})
}

// namedBeads keeps the listed beads the ids name, so a whole-board listing
// answers the rows a show of those ids would: a bead the listing does not hold
// is left out, and an id bd would read as a flag is never matched.
func namedBeads(listed []*beads.Issue, ids []string) []*beads.Issue {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		if isBeadID(id) {
			wanted[id] = true
		}
	}
	held := make([]*beads.Issue, 0, len(wanted))
	for _, issue := range listed {
		if wanted[issue.ID] {
			held = append(held, issue)
		}
	}
	return held
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
