// Package doltsquash decides when a rolling Dolt history squash runs.
package doltsquash

import (
	"fmt"
	"strings"
	"time"
)

// Trigger names an input the daily squash check reads for one database.
type Trigger string

// The inputs a daily check compares against its thresholds.
const (
	TriggerBackupBytes    Trigger = "backup bytes"
	TriggerBackupDuration Trigger = "backup duration"
	TriggerQueryP95       Trigger = "query p95"
)

// Reading is one measured value, or the error that kept it from being read;
// only a value Read built with no error is compared.
type Reading[T any] struct {
	// Value is the number the read yielded. Evaluate compares it only when
	// Read built the Reading and Err is nil.
	Value T
	// Err is why the read failed. Evaluate never compares a Reading that
	// carries one, so a failed read stays unreadable even beside a stale value.
	Err error
	// read marks a Reading Read built. A caller who never marked a value is
	// reported unreadable rather than compared as a zero.
	read bool
}

// Read returns a Reading of a value that was measured.
func Read[T any](v T) Reading[T] { return Reading[T]{Value: v, read: true} }

// Failed returns an unreadable Reading that carries err.
func Failed[T any](err error) Reading[T] { return Reading[T]{Err: err} }

// readable reports whether Evaluate may compare the value: Read built the
// Reading and its read reported no error.
func (r Reading[T]) readable() bool { return r.read && r.Err == nil }

// Inputs is what one daily check read for a database. An input the caller did
// not build with Read, or one carrying an error, is named in
// Verdict.Unreadable and never compared; Decide includes those names in the
// reason it returns.
type Inputs struct {
	BackupBytes    Reading[int64]
	BackupDuration Reading[time.Duration]
	QueryP95       Reading[time.Duration]
}

// Thresholds are the trigger limits. A reading trips when it is over its
// limit, not when it equals it.
type Thresholds struct {
	BackupBytes    int64
	BackupDuration time.Duration
	QueryP95       time.Duration
}

// Verdict is the trigger check's decision for one database.
type Verdict struct {
	// TrippedBy names the triggers whose reading is over its threshold, in
	// check order.
	TrippedBy []Trigger
	// Unreadable names the inputs whose value was never read, whether the read
	// failed or the caller left the Reading zero. An unreadable input is never
	// compared.
	Unreadable []Trigger
}

// Tripped reports whether at least one threshold is over.
func (v Verdict) Tripped() bool { return len(v.TrippedBy) > 0 }

// Evaluate reports which thresholds the readings are over, and which inputs
// could not be read.
func Evaluate(in Inputs, th Thresholds) Verdict {
	var v Verdict
	check := func(t Trigger, readable, over bool) {
		if !readable {
			// A value nobody read — the read failed, or the caller never
			// recorded one — is named, not compared: a stale or zero number
			// must not trip a run, and a zero must not pass as a quiet answer.
			v.Unreadable = append(v.Unreadable, t)
			return
		}
		if over {
			v.TrippedBy = append(v.TrippedBy, t)
		}
	}
	check(TriggerBackupBytes, in.BackupBytes.readable(), in.BackupBytes.Value > th.BackupBytes)
	check(TriggerBackupDuration, in.BackupDuration.readable(), in.BackupDuration.Value > th.BackupDuration)
	check(TriggerQueryP95, in.QueryP95.readable(), in.QueryP95.Value > th.QueryP95)
	return v
}

// Settings are the values a daily check decides against: the trigger
// thresholds and the keep window. The squash block of daemon.json fills them
// (D8 of docs/plans/2026-11-15-dolt-squash-plan.md).
type Settings struct {
	Thresholds Thresholds
	// KeepDays is how many days of history stay unsquashed. Decide refuses a
	// value below 1: a zero window would put the cut at the check's clock and
	// a negative one after it, so every commit would be squashed.
	// A window longer than a time.Duration holds is refused too.
	KeepDays int
}

// RunInterval is the shortest gap between two squash runs: a check inside it is
// not due even when a threshold trips (design step 1, "no run in the last 24
// hours").
const RunInterval = 24 * time.Hour

// maxKeepDays is the longest keep window a time.Duration holds. Above it the
// window wraps, and the wrapped cut can land after now, where every commit is
// older than the cut.
const maxKeepDays = int(time.Duration(1<<63-1) / (24 * time.Hour))

// Decision is a daily check's answer for one database.
type Decision struct {
	// Due reports whether a squash run may start now.
	Due bool
	// Cut is the instant a commit must predate to be squashed: the check's
	// clock minus the keep window.
	Cut time.Time
	// Verdict names the thresholds the readings are over and the inputs that
	// could not be read.
	Verdict Verdict
	// Reason is the log line: why the check is due, or what held it back,
	// naming every unreadable input.
	Reason string
}

// Decide reports whether a squash run is due at now for one database, with the
// cut instant it would squash to. lastRun is when the previous run started,
// zero when none is recorded; a run started inside RunInterval holds the check
// back even when a threshold trips. A stamp after now is read as a backwards
// clock and holds nothing back, so a clock set back costs one extra run rather
// than a schedule that never comes due. A KeepDays below 1 is an error, not a
// decision.
// A KeepDays above maxKeepDays is an error too: the window would wrap and the
// cut would no longer be then minus the window.
func Decide(now, lastRun time.Time, in Inputs, cfg Settings) (Decision, error) {
	if cfg.KeepDays < 1 {
		return Decision{}, fmt.Errorf("doltsquash: keep_days %d is below 1, so the cut would leave no history", cfg.KeepDays)
	}
	if cfg.KeepDays > maxKeepDays {
		return Decision{}, fmt.Errorf("doltsquash: keep_days %d is above %d, the longest window the cut can reach", cfg.KeepDays, maxKeepDays)
	}
	v := Evaluate(in, cfg.Thresholds)
	d := Decision{
		Cut:     now.Add(-time.Duration(cfg.KeepDays) * 24 * time.Hour),
		Verdict: v,
	}
	switch {
	case !v.Tripped():
		d.Reason = "no threshold tripped"
	case !lastRun.IsZero() && !lastRun.After(now) && now.Sub(lastRun) < RunInterval:
		d.Reason = fmt.Sprintf("tripped (%s), but the last run started %s ago, inside the %s gate",
			joinTriggers(v.TrippedBy), now.Sub(lastRun).Truncate(time.Minute), hours(RunInterval))
	default:
		d.Due = true
		d.Reason = "due: " + joinTriggers(v.TrippedBy) + " over the limit"
	}
	if len(v.Unreadable) > 0 {
		d.Reason += "; unreadable: " + joinTriggers(v.Unreadable)
	}
	return d, nil
}

func joinTriggers(ts []Trigger) string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
}

// hours renders a duration in whole hours for a log line.
func hours(d time.Duration) string {
	return fmt.Sprintf("%gh", d.Hours())
}
