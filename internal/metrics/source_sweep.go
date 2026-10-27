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
// The sum is only the hour's when the read shows where the hour starts: the
// oldest stamp in what was read must not be after the cutoff. Two reads fail
// that, and both leave the column nil. A byte-bounded read (daemonLogHourBytes)
// can stop partway through the hour when the daemon wrote more than the bound.
// And the log itself can begin partway through the hour — lumberjack rotated
// it, or the daemon restarted — which a short read cannot tell from an hour
// the daemon logged nothing in until then. What a read like that holds is part
// of the hour, and nothing in it says how much the missing part had, so a
// number would be read as a quiet hour.
type sweepSource struct {
	// tailBytes is how much of the log the read covers; zero means
	// daemonLogHourBytes. Tests set it small so a fixture needs no megabytes.
	tailBytes int64
}

// tailBound is the byte bound the source asks Env.Tail for.
func (s sweepSource) tailBound() int64 {
	if s.tailBytes > 0 {
		return s.tailBytes
	}
	return daemonLogHourBytes
}

// sweepFinishedRe matches a stage's finished line after its "tier_sweep: "
// prefix is trimmed. The rig and tier names are free text, so the pattern is
// anchored on the words around them rather than on a field count.
var sweepFinishedRe = regexp.MustCompile(`^.+?: .+? finished \(exit -?\d+\) in (\S+);`)

func (sweepSource) Group() string { return "sweep" }

func (s sweepSource) Collect(_ context.Context, env Env) (map[string]any, error) {
	cols := map[string]any{"sweep_minutes_1h": nil}
	data, err := env.Tail(daemonLogPath(env.Town), s.tailBound())
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
	// The oldest stamp in the read is where the read begins. A read that
	// begins after the cutoff — a byte bound that cut it off, or a log that
	// starts there after a rotation or a restart — holds part of the hour,
	// and a read with no stamp at all shows nothing. Neither can be told from
	// well inside the hour to be the hour's sum, so the column is nil rather
	// than a number that would read as a quiet hour.
	if oldest.IsZero() || oldest.After(cutoff) {
		return cols, nil
	}
	cols["sweep_minutes_1h"] = swept.Minutes()
	return cols, nil
}
