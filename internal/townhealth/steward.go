package townhealth

import (
	"context"
	"fmt"
)

// FieldSteward is the steward job runner's health (gt-9bioi.3).
const FieldSteward = "steward"

// StewardCounters is the steward job ledger over the last Window, published
// in the report so a reader compares them between ticks.
type StewardCounters struct {
	// Window is the span the counts cover, as a Go duration string.
	Window string `json:"window"`
	// Running is the jobs with no end; Stuck the ones past their timeout.
	Running int `json:"running"`
	Stuck   int `json:"stuck"`
	// Jobs is every job in the window; Finished the ones that ended;
	// Attempted the finished ones that got to try (not interrupted).
	Jobs      int `json:"jobs"`
	Finished  int `json:"finished"`
	Attempted int `json:"attempted"`
	// Broke is the attempted jobs that ended in error or timeout.
	Broke int `json:"broke"`
	// Escalated is the jobs that ended by escalating to the overseer.
	Escalated int `json:"escalated"`
	// Pro is the jobs that ran on the hard preset.
	Pro int `json:"pro"`
}

// ErrorRate is the share of attempted jobs that broke; 0 when none ran.
func (c StewardCounters) ErrorRate() float64 {
	if c.Attempted == 0 {
		return 0
	}
	return float64(c.Broke) / float64(c.Attempted)
}

// ErrorRateTripped reports whether the window holds enough attempts, and a
// large enough share of them broke, to count as the steward failing. The
// daemon's alert and the field judge by this one rule.
func (c StewardCounters) ErrorRateTripped(th Thresholds) bool {
	return th.StewardErrorRate > 0 && c.Attempted >= th.StewardMinJobs && c.ErrorRate() > th.StewardErrorRate
}

// StewardSnapshot is what the Steward source answers.
type StewardSnapshot struct {
	// Enabled is false when the town runs no steward; the report then has no
	// steward field at all, because a patrol that is off is not unhealthy.
	Enabled  bool
	Counters StewardCounters
}

// Steward summarizes the steward's job ledger.
type Steward interface {
	Steward(ctx context.Context) (StewardSnapshot, error)
}

// steward judges the job runner: a stuck job is red, an error rate over the
// threshold is degraded. A nil source is a town that has no steward.
func steward(ctx context.Context, in Inputs) (*Field, *StewardCounters) {
	if in.Steward == nil {
		return nil, nil
	}
	snap, err := in.Steward.Steward(ctx)
	if err != nil {
		f := unknown(FieldSteward, "", "", err)
		return &f, nil
	}
	if !snap.Enabled {
		return nil, nil
	}
	c := snap.Counters
	f := Field{Name: FieldSteward, Tag: Recorded, Verdict: Green, Value: fmt.Sprintf("%d running, %d jobs/%s", c.Running, c.Jobs, c.Window)}
	switch {
	case c.Stuck > 0:
		f.Verdict, f.Value = Red, fmt.Sprintf("%d stuck", c.Stuck)
		f.Detail = fmt.Sprintf("%d job(s) running past their timeout", c.Stuck)
		if c.ErrorRateTripped(in.Thresholds) {
			f.Detail += fmt.Sprintf("; %d of %d attempted jobs broke in the last %s", c.Broke, c.Attempted, c.Window)
		}
	case c.ErrorRateTripped(in.Thresholds):
		f.Verdict, f.Value = Degraded, fmt.Sprintf("%d/%d failed", c.Broke, c.Attempted)
		f.Detail = fmt.Sprintf("%d of %d attempted jobs ended in error or timeout in the last %s", c.Broke, c.Attempted, c.Window)
	}
	return &f, &c
}
