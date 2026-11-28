// Package cacheneed sizes each cache from the largest young figure its job recorded plus headroom.
package cacheneed

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/steveyegge/gastown/internal/jobledger"
)

const (
	// statYoungBytes is the bytes in use inside the cache's age floor.
	statYoungBytes       = "young_bytes"
	statClaimBytesBefore = "claim_bytes_before"
	statCapBytes         = "cap_bytes"
	// outcomeOK is the outcome of a run that finished its work.
	outcomeOK = "ok"
	// maxHeadroomPercent bounds headroom at three times the largest figure.
	maxHeadroomPercent = 200
)

// Row is what one job's runs say its cache needs.
type Row struct {
	Job           string
	Runs          int
	MaxYoungBytes int64
	// MaxClaimBytesBefore is nil when no counted run recorded the figure.
	MaxClaimBytesBefore *int64
	// CapBytes is the latest counted run's cap, nil when that run recorded none.
	CapBytes *int64
	// Need is MaxYoungBytes plus headroom percent, rounded up.
	Need              int64
	FirstEnd, LastEnd time.Time
}

// Compute returns one Row per job with an ok run ending at or after since that recorded
// young_bytes, sorted by job name; a headroom outside 0-200 percent is refused.
func Compute(runs []jobledger.Run, since time.Time, headroomPercent int) ([]Row, error) {
	if headroomPercent < 0 || headroomPercent > maxHeadroomPercent {
		return nil, fmt.Errorf("cacheneed: headroom %d%% is outside 0-%d%%", headroomPercent, maxHeadroomPercent)
	}
	rows := map[string]*Row{}
	for _, run := range runs {
		young, hasYoung := run.Stats[statYoungBytes]
		if run.Outcome != outcomeOK || run.EndedAt.Before(since) || !hasYoung {
			continue
		}
		row, ok := rows[run.Job]
		if !ok {
			row = &Row{Job: run.Job, MaxYoungBytes: young, FirstEnd: run.EndedAt, LastEnd: run.EndedAt, CapBytes: statPtr(run, statCapBytes)}
			rows[run.Job] = row
		} else {
			row.MaxYoungBytes = max(row.MaxYoungBytes, young)
			if run.EndedAt.Before(row.FirstEnd) {
				row.FirstEnd = run.EndedAt
			}
			if run.EndedAt.After(row.LastEnd) {
				row.LastEnd = run.EndedAt
				row.CapBytes = statPtr(run, statCapBytes)
			}
		}
		row.Runs++
		if b, has := run.Stats[statClaimBytesBefore]; has && (row.MaxClaimBytesBefore == nil || b > *row.MaxClaimBytesBefore) {
			row.MaxClaimBytesBefore = &b
		}
	}
	out := make([]Row, 0, len(rows))
	for _, job := range slices.Sorted(maps.Keys(rows)) {
		row := rows[job]
		row.Need = row.MaxYoungBytes + (row.MaxYoungBytes*int64(headroomPercent)+99)/100
		out = append(out, *row)
	}
	return out, nil
}

// statPtr is nil when the run did not record the stat, so an absent figure is never zero.
func statPtr(run jobledger.Run, name string) *int64 {
	if b, ok := run.Stats[name]; ok {
		return &b
	}
	return nil
}
