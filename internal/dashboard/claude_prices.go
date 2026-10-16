package dashboard

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Claude plan line's API-equivalent dollars: what the Claude tokens the town
// actually burned would have cost at Anthropic's list prices, so the operator
// can read the flat $200 plan beside the metered bill it stands in for and see
// the saving (gt-d8sem). The rates live in claude_prices.json rather than in
// code because they are data about the world that ages: the file carries the
// date it was written and where it was read from, and a test refuses a file
// older than claudePricesMaxAge.
//
// The tokens come from the usage scan's records, the same ones the Usage panel
// folds - nothing here reads a transcript.

// claudePricesMaxAge is how long the price file may stand before a reader is
// told it is out of date, which the plan reader reports on the block it draws:
// the age reaches the operator, who is the one who can re-read the list prices.
// Anthropic revises list prices a few times a year, so a quarter is long enough
// that refresh is rare and short enough that the figures stay near the truth.
const claudePricesMaxAge = 120 * 24 * time.Hour

// claudeAPIWeek is the span the week's figure covers: the seven days the plan's
// weekly limit closes on, which is the "this week" the plan line's own
// percentage is measured over.
const claudeAPIWeek = 7 * 24 * time.Hour

// claudeModelName is the word every Claude model id carries, wherever the scan
// read it. The table's families are keyed by the ids the reader expects, so a
// Claude model whose key carries something else of its own - a vendor prefix,
// say - matches no family and is counted at zero: this is what keeps that model
// in the named list rather than out of the figure entirely.
const claudeModelName = "claude"

//go:embed claude_prices.json
var claudePricesJSON []byte

// ClaudePriceFamily is one model family's list rates, in USD per million tokens.
//
// A price file written from Anthropic's table carries the input and output rates
// and the cache rates that follow from them: a 1-hour cache write is the input
// rate twice over, and a cache read a tenth of it. The scan's records count
// every cache write together, without the TTL they were written for, so every
// one of them is priced at the 1-hour rate - the dearer of the two, which keeps
// the figure a ceiling rather than a flattering undercount.
type ClaudePriceFamily struct {
	Input        float64 `json:"input"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	CacheRead    float64 `json:"cache_read"`
	Output       float64 `json:"output"`
}

// ClaudePriceTable is claude_prices.json decoded: the rates, the day they were
// read, and where they were read from.
type ClaudePriceTable struct {
	// AsOf is the day the rates were taken from Source, as YYYY-MM-DD, and
	// Source the page they were taken from.
	AsOf   string `json:"as_of"`
	Source string `json:"source"`
	// Unit is what the four rates are per. apiModelCost divides by a million, so
	// a file that said anything else would be silently wrong; the test pins it.
	// Prefix is the model ids that are Claude's at all: a model without it is
	// not this figure's business and is left out rather than listed as
	// unpriced, since the town's DeepSeek tokens have their own meter.
	Unit   string `json:"unit"`
	Prefix string `json:"prefix"`
	// Families is keyed by model id prefix, so one entry prices a family across
	// the versions of it that share its rates.
	Families map[string]ClaudePriceFamily `json:"families"`
}

// EmbeddedClaudePrices is the price table compiled into the binary, decoded
// once. It fails only for a file this package ships broken, which the tests
// catch; a reader handed an error leaves the figures unset rather than pricing
// the town at nothing.
var EmbeddedClaudePrices = sync.OnceValues(func() (*ClaudePriceTable, error) {
	var t ClaudePriceTable
	if err := json.Unmarshal(claudePricesJSON, &t); err != nil {
		return nil, err
	}
	return &t, nil
})

// Claude is whether a model id belongs to the Claude API at all: the ids the
// table lists its families under. A Claude model outside that prefix is Claude's
// too, and the caller names it in Unpriced rather than dropping it.
func (t *ClaudePriceTable) Claude(model string) bool {
	return t.Prefix != "" && strings.HasPrefix(model, t.Prefix)
}

// Aged is how long the rates have stood at now, and whether that is past
// claudePricesMaxAge. A table dated with something that is not a date is aged
// as well: nobody can read how old its rates are, which is the same news as
// rates nobody has refreshed.
func (t *ClaudePriceTable) Aged(now time.Time) (time.Duration, bool) {
	asOf, err := time.Parse(time.DateOnly, t.AsOf)
	if err != nil {
		return 0, true
	}
	age := now.Sub(asOf)
	return age, age > claudePricesMaxAge
}

// Family is the rates for a model id's family, by the longest key the id starts
// with: "claude-opus-5-5" is priced as itself rather than as the whole Opus 5
// generation, whose earlier versions bill differently. The second return is
// false for a Claude model the table has no rates for, which is what the caller
// counts at zero and names in its unpriced list.
func (t *ClaudePriceTable) Family(model string) (ClaudePriceFamily, bool) {
	best, found := "", false
	for key := range t.Families {
		if len(key) <= len(best) || !strings.HasPrefix(model, key) {
			continue
		}
		best, found = key, true
	}
	if !found {
		return ClaudePriceFamily{}, false
	}
	return t.Families[best], true
}

// ClaudeAPIEquivalent is what the town's Claude tokens were worth at list
// prices, over the plan's week and the current billing month. The plan block
// carries the three as its own fields; nothing serializes this type itself.
type ClaudeAPIEquivalent struct {
	WeekUSD  float64
	MonthUSD float64
	// Unpriced names the Claude models in those windows the price table cannot
	// price, whether it lists no family for them or their id carries something
	// the table's own prefix does not match. Their tokens are counted at zero
	// rather than guessed at, so a reading with a non-empty Unpriced is a floor
	// and says so.
	Unpriced []string
}

// APIEquivalent prices the records' Claude tokens over the plan's current weekly
// window and the calendar month now falls in.
//
// The week is the seven days closing at weeklyResetsAt, the instant the status
// line reports for the plan's own weekly limit: it is the same "this week" the
// line's percentage is measured over, so the two figures on the hover are two
// readings of one span. A reset instant the plan does not have, one the clock
// has already passed, or one more than a week ahead, falls back to the seven
// days to now. The month is the billing month, whose first day is where an
// invoice's count starts.
//
// Only complete records count, as in AggregateUsage: a run still in flight would
// understate both windows.
func APIEquivalent(recs []UsageRecord, prices *ClaudePriceTable, now time.Time, weeklyResetsAt int64) ClaudeAPIEquivalent {
	var eq ClaudeAPIEquivalent
	if prices == nil {
		return eq
	}
	// The reset instant dates the week only while the window it opens has opened
	// and lies inside the past week: an instant further ahead leaves no day in
	// the past to measure, and one the clock has passed opens the window more
	// than seven days back, so the figure would cover more than the week it
	// claims to read. Zero is the absent instant and falls back the same way.
	weekFrom := now.Add(-claudeAPIWeek)
	if weeks := time.Unix(weeklyResetsAt, 0).Add(-claudeAPIWeek); weeks.After(weekFrom) && !weeks.After(now) {
		weekFrom = weeks
	}
	// A month's first day, in the clock the caller reads now off: the boundary
	// is one instant, and every End below is compared as one.
	monthFrom := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	unpriced := map[string]bool{}
	for _, r := range recs {
		if r.End.IsZero() || !r.Complete {
			continue
		}
		week, month := inWindow(r.End, weekFrom, now), inWindow(r.End, monthFrom, now)
		if !week && !month {
			continue
		}
		for model, m := range r.Models {
			if !prices.Claude(model) {
				// A model id that reaches the scan carrying something of its own
				// ("anthropic/claude-opus-5-5") is outside the table's prefix and
				// so has no rates this figure can use. Naming it is what keeps it
				// from vanishing: counted at zero, it is part of the floor.
				if strings.Contains(strings.ToLower(model), claudeModelName) {
					unpriced[model] = true
				}
				continue
			}
			f, ok := prices.Family(model)
			if !ok {
				unpriced[model] = true
				continue
			}
			c := apiModelCost(m, f)
			if week {
				eq.WeekUSD += c
			}
			if month {
				eq.MonthUSD += c
			}
		}
	}
	eq.WeekUSD, eq.MonthUSD = roundTo2(eq.WeekUSD), roundTo2(eq.MonthUSD)
	for model := range unpriced {
		eq.Unpriced = append(eq.Unpriced, model)
	}
	sort.Strings(eq.Unpriced)
	return eq
}

// apiModelCost is one model's tokens at one family's rates.
func apiModelCost(m UsageModel, f ClaudePriceFamily) float64 {
	return (float64(m.Input)*f.Input +
		float64(m.CacheCreation)*f.CacheWrite1h +
		float64(m.CacheRead)*f.CacheRead +
		float64(m.Output)*f.Output) / 1e6
}
