// Package reconcile judges a maintenance duty by the age of its last good
// result against a freshness limit.
package reconcile

import (
	"errors"
	"time"
)

// ErrNoResult is a reader's answer when the duty has nothing to measure yet:
// no last good result exists.
var ErrNoResult = errors.New("no result")

// Verdict is how a duty's last good result compares with its freshness limit
// and grace.
type Verdict string

const (
	// VerdictOK: the last good result is at or under the limit.
	VerdictOK Verdict = "ok"
	// VerdictDue: the result is past the limit but within the grace, so the
	// duty runs now without raising an alarm.
	VerdictDue Verdict = "due"
	// VerdictOverdue: the result is past the limit plus the grace, or the duty
	// has no result at all: stale enough to alarm.
	VerdictOverdue Verdict = "overdue"
	// VerdictUnknown: the last good result could not be read, or claims a time
	// after now, so its age cannot be judged.
	VerdictUnknown Verdict = "unknown"
)

// Duty is one maintenance duty to judge: its name, and how old its last good
// result may be before the duty is due (Limit) and before it is overdue
// (Limit plus Grace).
type Duty struct {
	Name  string
	Limit time.Duration
	Grace time.Duration
}

// Observation is what Evaluate found for a duty.
type Observation struct {
	Duty     string
	LastGood time.Time
	Age      time.Duration
	Verdict  Verdict
	// Err is the reader's error that made the verdict unknown, or ErrNoResult
	// for a duty with no result.
	Err error
}

// Evaluate judges d from its last good result, read at now, and the reader's
// error. With no read error the age is now minus lastGood: at or under Limit
// is ok, past Limit up to Limit plus Grace is due, and past that is overdue. A
// last good time after now is unknown.
//
// A read error wrapping ErrNoResult means nothing exists yet, so the duty is
// overdue with a zero LastGood and the error kept. Any other read error is
// unknown, never ok: a read that failed is not a fresh result.
func Evaluate(d Duty, now, lastGood time.Time, readErr error) Observation {
	if readErr != nil {
		verdict := VerdictUnknown
		if errors.Is(readErr, ErrNoResult) {
			verdict = VerdictOverdue
		}
		return Observation{Duty: d.Name, Verdict: verdict, Err: readErr}
	}
	age := now.Sub(lastGood)
	if age < 0 {
		return Observation{Duty: d.Name, LastGood: lastGood, Age: age, Verdict: VerdictUnknown}
	}
	obs := Observation{Duty: d.Name, LastGood: lastGood, Age: age}
	switch {
	case age <= d.Limit:
		obs.Verdict = VerdictOK
	case age <= d.Limit+d.Grace:
		obs.Verdict = VerdictDue
	default:
		obs.Verdict = VerdictOverdue
	}
	return obs
}
