package land

import (
	"sort"
	"time"
)

// gateRunRotateLimit is the size in bytes at which the gate-run log is rotated: 50 MiB.
const gateRunRotateLimit int64 = 50 << 20

// gateRunRetentionDays is how many days a gate-run month file is kept before it expires.
const gateRunRetentionDays = 90

// gateRunMonthGroup is the rows of one month, under that month's file name, in input order.
type gateRunMonthGroup struct {
	File string
	Rows []GateRun
}

// gateRunRotationDue reports whether a log of size bytes has reached limit and is due to rotate.
func gateRunRotationDue(size, limit int64) bool {
	return size >= limit
}

// gateRunMonthFile names the month file that holds a run recorded at ts: the UTC calendar month,
// as gate-runs-YYYY-MM.jsonl.gz.
func gateRunMonthFile(ts time.Time) string {
	return "gate-runs-" + ts.UTC().Format("2006-01") + ".jsonl.gz"
}

// groupGateRunsByMonth splits rows into one group per month file. Groups are in file-name order,
// which is chronological because the names sort by year then month, and each group keeps its rows
// in input order.
func groupGateRunsByMonth(rows []GateRun) []gateRunMonthGroup {
	var groups []gateRunMonthGroup
	index := make(map[string]int)
	for _, row := range rows {
		file := gateRunMonthFile(row.TS)
		i, ok := index[file]
		if !ok {
			i = len(groups)
			index[file] = i
			groups = append(groups, gateRunMonthGroup{File: file})
		}
		groups[i].Rows = append(groups[i].Rows, row)
	}
	sort.Slice(groups, func(a, b int) bool { return groups[a].File < groups[b].File })
	return groups
}
