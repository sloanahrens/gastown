// Package usage turns the Claude Code status-line payload into claude_plan
// cost samples: the five-hour and seven-day limit readings the status line
// hook reports, with the ten-minute dedupe the status-line script applies
// before it appends a row. The store behind the rows is an interface, so the
// parse and dedupe rules stay testable without a beads database.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DedupeWindow is how long a reading with unchanged values is held back. The
// status line writes a row when a reading moves, and at most one row per
// window when it does not, so the series stays a record of changes plus a
// heartbeat.
const DedupeWindow = 10 * time.Minute

// ClaudePlan is one claude_plan cost sample: the two limit windows the
// status line reports, and the time the sample was taken (UTC).
type ClaudePlan struct {
	At               time.Time
	FiveHourPct      float64
	FiveHourResetsAt time.Time
	SevenDayPct      float64
	SevenDayResetsAt time.Time
}

// SameValues reports whether two samples carry the same four limit values.
// The sample time does not count: the dedupe compares readings, not clocks.
func (p ClaudePlan) SameValues(o ClaudePlan) bool {
	return p.FiveHourPct == o.FiveHourPct &&
		p.FiveHourResetsAt.Equal(o.FiveHourResetsAt) &&
		p.SevenDayPct == o.SevenDayPct &&
		p.SevenDayResetsAt.Equal(o.SevenDayResetsAt)
}

// Store reads and writes claude_plan rows. The production store is the beads
// cost_samples table; a test supplies an in-memory one.
type Store interface {
	// NewestClaudePlan returns the newest claude_plan row, or nil when the
	// table holds none. A read that fails is an error, never "no row".
	NewestClaudePlan(ctx context.Context) (*ClaudePlan, error)
	// WriteClaudePlan appends one claude_plan row.
	WriteClaudePlan(ctx context.Context, p ClaudePlan) error
}

// statusLine is the part of the status-line payload this package reads. Only
// rate_limits matters here; the rest of the document is ignored.
type statusLine struct {
	RateLimits json.RawMessage `json:"rate_limits"`
}

type rateWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *int64   `json:"resets_at"`
}

type rateLimits struct {
	FiveHour *rateWindow `json:"five_hour"`
	SevenDay *rateWindow `json:"seven_day"`
}

// ParseStatusLine reads one status-line JSON document and returns the plan
// reading it carries, with At left zero for the caller to set. A document with
// no rate_limits (an API-key session, or one before its first response) returns
// ok=false and no error: there is nothing to record. A document that is not
// JSON, or whose rate_limits is missing a window or a field, is an error, so a
// broken payload is never recorded as an absent one.
func ParseStatusLine(data []byte) (plan ClaudePlan, ok bool, err error) {
	var line statusLine
	if err := json.Unmarshal(data, &line); err != nil {
		return ClaudePlan{}, false, fmt.Errorf("status-line JSON: %w", err)
	}
	if len(line.RateLimits) == 0 || string(line.RateLimits) == "null" {
		return ClaudePlan{}, false, nil
	}
	var limits rateLimits
	if err := json.Unmarshal(line.RateLimits, &limits); err != nil {
		return ClaudePlan{}, false, fmt.Errorf("rate_limits: %w", err)
	}
	fh, err := window("five_hour", limits.FiveHour)
	if err != nil {
		return ClaudePlan{}, false, err
	}
	sd, err := window("seven_day", limits.SevenDay)
	if err != nil {
		return ClaudePlan{}, false, err
	}
	return ClaudePlan{
		FiveHourPct:      fh.pct,
		FiveHourResetsAt: fh.resetsAt,
		SevenDayPct:      sd.pct,
		SevenDayResetsAt: sd.resetsAt,
	}, true, nil
}

type parsedWindow struct {
	pct      float64
	resetsAt time.Time
}

// window checks one limit window. A percentage outside 0..100 is refused; the
// comparison is written so a NaN fails it too.
func window(name string, w *rateWindow) (parsedWindow, error) {
	if w == nil {
		return parsedWindow{}, fmt.Errorf("rate_limits.%s is missing", name)
	}
	if w.UsedPercentage == nil {
		return parsedWindow{}, fmt.Errorf("rate_limits.%s.used_percentage is missing", name)
	}
	if w.ResetsAt == nil {
		return parsedWindow{}, fmt.Errorf("rate_limits.%s.resets_at is missing", name)
	}
	p := *w.UsedPercentage
	if !(p >= 0 && p <= 100) {
		return parsedWindow{}, fmt.Errorf("rate_limits.%s.used_percentage %v is outside 0..100", name, p)
	}
	return parsedWindow{pct: p, resetsAt: time.Unix(*w.ResetsAt, 0).UTC()}, nil
}

// ShouldWrite reports whether next is a row to append after newest. It reports
// false when the values match the newest row and that row is under DedupeWindow old.
// A newest row stamped after next (the clock moved back) is not treated as
// recent, so the reading is written.
func ShouldWrite(newest *ClaudePlan, next ClaudePlan) bool {
	if newest == nil {
		return true
	}
	age := next.At.Sub(newest.At)
	recent := age >= 0 && age < DedupeWindow
	return !(recent && newest.SameValues(next))
}

// ErrNoStore is returned by callers that have no store wired. It is exported
// so the command can name the missing piece instead of failing silently.
var ErrNoStore = errors.New("no cost_samples store is wired")

// Record appends next to the store unless ShouldWrite says the newest row
// already covers it. It reports whether a row was written.
func Record(ctx context.Context, store Store, next ClaudePlan) (bool, error) {
	newest, err := store.NewestClaudePlan(ctx)
	if err != nil {
		return false, fmt.Errorf("read newest claude_plan row: %w", err)
	}
	if !ShouldWrite(newest, next) {
		return false, nil
	}
	if err := store.WriteClaudePlan(ctx, next); err != nil {
		return false, fmt.Errorf("write claude_plan row: %w", err)
	}
	return true, nil
}
