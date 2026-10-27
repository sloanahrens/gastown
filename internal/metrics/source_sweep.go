package metrics

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/daemonlog"
)

// The sweep group: the minutes the tier sweep spent testing in the hour the
// snapshot covers, so the record's seat counts are read against the test load
// that competes with them for the host's cores (gt-b9j3o.3).
//
// The reading is the daemon's own stage line, written once per stage:
//
//	tier_sweep: <rig>: <tier> finished (exit <n>) in <duration>; last lines:
//
// The durations of the finished lines stamped in the last hour are summed and
// reported in minutes:
//
//	sweep_minutes_1h  the stage durations stamped in the last hour, summed
//
// A stage counts when its line is written, so a sweep still running is the
// stages that have finished so far: the column is the hour's completed test
// time. A read that failed, or a finished line whose duration does not parse,
// leaves the column nil rather than 0, as the queue source's log columns do: a
// log that could not be read is not an hour with no sweep in it.
//
// The read is byte-bounded (daemonLogHourBytes), so an hour in which the daemon
// wrote more than the bound starts partway through: the tail's oldest stamp
// then falls after the cutoff, and what it holds is the tail's sweep time, not
// the hour's. That too leaves the column nil, because a sum over part of the
// hour reads as a quiet hour.
type sweepSource struct{}

// sweepFinishedRe matches a stage's finished line after its "tier_sweep: "
// prefix is trimmed. The rig and tier names are free text, so the pattern is
// anchored on the words around them rather than on a field count.
var sweepFinishedRe = regexp.MustCompile(`^.+?: .+? finished \(exit -?\d+\) in (\S+);`)

func (sweepSource) Group() string { return "sweep" }

func (sweepSource) Collect(_ context.Context, env Env) (map[string]any, error) {
	cols := map[string]any{"sweep_minutes_1h": nil}
	data, err := env.Tail(daemonLogPath(env.Town), daemonLogHourBytes)
	if err != nil {
		return cols, nil
	}
	now := env.Now()
	cutoff := now.Add(-time.Hour)
	p := &daemonlog.Parser{Loc: now.Location()}
	var swept time.Duration
	var oldest time.Time
	for _, raw := range strings.Split(string(data), "\n") {
		at, text := p.Parse(raw)
		if !at.IsZero() && (oldest.IsZero() || at.Before(oldest)) {
			oldest = at
		}
		m := sweepFinishedRe.FindStringSubmatch(strings.TrimPrefix(text, "tier_sweep: "))
		if m == nil {
			continue
		}
		// A line with no stamp above it cannot be placed in the hour; the
		// fragment a byte-bounded tail cuts off the first line arrives this way.
		if at.IsZero() || at.Before(cutoff) {
			continue
		}
		d, err := time.ParseDuration(m[1])
		if err != nil {
			return cols, nil
		}
		swept += d
	}
	// The read filled its bound, so it may have stopped after the hour began.
	// The oldest stamp says where the tail starts: unless it reaches the
	// cutoff, the hour is not covered and the sum is short by however much the
	// cut-off part held. Nothing in the tail can tell how much that is, so the
	// column is nil rather than a number that would be read as the hour's.
	if int64(len(data)) >= daemonLogHourBytes && (oldest.IsZero() || oldest.After(cutoff)) {
		return cols, nil
	}
	cols["sweep_minutes_1h"] = swept.Minutes()
	return cols, nil
}
