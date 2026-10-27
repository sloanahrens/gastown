package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
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
// verdict judged, from the branch prefix and the bead's assignee
// (AttributeReviews). They add up to om_reviews_1h.
//
//	om_polecat_reviews_1h  verdicts on a polecat branch
//	om_crew_reviews_1h     verdicts on a crew branch
//	om_overseer_reviews_1h verdicts on the overseer's work
//	om_unknown_reviews_1h  verdicts this town cannot attribute
//
// The hour's beads are read with one `bd show` per bead in the review's rig, so
// only the reviews counted are looked up. A read that fails leaves all four
// columns null: a review the source could not attribute is not a review by an
// unknown author, and the counts above still stand. A bead bd no longer holds
// is an unknown author's, which is an answer.
//
// A file that does not exist yet is an hour with no reviews, since the landing
// worker creates it with its first verdict. A file that cannot be read makes
// Collect fail and the group null; a line that will not parse, carries no bead
// or no verdict, or falls outside the hour is skipped rather than failing the
// group.
type reviewSource struct{}

// reviewBdTimeout is one `bd show` deadline. The reviews of an hour are few, and
// one bd hung on a dead Dolt server costs the kind columns rather than the
// group's counts.
const reviewBdTimeout = 5 * time.Second

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
		// tail of an append, or a line this source cannot attribute.
		if json.Unmarshal(line, &rec) != nil || rec.BeadID == "" || rec.Verdict == "" {
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
// attribution that failed: a bead read that errored, or no runner to read it.
func reviewKinds(ctx context.Context, env Env, reviews []Review) map[string]int {
	counts := zeroAuthorKinds()
	if len(reviews) == 0 {
		return counts
	}
	beads, ok := reviewBeads(ctx, env, reviews)
	if !ok {
		return nil
	}
	for _, review := range AttributeReviews(reviews, beads) {
		counts[review.AuthorKind]++
	}
	return counts
}

// reviewBeads reads each reviewed bead once with `bd show`, so a review of a
// bead bd no longer holds is left out and attributed to an unknown author.
// ok is false when a read fails, since that bead's branch is unknown rather
// than absent.
func reviewBeads(ctx context.Context, env Env, reviews []Review) ([]BeadRow, bool) {
	if env.Run == nil {
		return nil, false
	}
	seen := make(map[string]bool, len(reviews))
	var rows []BeadRow
	for _, review := range reviews {
		if seen[review.Bead] {
			continue
		}
		seen[review.Bead] = true
		out, err := reviewBdShow(ctx, env, review)
		if err != nil {
			return nil, false
		}
		shown, ok := ShownBeads(out)
		if !ok {
			return nil, false
		}
		rows = append(rows, shown...)
	}
	return rows, true
}

// reviewBdShow runs `bd show --json` on the review's bead in the review's rig,
// under its own deadline so one slow bd costs its bead rather than the group.
func reviewBdShow(ctx context.Context, env Env, review Review) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, reviewBdTimeout)
	defer cancel()
	dir := env.Town
	if review.Rig != "" {
		dir = filepath.Join(env.Town, review.Rig)
	}
	// The flags precede "--" so the bead id is read as an argument, never a flag.
	return env.Run(callCtx, []string{"bd", "-C", dir, "show", "--json", "--", review.Bead})
}
