package dashboard

import (
	"sort"
	"time"
)

// The Usage panel's payload: what the town spent over the last day, folded out
// of the per-record files the usage scan writes under
// <town>/.runtime/usage/beads. The reader that opens those files lives with the
// town's other readers, internal/cmd/dashboard_usage.go, for the reason the
// Deep review block's does; this file holds the payload's types and the
// arithmetic that turns records into them, so the windows and the sort order
// are testable without a disk.

// The classes a record's own class field carries.
const (
	UsageClassPolecat    = "polecat"
	UsageClassOMReview   = "om-review"
	UsageClassDeepReview = "deep-review"
)

// The kinds a record's own kind field carries.
const (
	UsageKindWork = "work"
	UsageKindOM   = "om"
)

const (
	// usageWindowName is the window AggregateUsage reports and sums over.
	usageWindowName = "24h"
	// usageWindow is usageWindowName as a duration.
	usageWindow = 24 * time.Hour
	// usageSlowWindow is how far back the Over20 list reaches. A bead that
	// took over twenty minutes is worth naming for two days; older than that
	// it is history, not a current drag on the town.
	usageSlowWindow = 48 * time.Hour
	// usageSlowWallS is the wall seconds above which a work record is slow.
	// Twenty minutes is where a bead stops looking like work and starts
	// looking like a stall, and it is what names the list.
	usageSlowWallS = 1200
	// usageTopBeads and usageDeepReviewRows cap the two lists the panel draws
	// as tables.
	usageTopBeads       = 5
	usageDeepReviewRows = 5
)

// UsageModel is one model's share of a record: what the panel adds up per class
// and model.
type UsageModel struct {
	Calls         int64   `json:"calls"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheCreation int64   `json:"cache_creation"`
	CacheRead     int64   `json:"cache_read"`
	EstCostUSD    float64 `json:"est_cost_usd"`
}

// UsageTotals is a record's totals across every model it used.
type UsageTotals struct {
	Calls         int64 `json:"calls"`
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheCreation int64 `json:"cache_creation"`
	CacheRead     int64 `json:"cache_read"`
	// Uncached counts the tokens the run paid full price for: input, output
	// and cache creation. It is what the panel shows for a Claude record,
	// whose cost the scan does not estimate.
	Uncached int64 `json:"uncached"`
}

// UsageSubagents is how much of a record's work ran in subagents rather than on
// the seat's own loop.
type UsageSubagents struct {
	Calls    int64   `json:"calls"`
	Share    float64 `json:"share"`
	Uncached int64   `json:"uncached"`
}

// UsageActivity is what a record's session did, as opposed to what it spent.
type UsageActivity struct {
	ToolCalls      int64 `json:"tool_calls"`
	Edits          int64 `json:"edits"`
	TestRuns       int64 `json:"test_runs"`
	FailedTestRuns int64 `json:"failed_test_runs"`
}

// UsageRecord is one scan file: a bead's run of one kind. Its fields carry the
// scan's JSON names so the reader decodes a file into it directly, and a
// file whose shape is wrong fails the decode rather than arriving half-filled.
type UsageRecord struct {
	Bead     string  `json:"bead"`
	Kind     string  `json:"kind"`
	Class    string  `json:"class"`
	Rig      string  `json:"rig"`
	Seat     *string `json:"seat"`
	Complete bool    `json:"complete"`
	// Start and End are the run's RFC3339 bounds. A record without a parsable
	// End cannot be placed in any window and is counted as Skipped.
	Start      time.Time             `json:"start"`
	End        time.Time             `json:"end"`
	WallS      int64                 `json:"wall_s"`
	EstCostUSD float64               `json:"est_cost_usd"`
	Models     map[string]UsageModel `json:"models"`
	Totals     UsageTotals           `json:"totals"`
	Subagents  UsageSubagents        `json:"subagents"`
	Activity   UsageActivity         `json:"activity"`
}

// UsageClassModelRow is one class and model's share of the window: the panel's
// answer to "where did the day's tokens go".
type UsageClassModelRow struct {
	Class      string  `json:"class"`
	Model      string  `json:"model"`
	Calls      int64   `json:"calls"`
	Uncached   int64   `json:"uncached"`
	EstCostUSD float64 `json:"est_cost_usd"`
}

// UsageDeepReviewRow is one deep review run, as the panel draws it.
type UsageDeepReviewRow struct {
	Bead          string  `json:"bead"`
	End           string  `json:"end"`
	WallS         int64   `json:"wall_s"`
	Uncached      int64   `json:"uncached"`
	SubagentShare float64 `json:"subagent_share"`
}

// UsageTopBead is one record's spend, for the window's most expensive rows.
type UsageTopBead struct {
	Bead       string  `json:"bead"`
	Rig        string  `json:"rig"`
	Class      string  `json:"class"`
	WallS      int64   `json:"wall_s"`
	Uncached   int64   `json:"uncached"`
	EstCostUSD float64 `json:"est_cost_usd"`
}

// UsageOver20Bead is one work record that took over twenty minutes, with what
// its session was doing for that time: a long bead that ran no tests reads
// differently from one that ran them and failed.
type UsageOver20Bead struct {
	Bead           string `json:"bead"`
	Rig            string `json:"rig"`
	WallS          int64  `json:"wall_s"`
	TestRuns       int64  `json:"test_runs"`
	FailedTestRuns int64  `json:"failed_test_runs"`
}

// Usage is the panel's payload.
//
// Unavailable says the read failed, which the pane draws as itself and never as
// an empty page: "nothing ran today" and "the records could not be read" are
// different things to tell the operator (gt-xyzs8).
type Usage struct {
	// AsOf is when the scan that wrote the records last ran, as the pane draws
	// it. The reader sets it; an aggregate does not know it.
	AsOf string `json:"as_of,omitempty"`
	// Window is what every total in the payload covers: the last 24 hours.
	Window         string               `json:"window"`
	ByClassModel   []UsageClassModelRow `json:"by_class_model"`
	DeepReviewRuns []UsageDeepReviewRow `json:"deep_review_runs"`
	TopBeads       []UsageTopBead       `json:"top_beads"`
	Over20         []UsageOver20Bead    `json:"over20"`
	// Unmatched counts the transcript sessions the scan could match to no bead.
	// The reader reads it off unmatched.jsonl.
	Unmatched int `json:"unmatched"`
	// Incomplete counts the records still marked incomplete: runs that never
	// finished, whose totals would understate the day.
	Incomplete int `json:"incomplete"`
	// Skipped counts the records with no end time, which no window can hold.
	Skipped     int  `json:"skipped"`
	Unavailable bool `json:"unavailable,omitempty"`
}

// AggregateUsage folds the records the reader decoded into the payload the page
// draws: totals over the last 24 hours, the last five deep review runs however
// old they are, and the work beads that took over twenty minutes in the last
// week.
//
// Only complete records are counted, since a run still in flight would
// understate the day, and top beads are ranked by uncached tokens: Claude
// records carry no cost estimate, so the estimate cannot order them, while
// DeepSeek's estimate is a rate over those same tokens and so separates the
// records that spent them equally.
func AggregateUsage(recs []UsageRecord, now time.Time, unmatched int) Usage {
	u := Usage{
		Window:         usageWindowName,
		Unmatched:      unmatched,
		ByClassModel:   []UsageClassModelRow{},
		DeepReviewRuns: []UsageDeepReviewRow{},
		TopBeads:       []UsageTopBead{},
		Over20:         []UsageOver20Bead{},
	}
	dayAgo, slowFrom := now.Add(-usageWindow), now.Add(-usageSlowWindow)

	type classModel struct{ class, model string }
	spend := map[classModel]*UsageClassModelRow{}

	for _, r := range recs {
		if r.End.IsZero() {
			u.Skipped++
			continue
		}
		if !r.Complete {
			u.Incomplete++
			continue
		}
		if r.Class == UsageClassDeepReview {
			u.DeepReviewRuns = append(u.DeepReviewRuns, UsageDeepReviewRow{
				Bead:          r.Bead,
				End:           r.End.UTC().Format(time.RFC3339),
				WallS:         r.WallS,
				Uncached:      uncachedTokens(r),
				SubagentShare: r.Subagents.Share,
			})
		}
		// Over20 reaches back further than the totals do, so it is tested
		// against its own window rather than inheriting the day's. A work bead
		// is what the record's own kind says it is; the om reviewer's records
		// are a different kind, and a deep review's run is a work record.
		if r.Kind == UsageKindWork && r.WallS > usageSlowWallS && inWindow(r.End, slowFrom, now) {
			u.Over20 = append(u.Over20, UsageOver20Bead{
				Bead:           r.Bead,
				Rig:            r.Rig,
				WallS:          r.WallS,
				TestRuns:       r.Activity.TestRuns,
				FailedTestRuns: r.Activity.FailedTestRuns,
			})
		}
		if !inWindow(r.End, dayAgo, now) {
			continue
		}
		for model, m := range r.Models {
			key := classModel{r.Class, model}
			row, ok := spend[key]
			if !ok {
				row = &UsageClassModelRow{Class: r.Class, Model: model}
				spend[key] = row
			}
			row.Calls += m.Calls
			row.Uncached += m.Input + m.Output + m.CacheCreation
			row.EstCostUSD += m.EstCostUSD
		}
		u.TopBeads = append(u.TopBeads, UsageTopBead{
			Bead:       r.Bead,
			Rig:        r.Rig,
			Class:      r.Class,
			WallS:      r.WallS,
			Uncached:   uncachedTokens(r),
			EstCostUSD: estCost(r),
		})
	}

	for _, row := range spend {
		u.ByClassModel = append(u.ByClassModel, *row)
	}
	// Every list is ordered for its own reason and breaks ties on the bead,
	// which is unique per record, so the page never reshuffles between polls.
	sort.Slice(u.ByClassModel, func(i, j int) bool {
		a, b := u.ByClassModel[i], u.ByClassModel[j]
		if a.EstCostUSD != b.EstCostUSD {
			return a.EstCostUSD > b.EstCostUSD
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Model < b.Model
	})
	sort.Slice(u.DeepReviewRuns, func(i, j int) bool {
		a, b := u.DeepReviewRuns[i], u.DeepReviewRuns[j]
		if a.End != b.End {
			return a.End > b.End
		}
		return a.Bead < b.Bead
	})
	sort.Slice(u.TopBeads, func(i, j int) bool {
		a, b := u.TopBeads[i], u.TopBeads[j]
		if a.Uncached != b.Uncached {
			return a.Uncached > b.Uncached
		}
		if a.EstCostUSD != b.EstCostUSD {
			return a.EstCostUSD > b.EstCostUSD
		}
		if a.Bead != b.Bead {
			return a.Bead < b.Bead
		}
		return a.Class < b.Class
	})
	sort.Slice(u.Over20, func(i, j int) bool {
		a, b := u.Over20[i], u.Over20[j]
		if a.WallS != b.WallS {
			return a.WallS > b.WallS
		}
		return a.Bead < b.Bead
	})

	u.DeepReviewRuns = truncate(u.DeepReviewRuns, usageDeepReviewRows)
	u.TopBeads = truncate(u.TopBeads, usageTopBeads)
	return u
}

// inWindow reports whether a record ending at end falls in the span that opens
// at from and closes at now. A record ending in the future is out: it has not
// run yet, and the scan writes an end when it has.
func inWindow(end, from, now time.Time) bool {
	return !end.Before(from) && !end.After(now)
}

// uncachedTokens is a record's uncached token count, falling back to the sum
// over its models so a record written before the total existed still ranks.
func uncachedTokens(r UsageRecord) int64 {
	if r.Totals.Uncached != 0 {
		return r.Totals.Uncached
	}
	var n int64
	for _, m := range r.Models {
		n += m.Input + m.Output + m.CacheCreation
	}
	return n
}

// estCost is a record's estimated cost, on the same fallback as uncachedTokens.
func estCost(r UsageRecord) float64 {
	if r.EstCostUSD != 0 {
		return r.EstCostUSD
	}
	var c float64
	for _, m := range r.Models {
		c += m.EstCostUSD
	}
	return c
}

// truncate keeps at most n rows of a list already sorted worst first.
func truncate[T any](rows []T, n int) []T {
	if len(rows) <= n {
		return rows
	}
	return rows[:n]
}
