// Package usagerecord holds the usage scan's record, the payload of one bead's
// run. It is a leaf package so both the dashboard that draws records and the
// beads client that stores them as rows can name one type: the dashboard's tests
// import the dispatcher, which imports internal/beads, so internal/beads cannot
// import internal/dashboard. The dashboard aliases these names (dashboard.UsageRecord
// and friends), which are the ones its callers use.
package usagerecord

import "time"

// Model is one model's share of a record: the calls and tokens one model spent
// on the run.
type Model struct {
	Calls         int64   `json:"calls"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheCreation int64   `json:"cache_creation"`
	CacheRead     int64   `json:"cache_read"`
	EstCostUSD    float64 `json:"est_cost_usd"`
}

// Totals is a record's totals across every model it used.
type Totals struct {
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

// Subagents is how much of a record's work ran in subagents rather than on the
// seat's own loop.
type Subagents struct {
	Calls    int64   `json:"calls"`
	Share    float64 `json:"share"`
	Uncached int64   `json:"uncached"`
}

// Activity is what a record's session did, as opposed to what it spent.
type Activity struct {
	ToolCalls      int64 `json:"tool_calls"`
	Edits          int64 `json:"edits"`
	TestRuns       int64 `json:"test_runs"`
	FailedTestRuns int64 `json:"failed_test_runs"`
}

// Record is one scan file: a bead's run of one kind. Its fields carry the
// scan's JSON names so the reader decodes a file into it directly, and a file
// whose shape is wrong fails the decode rather than arriving half-filled.
type Record struct {
	Bead  string `json:"bead"`
	Kind  string `json:"kind"`
	Class string `json:"class"`
	Rig   string `json:"rig"`
	// Workspace is the workspace the run spent in, absent from records the
	// scan wrote before it carried one and so read as the empty string then.
	Workspace string  `json:"workspace,omitempty"`
	Seat      *string `json:"seat"`
	Complete  bool    `json:"complete"`
	// Start and End are the run's RFC3339 bounds. A record without a parsable
	// End cannot be placed in any window and is counted as Skipped.
	Start      time.Time        `json:"start"`
	End        time.Time        `json:"end"`
	WallS      int64            `json:"wall_s"`
	EstCostUSD float64          `json:"est_cost_usd"`
	Models     map[string]Model `json:"models"`
	Totals     Totals           `json:"totals"`
	Subagents  Subagents        `json:"subagents"`
	Activity   Activity         `json:"activity"`
}
