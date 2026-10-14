package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var usageNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// usageWorkRec is a complete polecat work record ending at end, so that a test
// reads as "a record in or out of the window" and nothing else.
func usageWorkRec(bead string, end time.Time, uncached int64) UsageRecord {
	return UsageRecord{
		Bead: bead, Kind: UsageKindWork, Class: UsageClassPolecat, Rig: "gastown",
		Complete: true, Start: end.Add(-time.Minute), End: end, WallS: 60,
		Models: map[string]UsageModel{
			"deepseek-flash": {Calls: 1, Input: uncached, EstCostUSD: 0.25},
		},
		Totals: UsageTotals{Calls: 1, Input: uncached, Uncached: uncached},
	}
}

// The window is the last 24 hours, both edges closed: a record ending exactly a
// day ago is still today's spend, and one ending a second later is not. A
// record with an end in the future has not run yet and is out.
func TestAggregateUsageCountsRecordsThatEndedWithinTheDay(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		end  time.Time
		want bool
	}{
		{"just ended", usageNow, true},
		{"one second inside", usageNow.Add(-24*time.Hour + time.Second), true},
		{"exactly a day ago", usageNow.Add(-24 * time.Hour), true},
		{"one second older", usageNow.Add(-24*time.Hour - time.Second), false},
		{"ending in the future", usageNow.Add(time.Second), false},
		{"a week old", usageNow.Add(-7 * 24 * time.Hour), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := AggregateUsage([]UsageRecord{usageWorkRec("gt-abc", tt.end, 100)}, usageNow, 0)
			if got := len(u.TopBeads); (got == 1) != tt.want {
				t.Fatalf("a record ending %s gave %d top beads, want in-window=%v", tt.end, got, tt.want)
			}
			if u.Window != "24h" {
				t.Errorf("window = %q, want 24h", u.Window)
			}
		})
	}
}

// An incomplete record is a run still in flight. Its totals would understate
// the day, so it is counted separately and left out of every total.
func TestAggregateUsageCountsIncompleteRecordsApartFromTheTotals(t *testing.T) {
	t.Parallel()

	done := usageWorkRec("gt-done", usageNow.Add(-time.Hour), 100)
	running := usageWorkRec("gt-running", usageNow.Add(-time.Hour), 9000)
	running.Complete = false

	u := AggregateUsage([]UsageRecord{done, running}, usageNow, 0)

	if u.Incomplete != 1 {
		t.Errorf("incomplete = %d, want 1", u.Incomplete)
	}
	if len(u.TopBeads) != 1 || u.TopBeads[0].Bead != "gt-done" {
		t.Errorf("an incomplete record reached the top beads: %+v", u.TopBeads)
	}
	if len(u.ByClassModel) != 1 || u.ByClassModel[0].Uncached != 100 {
		t.Errorf("an incomplete record reached the class totals: %+v", u.ByClassModel)
	}
	if len(u.Over20) != 0 {
		t.Errorf("an incomplete record reached Over20: %+v", u.Over20)
	}
}

// A record with no end time cannot be placed in any window. It is skipped and
// counted, and neither the skip nor nonsense numbers in its other fields may
// take the panel down: the scan's files are data, not a contract (gt-xyzs8).
func TestAggregateUsageSkipsARecordWithNoEndRatherThanFailing(t *testing.T) {
	t.Parallel()

	var noEnd UsageRecord
	u := AggregateUsage([]UsageRecord{noEnd, usageWorkRec("gt-abc", usageNow, 100)}, usageNow, 0)

	if u.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", u.Skipped)
	}
	if len(u.TopBeads) != 1 || u.TopBeads[0].Bead != "gt-abc" {
		t.Errorf("the good record did not survive its neighbour: %+v", u.TopBeads)
	}

	// A record that parsed but carries numbers no run could produce is drawn
	// as it is: the panel explains the day, it does not police it.
	odd := usageWorkRec("gt-odd", usageNow, -5)
	odd.WallS = -1
	odd.Models["deepseek-flash"] = UsageModel{Calls: -3, EstCostUSD: -0.5}
	u = AggregateUsage([]UsageRecord{odd}, usageNow, 0)
	if len(u.TopBeads) != 1 || u.Skipped != 0 {
		t.Errorf("an odd record was dropped instead of drawn: %+v", u)
	}
}

// ByClassModel answers "where did the day go" per class and model, adding the
// models' own fields: a model's uncached tokens are its input, output and cache
// creation, which is what the record's total counts too.
func TestAggregateUsageAddsModelsIntoClassAndModelRows(t *testing.T) {
	t.Parallel()

	first := usageWorkRec("gt-a", usageNow.Add(-time.Hour), 0)
	first.Models = map[string]UsageModel{
		"deepseek-flash":    {Calls: 2, Input: 100, Output: 50, CacheCreation: 10, EstCostUSD: 0.05},
		"claude-sonnet-5-5": {Calls: 1, Input: 1, Output: 9, CacheCreation: 0, EstCostUSD: 0.02},
	}
	second := usageWorkRec("gt-b", usageNow.Add(-2*time.Hour), 0)
	second.Class = UsageClassOMReview
	second.Kind = UsageKindOM
	second.Models = map[string]UsageModel{
		"deepseek-flash": {Calls: 3, Input: 200, Output: 100, CacheCreation: 20, EstCostUSD: 0.30},
	}

	u := AggregateUsage([]UsageRecord{first, second}, usageNow, 0)

	want := []UsageClassModelRow{
		{Class: UsageClassOMReview, Model: "deepseek-flash", Calls: 3, Uncached: 320, EstCostUSD: 0.30},
		{Class: UsageClassPolecat, Model: "deepseek-flash", Calls: 2, Uncached: 160, EstCostUSD: 0.05},
		{Class: UsageClassPolecat, Model: "claude-sonnet-5-5", Calls: 1, Uncached: 10, EstCostUSD: 0.02},
	}
	if len(u.ByClassModel) != len(want) {
		t.Fatalf("by class+model = %+v, want %d rows", u.ByClassModel, len(want))
	}
	for i, w := range want {
		if u.ByClassModel[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, u.ByClassModel[i], w)
		}
	}
}

// Two rows of equal cost still need one order, or the page reshuffles them
// between polls: they break on class, then model name.
func TestAggregateUsageSortsEqualCostRowsByName(t *testing.T) {
	t.Parallel()

	a := usageWorkRec("gt-a", usageNow, 0)
	a.Class = UsageClassPolecat
	a.Models = map[string]UsageModel{"z-model": {Calls: 1, EstCostUSD: 0.10}}
	b := usageWorkRec("gt-b", usageNow, 0)
	b.Class = UsageClassDeepReview
	b.Models = map[string]UsageModel{"a-model": {Calls: 1, EstCostUSD: 0.10}}
	c := usageWorkRec("gt-c", usageNow, 0)
	c.Class = UsageClassDeepReview
	c.Models = map[string]UsageModel{"b-model": {Calls: 1, EstCostUSD: 0.10}}

	u := AggregateUsage([]UsageRecord{a, b, c}, usageNow, 0)

	got := []string{u.ByClassModel[0].Class + "/" + u.ByClassModel[0].Model,
		u.ByClassModel[1].Class + "/" + u.ByClassModel[1].Model,
		u.ByClassModel[2].Class + "/" + u.ByClassModel[2].Model}
	want := []string{"deep-review/a-model", "deep-review/b-model", "polecat/z-model"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("class+model order = %v, want %v", got, want)
		}
	}
}

// The last deep reviews are named whatever their age — they run at most daily,
// so a day's window would usually hold none — newest first, five at most, with
// the run's own wall time, tokens and subagent share.
func TestAggregateUsageNamesTheLastFiveDeepReviews(t *testing.T) {
	t.Parallel()

	var recs []UsageRecord
	for i := range 6 {
		r := usageWorkRec("gt-dr"+string(rune('a'+i)), usageNow.Add(-time.Duration(i)*24*time.Hour), 1000)
		r.Class = UsageClassDeepReview
		r.WallS = int64(600 + i)
		r.Subagents = UsageSubagents{Calls: 4, Share: 0.5, Uncached: 300}
		recs = append(recs, r)
	}

	u := AggregateUsage(recs, usageNow, 0)

	if len(u.DeepReviewRuns) != 5 {
		t.Fatalf("deep review rows = %d, want 5", len(u.DeepReviewRuns))
	}
	if u.DeepReviewRuns[0].Bead != "gt-dra" {
		t.Errorf("newest deep review = %q, want gt-dra", u.DeepReviewRuns[0].Bead)
	}
	first := u.DeepReviewRuns[0]
	if first.End != usageNow.Format(time.RFC3339) || first.WallS != 600 || first.Uncached != 1000 || first.SubagentShare != 0.5 {
		t.Errorf("deep review row = %+v, want the record's own end, wall, tokens and share", first)
	}
	if last := u.DeepReviewRuns[4].Bead; last != "gt-dre" {
		t.Errorf("oldest of the five = %q, want gt-dre", last)
	}
}

// Top beads are the window's most expensive rows, five at most, ranked by
// uncached tokens: Claude records carry no cost estimate, so the estimate
// cannot order them, and it breaks ties between records that spent the same.
func TestAggregateUsageRanksTopBeadsByUncachedTokens(t *testing.T) {
	t.Parallel()

	var recs []UsageRecord
	for i := range 6 {
		recs = append(recs, usageWorkRec("gt-"+string(rune('a'+i)), usageNow, int64(100*(i+1))))
	}
	// Two records that spent the same tokens are separated by their estimate.
	tieLow := usageWorkRec("gt-low", usageNow, 50)
	tieLow.EstCostUSD = 0.01
	tieHigh := usageWorkRec("gt-high", usageNow, 50)
	tieHigh.EstCostUSD = 0.90

	u := AggregateUsage(append(recs, tieLow, tieHigh), usageNow, 0)

	if len(u.TopBeads) != 5 {
		t.Fatalf("top beads = %d, want 5", len(u.TopBeads))
	}
	if u.TopBeads[0].Bead != "gt-f" || u.TopBeads[0].Uncached != 600 {
		t.Errorf("top bead = %+v, want the 600-token record", u.TopBeads[0])
	}
	for i := 1; i < len(u.TopBeads); i++ {
		if u.TopBeads[i-1].Uncached < u.TopBeads[i].Uncached {
			t.Fatalf("top beads are not ranked by tokens: %+v", u.TopBeads)
		}
	}

	tie := AggregateUsage([]UsageRecord{tieLow, tieHigh}, usageNow, 0)
	if tie.TopBeads[0].Bead != "gt-high" {
		t.Errorf("equal-token records did not break on cost: %+v", tie.TopBeads)
	}
}

// A bead over twenty minutes is named for a week, since that is how long a
// stall stays worth chasing. The list is work records only — the reviewer's
// om records are a different kind — and it carries what the session was doing:
// a long bead that ran no tests reads differently from one that failed them.
func TestAggregateUsageNamesSlowWorkBeadsForAWeek(t *testing.T) {
	t.Parallel()

	slow := usageWorkRec("gt-slow", usageNow.Add(-time.Hour), 100)
	slow.WallS = 1201
	slow.Activity = UsageActivity{ToolCalls: 90, Edits: 4, TestRuns: 6, FailedTestRuns: 2}
	edge := usageWorkRec("gt-edge", usageNow.Add(-time.Hour), 100)
	edge.WallS = 1200 // exactly twenty minutes is not over it
	old := usageWorkRec("gt-old", usageNow.Add(-7*24*time.Hour-time.Second), 100)
	old.WallS = 5000
	weekEdge := usageWorkRec("gt-week", usageNow.Add(-7*24*time.Hour), 100)
	weekEdge.WallS = 5000
	review := usageWorkRec("gt-om", usageNow.Add(-time.Hour), 100)
	review.Kind = UsageKindOM
	review.Class = UsageClassOMReview
	review.WallS = 9000

	u := AggregateUsage([]UsageRecord{slow, edge, old, weekEdge, review}, usageNow, 0)

	got := []string{}
	for _, r := range u.Over20 {
		got = append(got, r.Bead)
	}
	want := []string{"gt-week", "gt-slow"} // slowest first
	if len(got) != len(want) {
		t.Fatalf("Over20 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Over20 = %v, want %v", got, want)
		}
	}
	if r := u.Over20[1]; r.Rig != "gastown" || r.WallS != 1201 || r.TestRuns != 6 || r.FailedTestRuns != 2 {
		t.Errorf("Over20 row = %+v, want the record's rig, wall and test counts", r)
	}
}

// The reader's two counts are what the scan could not place: sessions matched
// to no bead, and records no window could hold. They pass through untouched.
func TestAggregateUsageCarriesTheReadersCounts(t *testing.T) {
	t.Parallel()

	u := AggregateUsage(nil, usageNow, 7)
	if u.Unmatched != 7 || u.Incomplete != 0 || u.Skipped != 0 {
		t.Errorf("counts = unmatched %d, incomplete %d, skipped %d; want 7, 0, 0", u.Unmatched, u.Incomplete, u.Skipped)
	}
	if u.Unavailable {
		t.Error("an aggregate reports itself unavailable")
	}
	if len(u.ByClassModel) != 0 || len(u.TopBeads) != 0 || len(u.Over20) != 0 || len(u.DeepReviewRuns) != 0 {
		t.Errorf("no records gave rows: %+v", u)
	}
}

// The payload's JSON names are the page's contract, and the record's are the
// scan's: these lock both, so bead 3/3 can promise a page field that the reader
// bead actually fills (gt-xyzs8).
func TestUsagePayloadAndRecordCarryTheAgreedJSONNames(t *testing.T) {
	t.Parallel()

	payload, err := json.Marshal(Usage{
		AsOf:           "2026-10-14T11:00:00Z",
		Window:         "24h",
		ByClassModel:   []UsageClassModelRow{{Class: "polecat", Model: "deepseek-flash"}},
		DeepReviewRuns: []UsageDeepReviewRow{{Bead: "gt-a", End: "2026-10-14T09:00:00Z"}},
		TopBeads:       []UsageTopBead{{Bead: "gt-a", Rig: "gastown"}},
		Over20:         []UsageOver20Bead{{Bead: "gt-a", Rig: "gastown"}},
		Unmatched:      1, Incomplete: 2, Skipped: 3,
	})
	if err != nil {
		t.Fatalf("marshalling the payload: %v", err)
	}
	for _, want := range []string{
		`"as_of"`, `"window"`, `"by_class_model"`, `"deep_review_runs"`, `"top_beads"`,
		`"over20"`, `"unmatched"`, `"incomplete"`, `"skipped"`, `"class"`, `"model"`,
		`"uncached"`, `"est_cost_usd"`, `"subagent_share"`, `"test_runs"`, `"failed_test_runs"`,
	} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("the Usage payload has no %s: %s", want, payload)
		}
	}

	// A scan file parses straight into the record, including the two nullable
	// fields and the millisecond timestamps the scan writes.
	file := `{"bead":"gt-a","kind":"work","class":"polecat","rig":"gastown","seat":null,` +
		`"complete":true,"start":"2026-10-10T18:48:21.155Z","end":"2026-10-10T18:49:56.811Z",` +
		`"wall_s":95,"est_cost_usd":null,"models":{"deepseek-flash":{"calls":38,"input":52509,` +
		`"output":7676,"cache_creation":0,"cache_read":1725184,"est_cost_usd":0.0177}},` +
		`"totals":{"calls":38,"input":52509,"output":7676,"cache_creation":0,` +
		`"cache_read":1725184,"uncached":60185},"subagents":{"calls":0,"share":0,"uncached":0},` +
		`"activity":{"tool_calls":48,"edits":3,"test_runs":5,"failed_test_runs":0}}`
	var rec UsageRecord
	if err := json.Unmarshal([]byte(file), &rec); err != nil {
		t.Fatalf("a scan file does not parse into a UsageRecord: %v", err)
	}
	if rec.Bead != "gt-a" || rec.Kind != UsageKindWork || rec.Class != UsageClassPolecat || rec.Seat != nil {
		t.Errorf("record = %+v, want the file's own bead, kind, class and null seat", rec)
	}
	if rec.End.UTC().Format(time.RFC3339Nano) != "2026-10-10T18:49:56.811Z" {
		t.Errorf("end = %s, want the file's timestamp", rec.End)
	}
	if rec.EstCostUSD != 0 || rec.WallS != 95 || rec.Activity.FailedTestRuns != 0 {
		t.Errorf("record = %+v, want a null estimate read as zero", rec)
	}

	// A file the scan wrote badly is an error, and the reader turns that into
	// a skip; it must never be a panic inside the aggregate.
	var bad UsageRecord
	if err := json.Unmarshal([]byte(`{"bead":`), &bad); err == nil {
		t.Error("a truncated scan file parsed into a record")
	}
	if u := AggregateUsage([]UsageRecord{bad}, usageNow, 0); u.Skipped != 1 {
		t.Errorf("the zero record was not skipped: %+v", u)
	}
}
