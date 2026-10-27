package metrics

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
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

// reviewBeadChunk is how many bead ids one `bd show` reads: a town with many
// merged beads is several commands, not one command line of every id.
const reviewBeadChunk = 64

// AttributeRecords derives the author and author kind of each review record
// (AttributeReviews), reading the beads of the stores the records name once.
// Records keep the log's order within a store and the records of a store do
// not interleave with another's, so a caller reading the first review of a bead
// still sees the log's first.
//
// A read that fails, output that is not a bead list, or a rig that is not one
// path element under the town is an error: a kind derived from a read that
// broke would be printed as the unknown seat's rather than the read being
// reported. A bead the store no longer lists is the unknown author's, which is
// an answer.
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

// storeBeads reads the notes of the beads records name from the store bd runs
// in dir, reviewBeadChunk ids per call. A call that fails, or whose output is
// not a bead list, is an error; a bead the listing leaves out is not, because
// its branch is then unknown rather than unread.
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
	var beads []BeadRow
	for start := 0; start < len(ids); start += reviewBeadChunk {
		end := min(start+reviewBeadChunk, len(ids))
		argv := []string{"bd", "-C", dir, "show", "--json"}
		for _, id := range ids[start:end] {
			// --id keeps a bead id from being read as a flag: the ids come from
			// the review log, which this package does not write.
			argv = append(argv, "--id="+id)
		}
		out, err := env.Run(ctx, argv)
		if err != nil {
			return nil, fmt.Errorf("bd show in %s: %w", dir, err)
		}
		shown, ok := ShownBeads(out)
		if !ok {
			return nil, fmt.Errorf("bd show in %s: the output is not a bead list", dir)
		}
		beads = append(beads, shown...)
	}
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
