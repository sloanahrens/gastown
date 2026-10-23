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
// prints, which is the figure the table has to match (gt-mctxx). The om time
// per bar is a second pass over the same records, and only the reader can fill
// it: the verdict line carries no duration, so the reader joins it to the
// stages line the landing worker logged for that review (gt-uiiml).

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
type ReworkReview struct {
	Bead    string
	Rig     string
	Attempt int
	Score   float64
	Verdict string
	// Rejected is the landing refusing the work on an approving verdict, for
	// its findings (gt-vu18r); such a review is not an approval.
	Rejected bool
	Findings int
	Bar      float64
	At       time.Time
	// OM is how long this review's om run took, which the reader joins on from
	// the stages line the landing worker logged as the review ended. Nil when
	// the log gave no duration for it: the review did not run, or its stages
	// line is gone.
	OM *time.Duration
}

// ReworkRefused is whether the landing turned an attempt back: its verdict was
// not an approval, the landing refused it (rejected), or it approved with
// findings and a later attempt of the same bead follows. A line written before
// Rejected existed carries a findings bounce as a plain approve, and that last
// case is how such a line reads. The rework panel and the agent counters both
// call this, so they agree on every attempt in the review log.
func ReworkRefused(verdict string, rejected bool, findings int, laterAttempt bool) bool {
	return rejected || verdict != reworkApproved || (findings > 0 && laterAttempt)
}

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
	// MeanOMSecs is the mean, over the OMBeads landed beads whose om time the
	// log gave, of each bead's total om time in seconds — every review it had
	// in this window, summed, so a rework loop that began before the window
	// opened is summed from its first review inside it. It is 0 with no OMBeads, which the page draws as
	// a dash rather than a fast bar. The denominator is the landed beads the
	// sums are known for and not every landed bead, since an unknown sum is not
	// a zero one.
	MeanOMSecs float64 `json:"mean_om_secs"`
	OMBeads    int     `json:"om_beads"`
	// MedianOMSecs and SlowestOMSecs are the median and the largest of those
	// per-bead sums, and SlowestOMBead names the bead the largest belongs to.
	// The mean is the column and these three are its hover.
	MedianOMSecs  float64 `json:"median_om_secs"`
	SlowestOMSecs float64 `json:"slowest_om_secs"`
	SlowestOMBead string  `json:"slowest_om_bead,omitempty"`
	// OMReviewsMissing counts the reviews of landed beads the log gave no om
	// duration for. They are left out of the sums, and the page says how many
	// they are, so a bead averaged over part of its reviews reads as that.
	OMReviewsMissing int `json:"om_reviews_missing"`
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
	// OMUnavailable says the daemon log the om times come from could not be
	// read, so no review has a duration; the page says so rather than count
	// every review as one the log had no line for.
	OMUnavailable bool `json:"om_unavailable,omitempty"`
}

// AggregateRework folds the review log into the table's two windows, the last
// seven days and the seven before them.
//
// A bead belongs to the bar in force at its first review in the window, so a
// rework loop that crosses a bar change is not split across two bars. A bead's
// attempts are its reviews in time order, and a verdict logged twice for the
// same attempt (a landing retried after the verdict, before the merge) counts
// once, as the later of the two. An approval that carried findings and was
// followed by a later attempt of the same bead counts as a rejection, whatever
// its line says (see reworkSpan).
//
// A bar's om time is read from the landed beads alone: each bead's sum of its
// reviews' durations in the window, averaged over the beads the log gave one
// for. Like every other count here, a bead's sum stops at the window's edge.
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

	type omSum struct {
		bead string
		d    time.Duration
	}
	type tally struct {
		ReworkBar
		scoreSum      float64
		landedReviews int
		// omSums is one entry per landed bead whose om time the log gave, in
		// the order the beads were folded; the mean and its hover are read off
		// it once every bar is complete.
		omSums []omSum
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
		// The lines written before Rejected existed carry a findings bounce as a
		// plain approve, and omitempty leaves a line that was not rejected
		// looking the same, so the line cannot tell the two apart. The bead's
		// next attempt can: attempts count the landing's rejections, so an
		// approval with findings that a later attempt follows was refused, for
		// its findings or, on a newer line, for what came after the review. An
		// approval with no findings was never a findings bounce, and the bead's
		// last review has no later attempt to show it was refused.
		refused := func(i int) bool {
			return ReworkRefused(rs[i].Verdict, rs[i].Rejected, rs[i].Findings, i < len(rs)-1)
		}
		if !refused(0) {
			t.FirstTry++
		}
		rejected, approved := 0, false
		var om time.Duration
		omKnown := 0
		for i, r := range rs {
			t.Reviews++
			t.scoreSum += r.Score
			if r.OM != nil {
				om += *r.OM
				omKnown++
			}
			if refused(i) {
				rejected++
			} else {
				approved = true
			}
		}
		t.Rejected += rejected
		switch {
		case approved:
			t.Landed++
			t.landedReviews += len(rs)
			// The bead's om time is every review it had in the window, the
			// ones with no duration left out; a bead that has none at all is
			// left out of the mean rather than counted as free.
			t.OMReviewsMissing += len(rs) - omKnown
			if omKnown > 0 {
				t.omSums = append(t.omSums, omSum{bead: rs[0].Bead, d: om})
			}
		case rejected >= ReworkLoopCap:
			t.LoopsOut++
		}
	}
	for _, t := range bars {
		if t.Landed > 0 {
			t.ReviewsPerLanded = float64(t.landedReviews) / float64(t.Landed)
		}
		t.MeanScore = t.scoreSum / float64(t.Reviews)
		sums := make([]time.Duration, 0, len(t.omSums))
		var total time.Duration
		slowest := omSum{}
		for i, s := range t.omSums {
			sums = append(sums, s.d)
			total += s.d
			if i == 0 || s.d > slowest.d {
				slowest = s
			}
		}
		if len(sums) > 0 {
			sort.Slice(sums, func(i, j int) bool { return sums[i] < sums[j] })
			t.OMBeads = len(sums)
			t.MeanOMSecs = total.Seconds() / float64(len(sums))
			t.MedianOMSecs = medianDuration(sums).Seconds()
			t.SlowestOMSecs = slowest.d.Seconds()
			t.SlowestOMBead = slowest.bead
		}
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

// medianDuration is the middle of an ascending slice: the mean of the two
// middle entries when the count is even, since taking the lower of the two
// would report one bead's total as the middle of two.
func medianDuration(ascending []time.Duration) time.Duration {
	n := len(ascending)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return ascending[n/2]
	}
	return (ascending[n/2-1] + ascending[n/2]) / 2
}
