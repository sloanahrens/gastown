// Package spendcap decides whether a workspace has reached its daily spend cap,
// from the usage records the town keeps.
//
// Evaluate is pure: it reads no clock and does no I/O, so the day it judges is
// the UTC day of the now its caller passes. Its callers are the dispatcher
// gate, the abuse limiter and the customer dashboard (plan rows S5 and O6,
// gt-6eejf); none calls it yet.
//
// The spend lags what a workspace has run: a usage record is written when a
// bead ends, so work still in flight is uncounted and the cap can be overshot
// by its cost. A per-sandbox cap is a later bead.
package spendcap

import (
	"math"
	"time"
)

// Level is where a workspace's spend for a day stands against its cap.
type Level string

const (
	// Under is a spend below the cap: a new dispatch is allowed.
	Under Level = "under"
	// At is a spend that has reached the cap: a new dispatch stops.
	At Level = "at"
	// Over is a spend above the cap: a new dispatch stops.
	Over Level = "over"
	// Unknown is a spend or cap that is not a number, so the day cannot be
	// judged: a new dispatch stops, as it does at At and Over.
	Unknown Level = "unknown"
)

// epsilon is the tolerance the cap comparison carries, because a sum of float
// costs can land a few billionths to either side of the cap it equals.
const epsilon = 1e-9

// Record is one usage record: the workspace a run spent in, when it ended, and
// what it cost.
type Record struct {
	Workspace string
	EndedAt   time.Time
	CostUSD   float64
}

// Cap is a workspace's daily spend cap in US dollars.
type Cap struct {
	// Workspace names the workspace the cap applies to.
	Workspace string
	// DailyUSD is the cap in US dollars for one UTC day, and zero or less means
	// no cap is set.
	DailyUSD float64
}

// Decision is Evaluate's answer: the level the day's spend stands at, what the
// day cost, and how much of the cap is left.
type Decision struct {
	// Level is Under, At or Over.
	Level Level
	// Spent is the day's spend for the workspace in US dollars.
	Spent float64
	// Remaining is the cap minus Spent, never below zero, and zero when no cap
	// is set.
	Remaining float64
}

// Evaluate decides where cap's workspace stands, counting only the records that
// ended in the UTC day of now for that workspace.
func Evaluate(cap Cap, spend []Record, now time.Time) Decision {
	local := now.UTC()
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	var spent float64
	for _, r := range spend {
		// The range is [start, end): a record ending on a UTC midnight belongs
		// to the day it opens.
		if r.Workspace != cap.Workspace || r.EndedAt.Before(start) || !r.EndedAt.Before(end) {
			continue
		}
		spent += r.CostUSD
	}
	// A NaN compares false against every number, so it would fall through to
	// Under: a corrupt cost or cap is refused instead of allowed.
	if math.IsNaN(spent) || math.IsNaN(cap.DailyUSD) {
		return Decision{Level: Unknown, Spent: spent}
	}
	if cap.DailyUSD <= 0 {
		return Decision{Level: Under, Spent: spent}
	}
	d := Decision{Level: Under, Spent: spent}
	switch {
	case spent > cap.DailyUSD+epsilon:
		d.Level = Over
	case spent >= cap.DailyUSD-epsilon:
		d.Level = At
	}
	if left := cap.DailyUSD - spent; left > 0 {
		d.Remaining = left
	}
	return d
}
