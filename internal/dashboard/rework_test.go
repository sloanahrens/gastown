package dashboard

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var reworkNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// rv is one om verdict logged hours before reworkNow.
func rv(bead string, attempt int, verdict string, score, bar float64, hoursAgo float64) ReworkReview {
	return ReworkReview{
		Bead: bead, Rig: "gastown", Attempt: attempt, Verdict: verdict, Score: score, Bar: bar,
		At: reworkNow.Add(-time.Duration(hoursAgo * float64(time.Hour))),
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// withOM is one review as the reader joined it to its stages line: om took
// secs. A review the log gave no duration for is one without this.
func withOM(r ReworkReview, secs float64) ReworkReview {
	d := time.Duration(secs * float64(time.Second))
	r.OM = &d
	return r
}

func reworkBars(t *testing.T, w ReworkWindow) map[float64]ReworkBar {
	t.Helper()
	out := map[float64]ReworkBar{}
	for _, b := range w.Bars {
		out[b.Bar] = b
	}
	return out
}

// The table's numbers are the ones tools/om-rework-stats.py prints for the same
// reviews: a bead belongs to the bar in force at its first review, first-try
// approvals are beads whose first review approved, reviews per landed bead
// averages over the beads with an approval, and a loop is a bead rejected three
// times with no approval (gt-mctxx).
func TestAggregateReworkGroupsByTheBarAtAbeadsFirstReview(t *testing.T) {
	t.Parallel()

	const approve, reject = "approve", "request_changes"
	reviews := []ReworkReview{
		// gt-a: lands first try under 0.8. Its verdict was logged twice (a
		// landing retried after the verdict), which counts once.
		rv("gt-a", 1, approve, 0.9, 0.8, 30), rv("gt-a", 1, approve, 0.9, 0.8, 29),
		// gt-b: two rejections, then lands.
		rv("gt-b", 1, reject, 0.5, 0.8, 60), rv("gt-b", 2, reject, 0.6, 0.8, 50), rv("gt-b", 3, approve, 0.85, 0.8, 40),
		// gt-c: rejected three times, never approved: a loop that ran out.
		rv("gt-c", 1, reject, 0.4, 0.8, 100), rv("gt-c", 2, reject, 0.4, 0.8, 90), rv("gt-c", 3, reject, 0.4, 0.8, 80),
		// gt-d: first reviewed under 0.7, approved after the bar moved to 0.8;
		// the whole loop is the 0.7 bar's.
		rv("gt-d", 1, reject, 0.6, 0.7, 20), rv("gt-d", 2, approve, 0.82, 0.8, 10),
		// gt-e: its tree names no bar.
		rv("gt-e", 1, approve, 0.95, 0, 5),
		// gt-f reviewed in the week before; gt-g is older than both windows.
		rv("gt-f", 1, approve, 0.9, 0.8, 24*10), rv("gt-g", 1, approve, 0.9, 0.8, 24*20),
	}
	got := AggregateRework(reviews, reworkNow)
	if len(got.Windows) != 2 || got.Windows[0].Name != "last 7 days" || got.Windows[1].Name != "the 7 days before" {
		t.Fatalf("windows = %+v, want the last 7 days then the 7 days before", got.Windows)
	}

	week := got.Windows[0]
	if len(week.Bars) != 3 || week.Bars[0].Bar != 0.7 || week.Bars[1].Bar != 0.8 || week.Bars[2].Bar != 0 {
		t.Fatalf("bars = %+v, want 0.7, 0.8 and the unknown bar last", week.Bars)
	}
	bars := reworkBars(t, week)
	b8 := bars[0.8]
	if b8.Reviews != 7 || b8.Rejected != 5 || b8.Beads != 3 || b8.FirstTry != 1 || b8.Landed != 2 || b8.LoopsOut != 1 ||
		!near(b8.ReviewsPerLanded, 2) || !near(b8.MeanScore, 4.05/7) {
		t.Errorf("0.8 bar = %+v, want 7 reviews, 5 rejected, 3 beads, 1 first try, 2 landed at 2.00 reviews, 1 loop out, mean 0.5786", b8)
	}
	b7 := bars[0.7]
	if b7.Reviews != 2 || b7.Rejected != 1 || b7.Beads != 1 || b7.FirstTry != 0 || b7.Landed != 1 || b7.LoopsOut != 0 ||
		!near(b7.ReviewsPerLanded, 2) || !near(b7.MeanScore, 0.71) {
		t.Errorf("0.7 bar = %+v, want gt-d's two reviews, landed after a rejection", b7)
	}
	b0 := bars[0]
	if b0.Reviews != 1 || b0.Rejected != 0 || b0.FirstTry != 1 || b0.Landed != 1 || !near(b0.ReviewsPerLanded, 1) {
		t.Errorf("unknown bar = %+v, want gt-e's one first-try approval", b0)
	}

	before := got.Windows[1]
	if len(before.Bars) != 1 || before.Bars[0].Bar != 0.8 || before.Bars[0].Reviews != 1 || before.Bars[0].FirstTry != 1 {
		t.Errorf("the week before = %+v, want only gt-f", before.Bars)
	}
}

// A bead rejected but not yet three times, or approved after three, is not a
// loop out; and a window with no reviews has no bars, not a nil list.
func TestAggregateReworkLoopsAndEmptyWindows(t *testing.T) {
	t.Parallel()

	const approve, reject = "approve", "request_changes"
	got := AggregateRework([]ReworkReview{
		rv("gt-two", 1, reject, 0.4, 0.8, 3), rv("gt-two", 2, reject, 0.4, 0.8, 2),
		rv("gt-late", 1, reject, 0.4, 0.8, 9), rv("gt-late", 2, reject, 0.4, 0.8, 8), rv("gt-late", 3, reject, 0.4, 0.8, 7), rv("gt-late", 4, approve, 0.9, 0.8, 6),
	}, reworkNow)
	b := got.Windows[0].Bars[0]
	if b.LoopsOut != 0 || b.Landed != 1 || !near(b.ReviewsPerLanded, 4) || b.Beads != 2 {
		t.Errorf("bar = %+v, want no loop out, gt-late landed on its fourth review", b)
	}
	if got.Windows[1].Bars == nil || len(got.Windows[1].Bars) != 0 {
		t.Errorf("empty window bars = %#v, want an empty list", got.Windows[1].Bars)
	}
	raw, err := json.Marshal(AggregateRework(nil, reworkNow))
	if err != nil || !strings.Contains(string(raw), `"bars":[]`) || strings.Contains(string(raw), "null") {
		t.Errorf("empty payload = %s (%v), want empty bar lists and no null", raw, err)
	}
}

// Each window takes the reviews from its start up to its end: the last seven
// days include the instant they open and the instant of now, and the instant
// they open is not the earlier window's.
func TestAggregateReworkWindowEdges(t *testing.T) {
	t.Parallel()

	week := 7 * 24.0
	got := AggregateRework([]ReworkReview{
		rv("gt-now", 1, "approve", 0.9, 0.8, 0),
		rv("gt-open", 1, "approve", 0.9, 0.8, week),
		rv("gt-prior-open", 1, "approve", 0.9, 0.8, 2*week),
		rv("gt-before", 1, "approve", 0.9, 0.8, 2*week+1),
		{Bead: "gt-future", Rig: "gastown", Attempt: 1, Verdict: "approve", Bar: 0.8, At: reworkNow.Add(time.Minute)},
	}, reworkNow)
	if n := got.Windows[0].Bars[0].Reviews; n != 2 {
		t.Errorf("last 7 days hold %d reviews, want gt-now and gt-open", n)
	}
	if n := got.Windows[1].Bars[0].Reviews; n != 1 {
		t.Errorf("the 7 days before hold %d reviews, want only gt-prior-open", n)
	}
	if got.Windows[0].From != "2026-10-07T12:00:00Z" || got.Windows[0].To != "2026-10-14T12:00:00Z" || got.Windows[1].From != "2026-09-30T12:00:00Z" {
		t.Errorf("window bounds = %+v", got.Windows)
	}
}

// Each window reaches the page with its own span under the keys the page dates
// it by, since the reader names both older stretches "the 7 days before" and the
// page can only tell them apart from from/to.
func TestReworkWindowsCarryTheirSpan(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(AggregateRework(nil, reworkNow))
	if err != nil {
		t.Fatalf("marshalling the table: %v", err)
	}
	var payload struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("reading the table: %v", err)
	}
	want := []struct{ from, to string }{
		{"2026-10-07T12:00:00Z", "2026-10-14T12:00:00Z"},
		{"2026-09-30T12:00:00Z", "2026-10-07T12:00:00Z"},
	}
	if len(payload.Windows) != len(want) {
		t.Fatalf("payload = %s, want %d windows", raw, len(want))
	}
	for i, w := range want {
		if got := payload.Windows[i]["from"]; got != w.from {
			t.Errorf("window %d from = %v, want %s", i, got, w.from)
		}
		if got := payload.Windows[i]["to"]; got != w.to {
			t.Errorf("window %d to = %v, want %s", i, got, w.to)
		}
	}
}

// The state the page reads carries the table, and a read that failed reaches it
// as itself.
func TestReworkReachesTheAPI(t *testing.T) {
	t.Parallel()

	for _, r := range []*Rework{
		{Windows: []ReworkWindow{{Name: "last 7 days", Bars: []ReworkBar{{Bar: 0.8, Reviews: 3}}}}},
		{Unavailable: true},
	} {
		h := NewHub(Config{Now: func() time.Time { return reworkNow }, Rework: func() *Rework { return r }})
		h.pollRework()

		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/state = %d", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"rework"`) {
			t.Fatalf("/api/state does not carry the rework the hub holds: %s", body)
		}
		if got := strings.Contains(body, `"unavailable":true`); got != r.Unavailable {
			t.Errorf("/api/state unavailable = %v, want %v: %s", got, r.Unavailable, body)
		}
	}
}

// A reader that reports nothing leaves the state as it was, and the poll
// defaults to a minute.
func TestReworkNilReadKeepsTheLastStateAndDefaultsToAMinute(t *testing.T) {
	t.Parallel()

	var next *Rework
	h := NewHub(Config{Rework: func() *Rework { return next }})
	h.pollRework()
	if h.State().Rework != nil {
		t.Fatal("a nil read set the table")
	}
	next = &Rework{Windows: []ReworkWindow{{Name: "last 7 days"}}}
	h.pollRework()
	next = nil
	h.pollRework()
	if got := h.State().Rework; got == nil || len(got.Windows) != 1 {
		t.Errorf("a nil read replaced the last state with %+v", got)
	}

	var c Config
	c.defaults()
	if c.ReworkEvery != 60*time.Second {
		t.Errorf("ReworkEvery defaults to %v, want 1m", c.ReworkEvery)
	}
}

// A bead whose approvals the landing bounced for findings is a rejected bead,
// not a first-try landing: the verdicts read approve and the table counts them
// as the three rejections they were (gt-vu18r).
func TestReworkSpanCountsFindingsBounce(t *testing.T) {
	t.Parallel()
	bounced := func(attempt int, hoursAgo float64) ReworkReview {
		r := rv("gt-bounced", attempt, "approve", 0.93, 0.8, hoursAgo)
		r.Rejected = true
		return r
	}
	got := AggregateRework([]ReworkReview{bounced(1, 6), bounced(2, 5), bounced(3, 4)}, reworkNow)
	b := got.Windows[0].Bars[0]
	if b.Rejected != 3 || b.FirstTry != 0 || b.Landed != 0 || b.LoopsOut != 1 || b.Beads != 1 || b.Reviews != 3 {
		t.Errorf("bar = %+v, want 3 rejected, no first try, none landed, the bead looped out", b)
	}

	// A third review the findings let through is the landing after two bounces.
	got = AggregateRework([]ReworkReview{bounced(1, 6), bounced(2, 5), rv("gt-bounced", 3, "approve", 0.93, 0.8, 4)}, reworkNow)
	b = got.Windows[0].Bars[0]
	if b.Rejected != 2 || b.FirstTry != 0 || b.Landed != 1 || b.LoopsOut != 0 || !near(b.ReviewsPerLanded, 3) {
		t.Errorf("bar = %+v, want 2 rejected then a landing on the third review", b)
	}
}

// A log written before Rejected existed carries a findings bounce as a plain
// approve, and the bead's next attempt is what shows the approval did not land
// it. Newer lines in the same log keep what they say.
func TestReworkSpanReadsLegacyBouncesInAMixedVersionLog(t *testing.T) {
	t.Parallel()
	withFindings := func(r ReworkReview, n int, rejected bool) ReworkReview {
		r.Findings, r.Rejected = n, rejected
		return r
	}
	tests := []struct {
		name    string
		reviews []ReworkReview
		want    ReworkBar // only the fields compared below
	}{
		{
			name: "legacy approvals with findings, then a landing",
			reviews: []ReworkReview{
				withFindings(rv("gt-m", 1, "approve", 0.9, 0.8, 6), 3, false),
				withFindings(rv("gt-m", 2, "approve", 0.9, 0.8, 5), 2, false),
				withFindings(rv("gt-m", 3, "approve", 0.9, 0.8, 4), 0, false),
			},
			want: ReworkBar{Rejected: 2, FirstTry: 0, Landed: 1, ReviewsPerLanded: 3},
		},
		{
			name: "legacy bounce then newer lines that say what they are",
			reviews: []ReworkReview{
				withFindings(rv("gt-m", 1, "approve", 0.9, 0.8, 6), 2, false),
				withFindings(rv("gt-m", 2, "approve", 0.9, 0.8, 5), 1, true),
				withFindings(rv("gt-m", 3, "approve", 0.9, 0.8, 4), 0, false),
			},
			want: ReworkBar{Rejected: 2, FirstTry: 0, Landed: 1, ReviewsPerLanded: 3},
		},
		{
			name: "an approval with no findings was not bounced for them",
			reviews: []ReworkReview{
				rv("gt-m", 1, "approve", 0.9, 0.8, 6),
				rv("gt-m", 2, "approve", 0.9, 0.8, 5),
			},
			want: ReworkBar{Rejected: 0, FirstTry: 1, Landed: 1, ReviewsPerLanded: 2},
		},
		{
			name: "the newest approval has no later attempt to show a bounce",
			reviews: []ReworkReview{
				withFindings(rv("gt-m", 1, "approve", 0.9, 0.8, 6), 3, false),
			},
			want: ReworkBar{Rejected: 0, FirstTry: 1, Landed: 1, ReviewsPerLanded: 1},
		},
		{
			// The line cannot say why the landing refused the work after this
			// approval, only that the bead was reviewed again.
			name: "an approval with findings reviewed again counts as refused",
			reviews: []ReworkReview{
				withFindings(rv("gt-m", 1, "approve", 0.9, 0.8, 6), 1, false),
				withFindings(rv("gt-m", 2, "approve", 0.9, 0.8, 5), 0, false),
			},
			want: ReworkBar{Rejected: 1, FirstTry: 0, Landed: 1, ReviewsPerLanded: 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := AggregateRework(tc.reviews, reworkNow).Windows[0].Bars[0]
			if b.Rejected != tc.want.Rejected || b.FirstTry != tc.want.FirstTry || b.Landed != tc.want.Landed ||
				b.LoopsOut != 0 || !near(b.ReviewsPerLanded, tc.want.ReviewsPerLanded) {
				t.Errorf("bar = %+v, want rejected %d, first try %d, landed %d, %.0f reviews per landed",
					b, tc.want.Rejected, tc.want.FirstTry, tc.want.Landed, tc.want.ReviewsPerLanded)
			}
		})
	}
}

// A bar's om time is the mean, over the landed beads the log timed, of each
// bead's own total: every review it had before landing, summed. A review the
// log gave no duration for is left out of its bead's total and counted, and a
// bead with no duration at all is left out of the mean rather than counted as
// free (gt-uiiml).
func TestAggregateReworkMeanOMTime(t *testing.T) {
	t.Parallel()

	const approve, reject = "approve", "request_changes"
	got := AggregateRework([]ReworkReview{
		// gt-one landed on its only review, 40s of om.
		withOM(rv("gt-one", 1, approve, 0.9, 0.8, 6), 40),
		// gt-three was reviewed three times, and the middle verdict's stages
		// line named no om stage: its total is the two that carry one, 240s.
		withOM(rv("gt-three", 1, reject, 0.5, 0.8, 5), 90),
		rv("gt-three", 2, reject, 0.6, 0.8, 4),
		withOM(rv("gt-three", 3, approve, 0.85, 0.8, 3), 150),
		// gt-loop was rejected three times and never landed: its reviews are
		// not part of any bead's total, and none of them is counted missing.
		rv("gt-loop", 1, reject, 0.4, 0.8, 5), rv("gt-loop", 2, reject, 0.4, 0.8, 4), rv("gt-loop", 3, reject, 0.4, 0.8, 3),
		// gt-none landed and no review of it has a time.
		rv("gt-none", 1, approve, 0.9, 0.8, 2),
	}, reworkNow)

	b := got.Windows[0].Bars[0]
	if b.Landed != 3 || b.LoopsOut != 1 {
		t.Fatalf("bar = %+v, want gt-one, gt-three and gt-none landed and gt-loop out", b)
	}
	if b.OMBeads != 2 || !near(b.MeanOMSecs, 140) || !near(b.MedianOMSecs, 140) ||
		!near(b.SlowestOMSecs, 240) || b.SlowestOMBead != "gt-three" {
		t.Errorf("om time = %+v, want 140s mean over gt-one's 40 and gt-three's 240, slowest gt-three", b)
	}
	if b.OMReviewsMissing != 2 {
		t.Errorf("om reviews missing = %d, want gt-three's middle review and gt-none's only one", b.OMReviewsMissing)
	}
}

// The median is the middle of the landed beads' totals, the mean of the two
// middle ones when the count is even; a bar whose beads have no om time at all
// has no mean to draw.
func TestAggregateReworkMeanOMTimeMedianAndEmptyBars(t *testing.T) {
	t.Parallel()

	const approve, reject = "approve", "request_changes"
	got := AggregateRework([]ReworkReview{
		withOM(rv("gt-a", 1, approve, 0.9, 0.8, 6), 60),
		withOM(rv("gt-b", 1, approve, 0.9, 0.8, 5), 120),
		withOM(rv("gt-c", 1, approve, 0.9, 0.8, 4), 300),
		withOM(rv("gt-d", 1, approve, 0.9, 0.8, 3), 900),
	}, reworkNow)
	b := got.Windows[0].Bars[0]
	if b.OMBeads != 4 || !near(b.MeanOMSecs, 345) || !near(b.MedianOMSecs, 210) || !near(b.SlowestOMSecs, 900) {
		t.Errorf("om time = %+v, want a mean of 345s and a median of 210s over four beads", b)
	}

	// Nothing landed in this bar, and the window after it holds nothing at all.
	empty := AggregateRework([]ReworkReview{
		rv("gt-x", 1, reject, 0.4, 0.5, 3), rv("gt-x", 2, reject, 0.4, 0.5, 2),
	}, reworkNow)
	eb := empty.Windows[0].Bars[0]
	if eb.OMBeads != 0 || eb.MeanOMSecs != 0 || eb.MedianOMSecs != 0 || eb.SlowestOMSecs != 0 || eb.SlowestOMBead != "" || eb.OMReviewsMissing != 0 {
		t.Errorf("a bar with no landing = %+v, want no om time and no missing reviews", eb)
	}
	if len(empty.Windows[1].Bars) != 0 {
		t.Errorf("a window with no reviews = %+v, want no bars", empty.Windows[1].Bars)
	}
}

// A bead's om time is the sum of its reviews inside the window, as every other
// figure in the table is: a loop that began before the window opened is summed
// from its first review inside it, and the reviews before are the prior
// window's.
func TestAggregateReworkOMTimeStopsAtTheWindowEdge(t *testing.T) {
	t.Parallel()

	const approve, reject = "approve", "request_changes"
	got := AggregateRework([]ReworkReview{
		// gt-edge was refused eight days ago, 600s of om, and landed three hours ago.
		withOM(rv("gt-edge", 1, reject, 0.5, 0.8, 8*24), 600),
		withOM(rv("gt-edge", 2, approve, 0.9, 0.8, 3), 100),
	}, reworkNow)

	b := got.Windows[0].Bars[0]
	if b.Landed != 1 || b.OMBeads != 1 || !near(b.MeanOMSecs, 100) || !near(b.SlowestOMSecs, 100) || b.OMReviewsMissing != 0 {
		t.Errorf("recent window = %+v, want gt-edge's one review in it, 100s", b)
	}
	p := got.Windows[1].Bars[0]
	if p.Landed != 0 || p.OMBeads != 0 || p.MeanOMSecs != 0 {
		t.Errorf("prior window = %+v, want the refused review and no om time, nothing landed in it", p)
	}
}

// A bar whose landed beads all lack a time reaches the page with no mean, no
// median, no slowest bead and the count of reviews the log gave nothing for,
// and the JSON keeps the fields the page tests against.
func TestAggregateReworkOMTimeJSONWhenNoLandedBeadHasOne(t *testing.T) {
	t.Parallel()

	const approve, reject = "approve", "request_changes"
	got := AggregateRework([]ReworkReview{
		rv("gt-a", 1, approve, 0.9, 0.8, 6),
		rv("gt-b", 1, reject, 0.5, 0.8, 5), rv("gt-b", 2, approve, 0.9, 0.8, 4),
	}, reworkNow)
	raw, err := json.Marshal(got.Windows[0].Bars[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"landed": 2, "om_beads": 0, "mean_om_secs": 0, "median_om_secs": 0, "slowest_om_secs": 0, "om_reviews_missing": 3}
	for k, v := range want {
		if g, ok := m[k].(float64); !ok || g != v {
			t.Errorf("%s = %v in %s, want %v", k, m[k], raw, v)
		}
	}
	if _, ok := m["slowest_om_bead"]; ok {
		t.Errorf("slowest_om_bead present in %s, want it omitted with no timed bead", raw)
	}
}

// ReworkRefused is the one reading both readers of the review log share: a
// verdict that is not an approval, a refused approval, and an approval with
// findings that a later attempt follows are refused; a clean approval is not.
func TestReworkRefusedReadsEachAttemptTheSameWay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		verdict      string
		rejected     bool
		findings     int
		laterAttempt bool
		want         bool
	}{
		{"clean approval, last", "approve", false, 0, false, false},
		{"clean approval with findings, last", "approve", false, 2, false, false},
		{"approval with findings, later attempt (pre-field bounce)", "approve", false, 2, true, true},
		{"approval with no findings, later attempt", "approve", false, 0, true, false},
		{"approval refused on its line", "approve", true, 1, false, true},
		{"request changes", "request_changes", false, 2, false, true},
	}
	for _, tc := range tests {
		if got := ReworkRefused(tc.verdict, tc.rejected, tc.findings, tc.laterAttempt); got != tc.want {
			t.Errorf("%s: ReworkRefused = %t, want %t", tc.name, got, tc.want)
		}
	}
}
