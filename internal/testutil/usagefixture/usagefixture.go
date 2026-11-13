// Package usagefixture is the town the usage report tests read: the same usage records and unmatched
// sessions for the report's builder test in internal/usageattrib and its command test in internal/cmd,
// kept in one place so the two cannot drift apart.
package usagefixture

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// Now is the report clock. The report tests read the default one-day window, the day before it.
func Now() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }

// Records is the town's usage records:
//   - gt-a: a polecat work run and its om review, both in the window, 25 and 5 minutes long;
//   - gt-b: a polecat run on DeepSeek, in the window, 10 minutes long;
//   - gt-c: a polecat run still incomplete, in the window: counted as incomplete, left out of the rows;
//   - gt-d: a deep review ending the day before the newest day, in the window;
//   - gt-old: a run a day older than the window: counted nowhere;
//   - gt-z: a run with no end: counted as skipped.
func Records() []dashboard.UsageRecord {
	return []dashboard.UsageRecord{
		record("gt-a", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "2026-11-13T10:00:00Z", 1500, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 4, Input: 100, CacheCreation: 40, Output: 25}}, 10),
		record("gt-a", dashboard.UsageKindOM, dashboard.UsageClassOMReview, "2026-11-13T09:30:00Z", 300, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 2, Input: 50, Output: 10}}, 0),
		record("gt-b", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "2026-11-13T11:00:00Z", 600, true,
			map[string]dashboard.UsageModel{"deepseek-flash": {Calls: 3, Input: 2000, Output: 500, EstCostUSD: 0.0123}}, 500),
		record("gt-c", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "2026-11-13T08:00:00Z", 1800, false,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 9, Input: 999}}, 0),
		record("gt-d", dashboard.UsageKindWork, dashboard.UsageClassDeepReview, "2026-11-12T13:00:00Z", 400, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 1, Input: 20, Output: 5}}, 0),
		record("gt-old", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "2026-11-10T00:00:00Z", 900, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 5, Input: 1000}}, 0),
		record("gt-z", dashboard.UsageKindWork, dashboard.UsageClassPolecat, "", 900, true,
			map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Calls: 7, Input: 7}}, 0),
	}
}

// Unmatched is the last write of each session the scan matched to no bead: two in the window and one
// before it.
func Unmatched() []time.Time {
	return []time.Time{
		mustTime("2026-11-13T05:00:00Z"),
		mustTime("2026-11-13T06:00:00Z"),
		mustTime("2026-11-01T00:00:00Z"),
	}
}

// UnmatchedJSONL is Unmatched as the unmatched.jsonl the scan writes, one session per line.
func UnmatchedJSONL() string {
	var b strings.Builder
	for i, t := range Unmatched() {
		fmt.Fprintf(&b, "{\"seat\":\"gastown/nux\",\"session\":\"s%d\",\"last_write\":%q,\"reason\":\"no run of its seat holds its last write\"}\n",
			i+1, t.Format(time.RFC3339))
	}
	return b.String()
}

// record is one usage record. An empty end is the zero time, a record with no end.
func record(bead, kind, class, end string, wallS int64, complete bool,
	models map[string]dashboard.UsageModel, subUncached int64) dashboard.UsageRecord {
	rec := dashboard.UsageRecord{
		Bead: bead, Kind: kind, Class: class, Rig: "gastown", WallS: wallS, Complete: complete,
		Models:    models,
		Subagents: dashboard.UsageSubagents{Uncached: subUncached},
	}
	if end != "" {
		rec.End = mustTime(end)
	}
	return rec
}

func mustTime(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return ts
}
