package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// The review group: what om judged in the last hour, from the landing worker's
// review log, <town>/.runtime/landings/reviews.jsonl (land.TownReviewsFile).
// Each line is a land.ReviewRecord: the verdict that judged a tree, the rework
// attempt it belongs to, the score, and whether the landing refused the work.
// The log is read through Env.Tail, so its tail is what a long series costs.
//
//	om_reviews_1h         verdicts stamped in the last hour
//	om_rejections_1h      those that refused the work: a request_changes
//	                      verdict or a rejected approval
//	om_mean_score_1h      the mean score of those verdicts, nil when the hour
//	                      has none: no review is not a score of zero
//	om_second_attempts_1h verdicts on a bead's second or later attempt, so a
//	                      bead that came back for rework is counted once per
//	                      review it took
//
// The four author-kind columns split om_reviews_1h by who wrote the branch each
// verdict judged, from the branch prefix the bead's notes name
// (AttributeReviews). They add up to om_reviews_1h.
//
//	om_polecat_reviews_1h  verdicts on a polecat branch
//	om_crew_reviews_1h     verdicts on a crew branch
//	om_overseer_reviews_1h verdicts on the overseer's work
//	om_unknown_reviews_1h  verdicts this town cannot attribute
//
// The hour's beads are read through the bead store seam, one store read per
// bead, in the store of the directory the review names: its rig under the town,
// or the town root for a review that names no rig, where bd routes the bead's
// prefix to its rig. Only the beads the counted reviews name are looked up, at
// most reviewBeadReadCap of them. Each read carries a deadline of its own
// (reviewBdTimeout), so a store slow on one bead cannot hold the reads of the
// next, and the reads together answer to reviewBdBudget. A read that fails, a
// rig that is not one path element under the town, and an hour naming more beads
// than the cap all leave the four columns null: a review the source could not
// attribute is not a review by an unknown author, and the counts above still
// stand. A bead the store no longer holds is an unknown author's, which is an
// answer.
//
// A file that does not exist yet is an hour with no reviews, since the landing
// worker creates it with its first verdict. A file that cannot be read makes
// Collect fail and the group null; a line that will not parse, carries no bead
// or no verdict, or falls outside the hour is skipped rather than failing the
// group.
type reviewSource struct{}

// reviewBdTimeout is one bead's store read deadline. The reviews of an hour are
// few, and one read hung on a dead Dolt server costs the kind columns rather
// than the group's counts.
const reviewBdTimeout = 5 * time.Second

// reviewBeadReadCap bounds the distinct beads one collection reads. The reads
// are sequential, so an hour naming more beads than this is left
// unattributed rather than holding the snapshot open on a log's ids.
const reviewBeadReadCap = 64

// reviewBdBudget bounds all the reads of one collection together. The per-read
// deadline and the cap each bound one factor of the total, so a slow store on
// every one of the capped reads could still hold the snapshot for their product;
// the budget is what keeps the kind columns from costing the snapshot more than
// this. When it runs out the kind columns are null, like any failed read.
const reviewBdBudget = 30 * time.Second

func (reviewSource) Group() string { return "review" }

func (reviewSource) Collect(ctx context.Context, env Env) (map[string]any, error) {
	cols := map[string]any{
		"om_reviews_1h":          0,
		"om_rejections_1h":       0,
		"om_mean_score_1h":       nil,
		"om_second_attempts_1h":  0,
		"om_polecat_reviews_1h":  nil,
		"om_crew_reviews_1h":     nil,
		"om_overseer_reviews_1h": nil,
		"om_unknown_reviews_1h":  nil,
	}
	path := land.TownReviewsFile(env.Town).Path
	data, err := env.Tail(path, sourceLogTailBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return reviewKindColumns(cols, zeroAuthorKinds()), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading reviews.jsonl: %w", err)
	}
	now := env.Now()
	cutoff := now.Add(-time.Hour)
	count, rejections, secondAttempts := 0, 0, 0
	var scoreSum float64
	var reviewed []Review
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec land.ReviewRecord
		// A line with no bead or no verdict names no review; it is the torn
		// tail of an append, or a line this source cannot attribute. A
		// follow-up queue line has no verdict on purpose: land.IsQueueLine is
		// the one decision on what one is, and the feed's bookkeeping is not a
		// review (gt-3arxj).
		if json.Unmarshal(line, &rec) != nil || rec.BeadID == "" || rec.Verdict == "" || land.IsQueueLine(line) {
			continue
		}
		if rec.At.Before(cutoff) || rec.At.After(now) {
			continue
		}
		count++
		scoreSum += rec.Score
		if rec.Rejected || rec.Verdict != land.VerdictApprove {
			rejections++
		}
		if rec.Attempt >= 2 {
			secondAttempts++
		}
		reviewed = append(reviewed, Review{Bead: rec.BeadID, Rig: rec.Rig, Attempt: rec.Attempt})
	}
	cols["om_reviews_1h"] = count
	cols["om_rejections_1h"] = rejections
	cols["om_second_attempts_1h"] = secondAttempts
	if count > 0 {
		cols["om_mean_score_1h"] = scoreSum / float64(count)
	}
	return reviewKindColumns(cols, reviewKinds(ctx, env, reviewed)), nil
}

// reviewKindColumns fills the four author-kind columns from counts. A nil map
// is an attribution that failed, which leaves the columns null.
func reviewKindColumns(cols map[string]any, counts map[string]int) map[string]any {
	if counts == nil {
		return cols
	}
	cols["om_polecat_reviews_1h"] = counts[AuthorKindPolecat]
	cols["om_crew_reviews_1h"] = counts[AuthorKindCrew]
	cols["om_overseer_reviews_1h"] = counts[AuthorKindOverseer]
	cols["om_unknown_reviews_1h"] = counts[AuthorKindUnknown]
	return cols
}

// zeroAuthorKinds is every kind counted at zero, for an hour with no reviews.
func zeroAuthorKinds() map[string]int {
	return map[string]int{
		AuthorKindPolecat:  0,
		AuthorKindCrew:     0,
		AuthorKindOverseer: 0,
		AuthorKindUnknown:  0,
	}
}

// reviewKinds counts the hour's reviews by author kind. An hour with no
// reviews is every kind at zero and reads no bead. A nil return is an
// attribution that failed: a bead read that errored, no store to read it, a
// rig outside the town, or an hour past reviewBeadReadCap.
func reviewKinds(ctx context.Context, env Env, reviews []Review) map[string]int {
	counts := zeroAuthorKinds()
	if len(reviews) == 0 {
		return counts
	}
	beads, ok := reviewBeads(ctx, env, reviews, reviewBdBudget)
	if !ok {
		return nil
	}
	for _, review := range AttributeReviews(reviews, beads) {
		counts[review.AuthorKind]++
	}
	return counts
}

// reviewBeads reads each reviewed bead once through the store of the directory
// its review names, so a review of a bead the store no longer holds is left out
// and attributed to an unknown author. The rows come back in id order, so the
// attribution does not depend on the review log's order. ok is false when a read
// fails, a rig is not under the town, the hour names more beads than
// reviewBeadReadCap, or the reads together outlast budget, since that bead's
// branch is unknown rather than absent.
func reviewBeads(ctx context.Context, env Env, reviews []Review, budget time.Duration) ([]BeadRow, bool) {
	type read struct{ bead, dir string }
	seen := make(map[string]bool, len(reviews))
	reads := make([]read, 0, len(reviews))
	for _, review := range reviews {
		if seen[review.Bead] {
			continue
		}
		seen[review.Bead] = true
		dir, ok := reviewDir(env.Town, review.Rig)
		if !ok {
			return nil, false
		}
		reads = append(reads, read{bead: review.Bead, dir: dir})
	}
	if len(reads) > reviewBeadReadCap {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	rows := make([]BeadRow, 0, len(reads))
	for _, r := range reads {
		row, ok, err := reviewBeadRow(ctx, env, r.dir, r.bead)
		if err != nil {
			return nil, false
		}
		if ok {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, true
}

// reviewDir returns the directory whose store a review is read from: the town
// root when the review names no rig, where bd routes the bead's prefix to its
// rig, else the rig's own directory under the town. ok is false for a rig that
// is not one path element under the town, so a rig read from the log cannot name
// a directory under the town to open a store in.
func reviewDir(town, rig string) (string, bool) {
	if rig == "" {
		return town, true
	}
	if rig == "." || rig == ".." || strings.ContainsAny(rig, `/\`) {
		return "", false
	}
	return filepath.Join(town, rig), true
}

// reviewBeadRow reads one bead through the store of dir under a deadline of its
// own (reviewBdTimeout), so one slow read costs its bead rather than the reads
// of every bead in the directory. ok is false for a bead the store does not
// hold, which is the store's answer rather than a failed read.
//
// The store's argv carries --allow-stale, so a bead reads as stale as the store
// last synced: the hourly counts tolerate that (gt-vu312).
func reviewBeadRow(ctx context.Context, env Env, dir, bead string) (BeadRow, bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, reviewBdTimeout)
	defer cancel()
	found, err := showBeads(callCtx, env, dir, []string{bead})
	if err != nil {
		return BeadRow{}, false, err
	}
	issue, ok := found[bead]
	if !ok || issue == nil || issue.ID == "" {
		return BeadRow{}, false, nil
	}
	return BeadRow{ID: issue.ID, Branch: BranchLine(issue.Notes)}, true, nil
}
