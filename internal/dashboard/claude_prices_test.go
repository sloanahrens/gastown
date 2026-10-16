package dashboard

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The price file is data about the world: the test reads it for the shape a
// caller depends on - a date, a source, four positive rates per family, the
// town's own models priced - and leaves the age to the reader that reports it,
// which is where an old table can be acted on (TestClaudePricesAgedAtTheBound).
func TestClaudePricesFileIsCurrentAndComplete(t *testing.T) {
	t.Parallel()

	prices, err := EmbeddedClaudePrices()
	if err != nil {
		t.Fatalf("the embedded price file does not decode: %v", err)
	}
	if _, err := time.Parse(time.DateOnly, prices.AsOf); err != nil {
		t.Fatalf("as_of = %q, want a YYYY-MM-DD date: %v", prices.AsOf, err)
	}
	if !strings.HasPrefix(prices.Source, "http") {
		t.Errorf("source = %q, want the URL the rates were read from", prices.Source)
	}
	if prices.Unit != "usd_per_million_tokens" || prices.Prefix == "" {
		t.Errorf("unit = %q, prefix = %q, want a stated unit and the model ids that are Claude's", prices.Unit, prices.Prefix)
	}
	if len(prices.Families) == 0 {
		t.Fatal("the price file carries no families")
	}
	for family, f := range prices.Families {
		if f.Input <= 0 || f.Output <= 0 {
			t.Errorf("%s: input = %v, output = %v, want both above zero", family, f.Input, f.Output)
		}
		if f.CacheWrite1h <= 0 || f.CacheRead <= 0 || f.CacheRead > f.Input {
			t.Errorf("%s: cache write 1h = %v, cache read = %v, want both above zero and a read below the input rate",
				family, f.CacheWrite1h, f.CacheRead)
		}
	}
	// The models the town actually runs are priced, so the figure the Cost panel
	// draws from live records is not a floor out of the box.
	for _, model := range []string{"claude-sonnet-5-5", "claude-opus-5-5"} {
		if _, ok := prices.Family(model); !ok {
			t.Errorf("the price file has no rates for %s, which the town runs", model)
		}
	}
}

// The bound on the rates' age is the reader's to apply, at the clock it measures
// the plan at: a table inside the bound is current, one past it is not, and a
// date nothing can read is a table nobody can vouch for. A pinned midnight, so a
// table dated a whole number of days back is that age exactly and the bound's own
// edge is the case under test rather than the hour the suite happens to run.
func TestClaudePricesAgedAtTheBound(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.October, 16, 0, 0, 0, 0, time.UTC)
	dated := func(back time.Duration) *ClaudePriceTable {
		p := priceTable(testFamily())
		p.AsOf = at.Add(-back).Format(time.DateOnly)
		return p
	}
	for _, tt := range []struct {
		name  string
		table *ClaudePriceTable
		aged  bool
	}{
		{"rates read today", dated(0), false},
		{"rates read a day inside the bound", dated(claudePricesMaxAge - 24*time.Hour), false},
		{"rates on the bound's own day", dated(claudePricesMaxAge), false},
		{"rates a day past the bound", dated(claudePricesMaxAge + 24*time.Hour), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			age, aged := tt.table.Aged(at)
			if aged != tt.aged {
				t.Errorf("%s: aged = %v (age %s), want %v", tt.name, aged, age.Round(24*time.Hour), tt.aged)
			}
		})
	}

	undated := priceTable(testFamily())
	undated.AsOf = "the Ides"
	if _, aged := undated.Aged(at); !aged {
		t.Errorf("a table dated %q read as current, want aged: its rates have no age to read", undated.AsOf)
	}
}

// priceTable is a table the cases below price against, in the shape the file
// decodes into: one family whose four rates are easy to read off a token count,
// so a case's dollars are its arithmetic and not the file's.
func priceTable(families map[string]ClaudePriceFamily) *ClaudePriceTable {
	return &ClaudePriceTable{
		AsOf: "2026-10-01", Source: "https://example.test/pricing",
		Unit: "usd_per_million_tokens", Prefix: "claude-",
		Families: families,
	}
}

// testFamily is the fixture's rates: $2 in, $4 a 1-hour cache write, $0.20 a
// cache read, $10 out, per million tokens.
func testFamily() map[string]ClaudePriceFamily {
	return map[string]ClaudePriceFamily{
		"claude-test": {Input: 2, CacheWrite1h: 4, CacheRead: 0.2, Output: 10},
	}
}

// priceRecord is one scan record ending at end, complete and holding models.
func priceRecord(end time.Time, models map[string]UsageModel) UsageRecord {
	return UsageRecord{
		Bead: "gt-x", Kind: UsageKindWork, Class: UsageClassPolecat, Rig: "gastown",
		Complete: true, Start: end.Add(-time.Minute), End: end, Models: models,
	}
}

// apiNow is the clock the window cases below are placed against: mid-month, so
// the week sits inside the billing month and the two windows can be told apart
// by a record that falls in one and not the other.
var apiNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// The two windows are not the same window: the week is the seven days to now and
// the month is the calendar month, so a record ten days old is in one and not
// the other, and a week that straddles the first of the month is in neither of
// the month's days before it.
func TestAPIEquivalentPricesTheWeekAndTheBillingMonth(t *testing.T) {
	t.Parallel()

	// A million of each class at the fixture's rates: 2 + 4 + 0.2 + 10.
	full := map[string]UsageModel{"claude-test": {Input: 1e6, CacheCreation: 1e6, CacheRead: 1e6, Output: 1e6}}
	// Input and output alone: 2 + 10.
	inOut := map[string]UsageModel{"claude-test": {Input: 1e6, Output: 1e6}}

	for _, tt := range []struct {
		name string
		now  time.Time
		// weekEnds is how far ahead of now the plan's weekly limit resets, which
		// places the week's window: it opens seven days before that instant.
		weekEnds time.Duration
		records  []UsageRecord
		wantWeek float64
		wantMon  float64
	}{
		{
			name: "a record in both windows counts twice, once each",
			now:  apiNow, records: []UsageRecord{priceRecord(apiNow.Add(-2*24*time.Hour), full)},
			wantWeek: 16.20, wantMon: 16.20,
		},
		{
			name: "a record older than the week is the month's alone",
			now:  apiNow, records: []UsageRecord{priceRecord(apiNow.Add(-10*24*time.Hour), full)},
			wantWeek: 0, wantMon: 16.20,
		},
		{
			name: "a record from before the billing month is in neither",
			now:  apiNow, records: []UsageRecord{priceRecord(apiNow.Add(-20*24*time.Hour), full)},
			wantWeek: 0, wantMon: 0,
		},
		{
			name:     "a week straddling the first of the month is the week's alone",
			now:      time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
			records:  []UsageRecord{priceRecord(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), inOut)},
			wantWeek: 12, wantMon: 0,
		},
		{
			name: "the week's own edge counts",
			now:  apiNow, records: []UsageRecord{priceRecord(apiNow.Add(-claudeAPIWeek), inOut)},
			wantWeek: 12, wantMon: 12,
		},
		{
			// The plan's limit resets two days out, so its window opened five
			// days ago: a record six days old is inside the seven days to now
			// and outside the week the line's own percentage is measuring.
			name: "the week is the plan's window, not the seven days to now",
			now:  apiNow, weekEnds: 48 * time.Hour,
			records:  []UsageRecord{priceRecord(apiNow.Add(-6*24*time.Hour), inOut)},
			wantWeek: 0, wantMon: 12,
		},
		{
			// A reset instant that leaves no day in the past is a window that
			// has just opened: the seven days to now is what the week falls
			// back to rather than a window with nothing in it.
			name: "a window with no past in it falls back to the week to now",
			now:  apiNow, weekEnds: 8 * 24 * time.Hour,
			records:  []UsageRecord{priceRecord(apiNow.Add(-6*24*time.Hour), inOut)},
			wantWeek: 12, wantMon: 12,
		},
		{
			// The plan's limit reset a day ago, so a week closing at that
			// instant opened eight days back: a record seven and a half days old
			// is outside the seven days to now and outside the plan's week, and
			// a window drawn from the passed reset would have counted it.
			name: "a reset the clock has passed does not stretch the week",
			now:  apiNow, weekEnds: -24 * time.Hour,
			records:  []UsageRecord{priceRecord(apiNow.Add(-180*time.Hour), inOut)},
			wantWeek: 0, wantMon: 12,
		},
		{
			// The same passed reset, and a record inside the seven days to now:
			// the fallback window is the week the line measures, not an empty
			// one.
			name: "a reset the clock has passed falls back to the week to now",
			now:  apiNow, weekEnds: -24 * time.Hour,
			records:  []UsageRecord{priceRecord(apiNow.Add(-6*24*time.Hour), inOut)},
			wantWeek: 12, wantMon: 12,
		},
		{
			name: "a record still in flight is not counted",
			now:  apiNow,
			records: []UsageRecord{func() UsageRecord {
				r := priceRecord(apiNow.Add(-time.Hour), full)
				r.Complete = false
				return r
			}()},
			wantWeek: 0, wantMon: 0,
		},
		{
			name: "a record with no end cannot be placed in either window",
			now:  apiNow,
			records: []UsageRecord{func() UsageRecord {
				r := priceRecord(apiNow.Add(-time.Hour), full)
				r.End = time.Time{}
				return r
			}()},
			wantWeek: 0, wantMon: 0,
		},
		{
			name: "a record ending in the future has not run yet",
			now:  apiNow, records: []UsageRecord{priceRecord(apiNow.Add(time.Hour), full)},
			wantWeek: 0, wantMon: 0,
		},
		{
			name: "every record adds to the figure",
			now:  apiNow,
			records: []UsageRecord{
				priceRecord(apiNow.Add(-2*time.Hour), full),
				priceRecord(apiNow.Add(-3*time.Hour), inOut),
			},
			wantWeek: 28.20, wantMon: 28.20,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// No offset is the limit closing at now, the window the line would
			// have drawn had the status line's newest sample been this instant.
			got := APIEquivalent(tt.records, priceTable(testFamily()), tt.now, tt.now.Add(tt.weekEnds).Unix())
			if got.WeekUSD != tt.wantWeek || got.MonthUSD != tt.wantMon {
				t.Errorf("week = $%.2f, month = $%.2f, want $%.2f and $%.2f",
					got.WeekUSD, got.MonthUSD, tt.wantWeek, tt.wantMon)
			}
		})
	}
}

// The four token classes are priced at their own rates rather than one blended
// rate: a cache read is a tenth of an input token and an output token five
// times it, so a figure that added the classes together would be wrong by
// whatever mix the town happened to run.
func TestAPIEquivalentPricesEachTokenClassAtItsOwnRate(t *testing.T) {
	t.Parallel()

	one := func(m UsageModel) []UsageRecord {
		return []UsageRecord{priceRecord(apiNow.Add(-time.Hour), map[string]UsageModel{"claude-test": m})}
	}
	for _, tt := range []struct {
		name  string
		model UsageModel
		want  float64
	}{
		{"input", UsageModel{Input: 1e6}, 2},
		{"cache write 1h", UsageModel{CacheCreation: 1e6}, 4},
		{"cache read", UsageModel{CacheRead: 1e6}, 0.2},
		{"output", UsageModel{Output: 1e6}, 10},
		{"a tenth of a million", UsageModel{Output: 1e5}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := APIEquivalent(one(tt.model), priceTable(testFamily()), apiNow, apiNow.Unix()); got.WeekUSD != tt.want {
				t.Errorf("%s = $%v, want $%v", tt.name, got.WeekUSD, tt.want)
			}
		})
	}
}

// A family is matched by the longest prefix its model id starts with, so a
// version the table prices on its own is not billed at its predecessor's rates.
func TestAPIEquivalentMatchesTheLongestFamilyPrefix(t *testing.T) {
	t.Parallel()

	prices := priceTable(map[string]ClaudePriceFamily{
		"claude-opus":   {Input: 1, CacheWrite1h: 2, CacheRead: 0.1, Output: 5},
		"claude-opus-9": {Input: 100, CacheWrite1h: 200, CacheRead: 10, Output: 500},
	})
	recs := []UsageRecord{priceRecord(apiNow.Add(-time.Hour), map[string]UsageModel{"claude-opus-9": {Input: 1e6}})}

	if got := APIEquivalent(recs, prices, apiNow, apiNow.Unix()); got.WeekUSD != 100 {
		t.Errorf("claude-opus-9 priced at $%v, want the family named for it ($100)", got.WeekUSD)
	}
}

// A Claude model the table has no rates for is counted at zero and named, never
// guessed at from its predecessor's rates and never dropped: the figure is a
// floor, and the list is what says so. The table lists no family for one of the
// models below, and the other carries something of its own in front of the name,
// which its prefix does not match. A model that is not Claude's at all is not
// this figure's business - its tokens have their own meter - so it is neither
// counted nor listed.
func TestAPIEquivalentCountsAnUnknownFamilyAtZeroAndNamesIt(t *testing.T) {
	t.Parallel()

	recs := []UsageRecord{priceRecord(apiNow.Add(-time.Hour), map[string]UsageModel{
		"claude-opus-9-9":         {Input: 1e6, Output: 1e6},
		"anthropic/claude-opus-5": {Input: 1e6, Output: 1e6},
		"claude-test":             {Input: 1e6},
		"deepseek-flash":          {Input: 1e6, Output: 1e6},
	})}
	got := APIEquivalent(recs, priceTable(testFamily()), apiNow, apiNow.Unix())

	if got.WeekUSD != 2 || got.MonthUSD != 2 {
		t.Errorf("week = $%v, month = $%v, want only the priced model's $2", got.WeekUSD, got.MonthUSD)
	}
	want := []string{"anthropic/claude-opus-5", "claude-opus-9-9"}
	if !reflect.DeepEqual(got.Unpriced, want) {
		t.Errorf("unpriced = %v, want the Claude models the table cannot price (%v)", got.Unpriced, want)
	}
	if strings.Contains(strings.Join(got.Unpriced, ","), "deepseek") {
		t.Errorf("a model that is not Claude's was named as unpriced: %v", got.Unpriced)
	}
}

// A table the reader could not decode, or none at all, prices nothing rather
// than guessing: the caller draws no dollars, which is the honest answer.
func TestAPIEquivalentWithoutATablePricesNothing(t *testing.T) {
	t.Parallel()

	recs := []UsageRecord{priceRecord(apiNow.Add(-time.Hour), map[string]UsageModel{"claude-test": {Input: 1e6}})}
	if got := APIEquivalent(recs, nil, apiNow, apiNow.Unix()); got.WeekUSD != 0 || got.MonthUSD != 0 || len(got.Unpriced) != 0 {
		t.Errorf("a nil table gave %+v, want nothing", got)
	}
	// An empty family map is a table with nothing in it: every Claude model is
	// unpriced, and none of them is priced at a rate nobody wrote down.
	empty := APIEquivalent(recs, priceTable(nil), apiNow, apiNow.Unix())
	if empty.WeekUSD != 0 || !reflect.DeepEqual(empty.Unpriced, []string{"claude-test"}) {
		t.Errorf("an empty table gave %+v, want nothing priced and the model named", empty)
	}
}

// The plan block carries the figures when the usage records are behind it, read
// off the same clock the week is measured at.
func TestClaudePlanReaderJoinsTheAPIEquivalentFigures(t *testing.T) {
	t.Parallel()

	resets := claudePlanNow.Add(48 * time.Hour).Unix()
	path := claudePlanFile(t, planLine(claudePlanNow, 4, 30, resets))
	// A day's worth of Sonnet 5.5 output: 1M tokens at the file's $10.
	tokens := UsageModel{Input: 1e6, Output: 1e6}
	recs := []UsageRecord{priceRecord(claudePlanNow.Add(-time.Hour), map[string]UsageModel{"claude-sonnet-5-5": tokens})}

	var asked time.Time
	p := NewClaudePlanReader(path, 200).
		WithUsage(func(now time.Time) []UsageRecord { asked = now; return recs }).
		Read(claudePlanNow)

	prices, err := EmbeddedClaudePrices()
	if err != nil {
		t.Fatal(err)
	}
	// The wiring: a reader built the way the dashboard builds one prices from the
	// table the binary ships, so the figures above are the file's rates.
	if shipped, err := NewClaudePlanReader(path, 200).prices(); err != nil || shipped != prices {
		t.Errorf("a reader's own price table is %v (%v), want the embedded table", shipped, err)
	}
	want := APIEquivalent(recs, prices, claudePlanNow, resets).WeekUSD
	if p.APIEquivalentWeekUSD == nil || p.APIEquivalentMonthUSD == nil {
		t.Fatalf("the payload carries no API-equivalent figures: %+v", p)
	}
	if *p.APIEquivalentWeekUSD != want || *p.APIEquivalentMonthUSD != want {
		t.Errorf("week = $%v, month = $%v, want $%v for both", *p.APIEquivalentWeekUSD, *p.APIEquivalentMonthUSD, want)
	}
	if len(p.Unpriced) != 0 {
		t.Errorf("unpriced = %v, want none for a model the file prices", p.Unpriced)
	}
	if !asked.Equal(claudePlanNow) {
		t.Errorf("the usage source was asked at %s, want the instant the plan is measured at (%s)", asked, claudePlanNow)
	}
}

// A price table the reader cannot use is named on the block, where records that
// cannot be read are not: a table that will not decode leaves a reading with no
// figures at all, and one past claudePricesMaxAge prices them from rates nobody
// has refreshed. The first would otherwise read as a town that spent nothing,
// and the second as rates read this month.
func TestClaudePlanReaderNamesAPriceTableItCannotUse(t *testing.T) {
	t.Parallel()

	resets := claudePlanNow.Add(48 * time.Hour).Unix()
	path := claudePlanFile(t, planLine(claudePlanNow, 4, 30, resets))
	recs := []UsageRecord{priceRecord(claudePlanNow.Add(-time.Hour),
		map[string]UsageModel{"claude-test": {Input: 1e6, Output: 1e6}})}
	// The fixture's rates price that record at $12 a week, which is the figure
	// the cases below either draw or do not.
	reader := func(table func() (*ClaudePriceTable, error)) *ClaudePlan {
		r := NewClaudePlanReader(path, 200).WithUsage(func(time.Time) []UsageRecord { return recs })
		r.prices = table
		return r.Read(claudePlanNow)
	}

	broken := reader(func() (*ClaudePriceTable, error) { return nil, errors.New("unexpected end of JSON input") })
	if broken.APIEquivalentWeekUSD != nil || broken.APIEquivalentMonthUSD != nil {
		t.Errorf("a table that will not decode priced the town: %+v", broken)
	}
	if !strings.Contains(broken.StateError, "price table") || !strings.Contains(broken.StateError, "unexpected end") {
		t.Errorf("state_error = %q, want the price table and why it could not be read", broken.StateError)
	}

	aged := priceTable(testFamily())
	aged.AsOf = claudePlanNow.Add(-claudePricesMaxAge - 24*time.Hour).Format(time.DateOnly)
	stale := reader(func() (*ClaudePriceTable, error) { return aged, nil })
	// The figures stand as the poll's best answer, and the age beside them is
	// what says how much they are worth.
	if stale.APIEquivalentWeekUSD == nil || *stale.APIEquivalentWeekUSD != 12 {
		t.Errorf("an aged table priced the week at %v, want the $12 it still holds", stale.APIEquivalentWeekUSD)
	}
	if !strings.Contains(stale.StateError, aged.AsOf) {
		t.Errorf("state_error = %q, want the day the rates were read (%s)", stale.StateError, aged.AsOf)
	}

	current := priceTable(testFamily())
	current.AsOf = claudePlanNow.Add(-24 * time.Hour).Format(time.DateOnly)
	fresh := reader(func() (*ClaudePriceTable, error) { return current, nil })
	if fresh.StateError != "" {
		t.Errorf("a table inside the bound reported %q", fresh.StateError)
	}
	if fresh.APIEquivalentWeekUSD == nil || *fresh.APIEquivalentWeekUSD != 12 {
		t.Errorf("a current table priced the week at %v, want $12", fresh.APIEquivalentWeekUSD)
	}
}

// Records that cannot be read leave the figures unset rather than at zero: the
// page draws nothing, where $0.00 would read as a month the town did not spend.
func TestClaudePlanReaderLeavesTheFiguresUnsetWithoutRecords(t *testing.T) {
	t.Parallel()

	resets := claudePlanNow.Add(48 * time.Hour).Unix()
	path := claudePlanFile(t, planLine(claudePlanNow, 4, 30, resets))

	unreadable := NewClaudePlanReader(path, 200).WithUsage(func(time.Time) []UsageRecord { return nil }).Read(claudePlanNow)
	if unreadable.APIEquivalentWeekUSD != nil || unreadable.APIEquivalentMonthUSD != nil {
		t.Errorf("records that could not be read gave %+v, want no figures", unreadable)
	}
	if unreadable.StateError != "" {
		t.Errorf("records that could not be read reported %q, want the reader's own silence", unreadable.StateError)
	}

	// A town whose records directory is there and empty is a town that spent
	// nothing, which is a figure of zero and not the same reading.
	spentNothing := NewClaudePlanReader(path, 200).
		WithUsage(func(time.Time) []UsageRecord { return []UsageRecord{} }).
		Read(claudePlanNow)
	if spentNothing.APIEquivalentWeekUSD == nil || spentNothing.APIEquivalentMonthUSD == nil {
		t.Fatalf("an empty record set gave %+v, want two figures of zero", spentNothing)
	}
	if *spentNothing.APIEquivalentWeekUSD != 0 || *spentNothing.APIEquivalentMonthUSD != 0 {
		t.Errorf("an empty record set priced the town at %v", spentNothing.APIEquivalentWeekUSD)
	}

	// A reader with no usage source at all draws the plan line it always did.
	bare := NewClaudePlanReader(path, 200).Read(claudePlanNow)
	if bare.APIEquivalentWeekUSD != nil || bare.APIEquivalentMonthUSD != nil {
		t.Errorf("a reader with no usage source gave %+v, want no figures", bare)
	}
}

// The embedded file's own families price a live-looking record, so the file and
// the arithmetic are wired to each other and not only to the fixtures.
func TestAPIEquivalentPricesTheTownsModelsFromTheEmbeddedFile(t *testing.T) {
	t.Parallel()

	prices, err := EmbeddedClaudePrices()
	if err != nil {
		t.Fatal(err)
	}
	recs := []UsageRecord{
		priceRecord(apiNow.Add(-time.Hour), map[string]UsageModel{
			"claude-sonnet-5-5": {Input: 1e6, CacheCreation: 1e6, CacheRead: 1e6, Output: 1e6},
			"deepseek-flash":    {Input: 1e6, Output: 1e6},
		}),
	}
	got := APIEquivalent(recs, prices, apiNow, apiNow.Unix())
	sonnet, _ := prices.Family("claude-sonnet-5-5")
	want := roundTo2(sonnet.Input + sonnet.CacheWrite1h + sonnet.CacheRead + sonnet.Output)
	if got.WeekUSD != want {
		t.Errorf("a Sonnet 5.5 record priced at $%v, want the file's summed rates ($%v)", got.WeekUSD, want)
	}
	if len(got.Unpriced) != 0 {
		t.Errorf("unpriced = %v, want none: the file prices the town's models", got.Unpriced)
	}
}
