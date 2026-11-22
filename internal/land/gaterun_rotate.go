package land

import (
	"sort"
	"time"
)

const gateRunRotateLimit int64 = 50 << 20 // bytes: 50 MiB
// gateRunClockBoundDays is how far past the newest rotated row the retention clock
// may be before the prune refuses to delete anything.
const gateRunClockBoundDays = 400

// gateRunClockBound is gateRunClockBoundDays as a duration.
const gateRunClockBound = gateRunClockBoundDays * 24 * time.Hour

const gateRunRetentionDays = 90 // days a month file is kept

type gateRunMonthGroup struct {
	File string
	Rows []GateRun
}

func gateRunRotationDue(size, limit int64) bool { return size >= limit }

// gateRunMonthFile names the file for the UTC calendar month of ts.
func gateRunMonthFile(ts time.Time) string {
	return "gate-runs-" + ts.UTC().Format("2006-01") + ".jsonl.gz"
}

// groupGateRunsByMonth returns one group per month file in file-name order, rows in input order.
func groupGateRunsByMonth(rows []GateRun) []gateRunMonthGroup {
	sorted := append([]GateRun(nil), rows...)
	sort.SliceStable(sorted, func(a, b int) bool { return gateRunMonthFile(sorted[a].TS) < gateRunMonthFile(sorted[b].TS) })
	var groups []gateRunMonthGroup
	for _, row := range sorted {
		file := gateRunMonthFile(row.TS)
		if n := len(groups); n == 0 || groups[n-1].File != file {
			groups = append(groups, gateRunMonthGroup{File: file})
		}
		groups[len(groups)-1].Rows = append(groups[len(groups)-1].Rows, row)
	}
	return groups
}

// gateRunMonthExpired reports whether name is a month file, named as
// gateRunMonthFile names one, whose month ended more than keepDays days before
// now. A month ends at the first instant of the next UTC month; exactly keepDays
// days after that is not yet expired. Any other name, the current log and the
// rejects file included, is never expired.
func gateRunMonthExpired(name string, now time.Time, keepDays int) bool {
	month, err := time.Parse("gate-runs-2006-01.jsonl.gz", name)
	if err != nil || gateRunMonthQuarantined(name, now) {
		return false
	}
	return now.Sub(month.AddDate(0, 1, 0)) > time.Duration(keepDays)*24*time.Hour
}

// gateRunFirstYear is the earliest year a month file may carry and still be
// retained or aged out; an earlier or far-future year comes from a row whose ts
// was missing or wrong, and is kept for an operator, not deleted (gt-jir41w).
const gateRunFirstYear = 2020

// gateRunMonthQuarantined reports whether name is a month file whose year is
// outside [gateRunFirstYear, the year of now plus one]. Such a file is neither
// expired nor kept by age: the rotation leaves it and names it.
func gateRunMonthQuarantined(name string, now time.Time) bool {
	month, err := time.Parse("gate-runs-2006-01.jsonl.gz", name)
	if err != nil {
		return false
	}
	return month.Year() < gateRunFirstYear || month.Year() > now.UTC().Year()+1
}
