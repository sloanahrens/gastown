package dashboard

import (
	"sort"
	"time"
)

// The Cost panel's om rework table: what each review bar costs in rework, from
// the one line the landing worker appends per om verdict. The reader lives with
// the town's other readers, internal/cmd/dashboard_rework.go; this file holds
// the payload and the arithmetic, so the windows and the grouping are testable
// without a disk. The grouping is the one ~/.claude/tools/om-rework-stats.py
// prints, which is the figure the table has to match (gt-mctxx).

const (
	// reworkWindow is how far back each of the table's two windows reaches:
	// the last seven days, and the seven days before them.
	reworkWindow = 7 * 24 * time.Hour
	// ReworkLoopCap is the rejection count that ends a bead's rework loop. It
	// is land.MaxReworkAttempts; the dashboard does not import the landing
	// package, and a test in internal/cmd holds the two equal.
	ReworkLoopCap = 3
	// ReworkReach is how much of the review log a reader must load for
	// AggregateRework to see both windows.
	ReworkReach = 2 * reworkWindow
)

// reworkApproved is the verdict value of an approving review.
const reworkApproved = "approve"

// ReworkReview is one om verdict from the landing worker's review log.
// Rejected is the landing refusing the work on an approving verdict, for its
// findings (gt-vu18r); such a review is not an approval.
type ReworkReview struct {
	Bead     string
	Rig      string
	Attempt  int
	Score    float64
	Verdict  string
	Rejected bool
	Findings int
	Bar      float64
	At       time.Time
}

// approved is whether the review let the work land: an approving verdict the
// landing did not turn back for its findings. Lines written before the log
// carried Rejected read as the verdict alone, which reworkSpan corrects for an
// approval the bead was reviewed again after.
func (r ReworkReview) approved() bool { return r.Verdict == reworkApproved && !r.Rejected }

// ReworkBar is one review bar's share of a window. Bar is 0 when the reviewed
// tree named none, which the page draws as unknown.
type ReworkBar struct {
	Bar float64 `json:"bar"`
	// Reviews counts the om verdicts under the bar, Rejected the ones that
	// were not approvals.
	Reviews  int `json:"reviews"`
	Rejected int `json:"rejected"`
	// Beads counts the beads whose first review in the window ran under the
	// bar, and FirstTry the ones whose first review approved.
	Beads    int `json:"beads"`
	FirstTry int `json:"first_try"`
	// Landed counts the beads with an approving review, and ReviewsPerLanded
	// is the mean number of reviews those beads took; 0 when none landed.
	Landed           int     `json:"landed"`
	ReviewsPerLanded float64 `json:"reviews_per_landed"`
	// LoopsOut counts the beads rejected ReworkLoopCap times with no approval.
	LoopsOut  int     `json:"loops_out"`
	MeanScore float64 `json:"mean_score"`
}

// ReworkWindow is one span of the table: the bars that reviewed in it,
// ascending with the unknown bar last.
type ReworkWindow struct {
	Name string      `json:"name"`
	From string      `json:"from"`
	To   string      `json:"to"`
	Bars []ReworkBar `json:"bars"`
}

// Rework is the table's payload. Unavailable says the log could not be read,
// which the page draws as itself and never as a week with no reviews.
type Rework struct {
	Windows     []ReworkWindow `json:"windows"`
	Unavailable bool           `json:"unavailable,omitempty"`
}

// AggregateRework folds the review log into the table's two windows, the last
// seven days and the seven before them.
//
// A bead belongs to the bar in force at its first review in the window, so a
// rework loop that crosses a bar change is not split across two bars. A bead's
// attempts are its reviews in time order, and a verdict logged twice for the
// same attempt (a landing retried after the verdict, before the merge) counts
// once, as the later of the two. An approval followed by a later attempt of the
// same bead is counted as a rejection: that is how a findings bounce reads in
// the lines written before the log carried Rejected.
func AggregateRework(reviews []ReworkReview, now time.Time) Rework {
	recent, prior := now.Add(-reworkWindow), now.Add(-2*reworkWindow)
	return Rework{Windows: []ReworkWindow{
		reworkSpan("last 7 days", reviews, recent, now, true),
		reworkSpan("the 7 days before", reviews, prior, recent, false),
	}}
}

// reworkSpan folds the reviews from from up to to, to itself included only for
// the window that ends at now: a review stamped at that instant has happened.
func reworkSpan(name string, reviews []ReworkReview, from, to time.Time, closed bool) ReworkWindow {
	w := ReworkWindow{
		Name: name, From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339),
		Bars: []ReworkBar{},
	}
	type beadKey struct{ rig, bead string }
	type attemptKey struct {
		beadKey
		attempt int
	}
	latest := map[attemptKey]ReworkReview{}
	for _, r := range reviews {
		if r.At.Before(from) || r.At.After(to) || (!closed && r.At.Equal(to)) {
			continue
		}
		k := attemptKey{beadKey{r.Rig, r.Bead}, r.Attempt}
		if old, ok := latest[k]; !ok || !r.At.Before(old.At) {
			latest[k] = r
		}
	}
	byBead := map[beadKey][]ReworkReview{}
	for _, r := range latest {
		k := beadKey{r.Rig, r.Bead}
		byBead[k] = append(byBead[k], r)
	}

	type tally struct {
		ReworkBar
		scoreSum      float64
		landedReviews int
	}
	bars := map[float64]*tally{}
	for _, rs := range byBead {
		sort.Slice(rs, func(i, j int) bool {
			if !rs[i].At.Equal(rs[j].At) {
				return rs[i].At.Before(rs[j].At)
			}
			return rs[i].Attempt < rs[j].Attempt
		})
		t := bars[rs[0].Bar]
		if t == nil {
			t = &tally{ReworkBar: ReworkBar{Bar: rs[0].Bar}}
			bars[rs[0].Bar] = t
		}
		t.Beads++
		// An approval the bead was reviewed again after did not land it, whatever
		// its line says: a log written before Rejected existed carries every
		// findings bounce as a plain approve, and the next attempt is what shows it.
		stood := func(i int) bool { return rs[i].approved() && i == len(rs)-1 }
		if stood(0) {
			t.FirstTry++
		}
		rejected, approved := 0, false
		for i, r := range rs {
			t.Reviews++
			t.scoreSum += r.Score
			if stood(i) {
				approved = true
			} else {
				rejected++
			}
		}
		t.Rejected += rejected
		switch {
		case approved:
			t.Landed++
			t.landedReviews += len(rs)
		case rejected >= ReworkLoopCap:
			t.LoopsOut++
		}
	}
	for _, t := range bars {
		if t.Landed > 0 {
			t.ReviewsPerLanded = float64(t.landedReviews) / float64(t.Landed)
		}
		t.MeanScore = t.scoreSum / float64(t.Reviews)
		w.Bars = append(w.Bars, t.ReworkBar)
	}
	// The bar a tree named orders the rows, and the trees that named none come
	// last: a 0 is "unknown", not a bar lower than the rest.
	sort.Slice(w.Bars, func(i, j int) bool {
		a, b := w.Bars[i].Bar, w.Bars[j].Bar
		if (a == 0) != (b == 0) {
			return b == 0
		}
		return a < b
	})
	return w
}
