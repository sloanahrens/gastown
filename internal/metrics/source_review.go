package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
// A file that does not exist yet is an hour with no reviews, since the landing
// worker creates it with its first verdict. A file that cannot be read makes
// Collect fail and the group null; a line that will not parse, carries no bead
// or no verdict, or falls outside the hour is skipped rather than failing the
// group.
type reviewSource struct{}

func (reviewSource) Group() string { return "review" }

func (reviewSource) Collect(_ context.Context, env Env) (map[string]any, error) {
	cols := map[string]any{
		"om_reviews_1h":         0,
		"om_rejections_1h":      0,
		"om_mean_score_1h":      nil,
		"om_second_attempts_1h": 0,
	}
	path := land.TownReviewsFile(env.Town).Path
	data, err := env.Tail(path, sourceLogTailBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return cols, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading reviews.jsonl: %w", err)
	}
	now := env.Now()
	cutoff := now.Add(-time.Hour)
	reviews, rejections, secondAttempts := 0, 0, 0
	var scoreSum float64
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
		reviews++
		scoreSum += rec.Score
		if rec.Rejected || rec.Verdict != land.VerdictApprove {
			rejections++
		}
		if rec.Attempt >= 2 {
			secondAttempts++
		}
	}
	cols["om_reviews_1h"] = reviews
	cols["om_rejections_1h"] = rejections
	cols["om_second_attempts_1h"] = secondAttempts
	if reviews > 0 {
		cols["om_mean_score_1h"] = scoreSum / float64(reviews)
	}
	return cols, nil
}
