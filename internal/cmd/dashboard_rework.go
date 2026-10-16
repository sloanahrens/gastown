package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
)

// The Cost panel's om rework reader: the landing worker's review log, folded by
// AggregateRework into the table the page draws. It reads the log the worker
// writes (<townRoot>/.runtime/landings/reviews.jsonl) and nothing else - the
// daemon's own log is not a source the dashboard may parse, which is why the
// worker writes this file at all (gt-mctxx).

// dashReworkReader reads the review log. A log that does not exist yet is a
// week with no reviews, since the landing worker creates it with its first
// verdict; one that cannot be read is an unavailable table.
type dashReworkReader struct {
	file *land.ReviewsFile
}

func newDashReworkReader(townRoot string) *dashReworkReader {
	return &dashReworkReader{file: land.TownReviewsFile(townRoot)}
}

// read loads both windows' worth of the log and folds it.
func (r *dashReworkReader) read(now time.Time) *dashboard.Rework {
	recs, err := r.file.Since(now.Add(-dashboard.ReworkReach))
	if err != nil {
		return &dashboard.Rework{Unavailable: true}
	}
	reviews := make([]dashboard.ReworkReview, 0, len(recs))
	for _, rec := range recs {
		reviews = append(reviews, dashboard.ReworkReview{
			Bead: rec.BeadID, Rig: rec.Rig, Attempt: rec.Attempt, Score: rec.Score,
			Verdict: rec.Verdict, Findings: rec.Findings, Bar: rec.Bar, At: rec.At,
		})
	}
	out := dashboard.AggregateRework(reviews, now)
	return &out
}
