package cmd

import (
	"sort"
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
	// stages hands back the daemon log's landing stages at or after a time,
	// which is where a review's om duration is: the verdict line carries none,
	// and the daemon log is no source the dashboard may parse itself, so the om
	// reader that already scans it does the reading. Nil leaves every review
	// without a duration, which the table draws as no om time at all. An error
	// is a daemon log that could not be read, which the table says apart from
	// reviews the log has no line for.
	stages func(since time.Time) ([]omStage, error)
}

func newDashReworkReader(townRoot string) *dashReworkReader {
	return &dashReworkReader{file: land.TownReviewsFile(townRoot)}
}

// withStages gives the reader the stage lines of the om reader that scans the
// daemon log.
func (r *dashReworkReader) withStages(stages func(since time.Time) ([]omStage, error)) *dashReworkReader {
	r.stages = stages
	return r
}

// read loads both windows' worth of the log, times the reviews it holds and
// folds them.
func (r *dashReworkReader) read(now time.Time) *dashboard.Rework {
	since := now.Add(-dashboard.ReworkReach)
	recs, err := r.file.Since(since)
	if err != nil {
		return &dashboard.Rework{Unavailable: true}
	}
	reviews := make([]dashboard.ReworkReview, 0, len(recs))
	for _, rec := range recs {
		reviews = append(reviews, dashboard.ReworkReview{
			Bead: rec.BeadID, Rig: rec.Rig, Attempt: rec.Attempt, Score: rec.Score,
			Verdict: rec.Verdict, Rejected: rec.Rejected, Findings: rec.Findings, Bar: rec.Bar, At: rec.At,
		})
	}
	omUnavailable := false
	if r.stages != nil {
		// A review at or after since is timed by a line up to omStageJoinMin
		// before it, so the lines are read from that far back: from since
		// itself, the oldest reviews of the window would lose theirs.
		stages, err := r.stages(since.Add(-omStageJoinMin))
		if err != nil {
			omUnavailable = true
		} else {
			withReviewOM(reviews, stages)
		}
	}
	out := dashboard.AggregateRework(reviews, now)
	out.OMUnavailable = omUnavailable
	return &out
}

// withReviewOM fills each review's om duration in from the stages lines: the
// last line the landing worker logged for the bead at or before the verdict,
// within omStageJoinMin and not already read as another review's. The worker
// logs the line that times a review as the review ends and writes the verdict a
// moment later, so a verdict's own line is always behind it. A bead's reviews
// are timed in order and each line is spent once: the window reaches back
// omStageJoinMin, far enough that a slow rework would otherwise let a review
// claim the line of the review before it and count one om run twice. A review
// left with no line, or with one that named no om stage (the review did not
// run), keeps no duration and is left out of its bead's sum.
func withReviewOM(reviews []dashboard.ReworkReview, stages []omStage) {
	byBead := map[string][]int{}
	for i := range reviews {
		byBead[reviews[i].Bead] = append(byBead[reviews[i].Bead], i)
	}
	for bead, idxs := range byBead {
		sort.Slice(idxs, func(a, b int) bool { return reviews[idxs[a]].At.Before(reviews[idxs[b]].At) })
		var mine []omStage
		for _, st := range stages {
			if st.Bead == bead {
				mine = append(mine, st)
			}
		}
		sort.Slice(mine, func(i, j int) bool { return mine[i].At.Before(mine[j].At) })
		spent := make([]bool, len(mine))
		for _, i := range idxs {
			at := reviews[i].At
			best := -1
			for j := range mine {
				if spent[j] || mine[j].At.After(at) || mine[j].At.Before(at.Add(-omStageJoinMin)) {
					continue
				}
				if best < 0 || mine[j].At.After(mine[best].At) {
					best = j
				}
			}
			if best < 0 {
				continue
			}
			// A line that named no om stage is still this review's: spending it
			// keeps it off the review after it, which has a line of its own.
			spent[best] = true
			if mine[best].OM == nil {
				continue
			}
			d := *mine[best].OM
			reviews[i].OM = &d
		}
	}
}
