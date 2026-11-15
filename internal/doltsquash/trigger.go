// Package doltsquash decides when a rolling Dolt history squash runs.
package doltsquash

import "time"

// Trigger names an input the daily squash check reads for one database.
type Trigger string

// The inputs a daily check compares against its thresholds.
const (
	TriggerBackupBytes    Trigger = "backup bytes"
	TriggerBackupDuration Trigger = "backup duration"
	TriggerQueryP95       Trigger = "query p95"
)

// Reading is one measured value, or the error that kept it from being read.
type Reading[T any] struct {
	Value T
	Err   error
}

// Inputs is what one daily check read for a database. A zero Reading is a
// readable zero, so a caller whose read failed must set Err.
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
	// Unreadable names the inputs whose read failed. An unreadable input is
	// never compared.
	Unreadable []Trigger
}

// Tripped reports whether at least one threshold is over.
func (v Verdict) Tripped() bool { return len(v.TrippedBy) > 0 }

// Evaluate reports which thresholds the readings are over, and which inputs
// could not be read.
func Evaluate(in Inputs, th Thresholds) Verdict {
	var v Verdict
	check := func(t Trigger, err error, over bool) {
		if err != nil {
			// An unreadable input's value is not compared: a stale or zero
			// value must not trip a run for a number nobody read.
			v.Unreadable = append(v.Unreadable, t)
			return
		}
		if over {
			v.TrippedBy = append(v.TrippedBy, t)
		}
	}
	check(TriggerBackupBytes, in.BackupBytes.Err, in.BackupBytes.Value > th.BackupBytes)
	check(TriggerBackupDuration, in.BackupDuration.Err, in.BackupDuration.Value > th.BackupDuration)
	check(TriggerQueryP95, in.QueryP95.Err, in.QueryP95.Value > th.QueryP95)
	return v
}
