package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// JobReading is one maintenance job as the job-overdue collector reads it: the
// few fields of a jobs.Job the collector judges. internal/jobs imports this
// package, so the daemon cannot call jobs.List itself: the code that starts the
// daemon sets JobListSource from it and maps each job to a reading.
type JobReading struct {
	Name    string
	State   string
	Outcome string
	// LastRun is the job's last completion, the zero time when none is recorded.
	LastRun time.Time
	Cadence time.Duration
}

// These are the states and the outcome internal/jobs spells, named here for
// the reason JobReading exists.
const (
	jobStateOverdue   = "overdue"
	jobStateNeverRun  = "never_run"
	jobStateNoCadence = "no_cadence"
	jobStateUnknown   = "unknown"
	jobOutcomeFailed  = "failed"
)

// jobOverdueListKey is the one item a list that cannot be read raises, so the
// queue never reads a broken read as a list with no overdue jobs (gt-1ytq9f).
const jobOverdueListKey = "job-overdue:list"

// jobOverdueWakePrefix is the key prefix attention.RuleFor reads as
// RuleJobOverdueWake; the job's name follows it (gt-toiff6, gt-obe5qp).
const jobOverdueWakePrefix = "job-overdue:wake:"

// jobOverdueWakeJobs is the named table of maintenance jobs whose lapsed
// cadence the town cannot wait for: each raises the wake key once it has
// missed two cadences, so the overseer restarts it or finds what stopped it
// (gt-obe5qp).
var jobOverdueWakeJobs = map[string]bool{
	"dolt_gc":     true,
	"dolt_backup": true,
}

// JobListSource lists the town's maintenance jobs at now. It is nil until the
// daemon's start code sets it; the collector then raises nothing.
var JobListSource func(townRoot string, now time.Time) ([]JobReading, error)

// collectJobOverdue raises one item per job jobOverdueRaises admits; its key
// (jobOverdueKey) names the job, so one job has one item however many ticks it
// stays overdue or failed, and a job both overdue and failed raises one item,
// not two. The item clears on the tick after the job runs again and its
// outcome is no longer failed.
// A list that cannot be read raises one item keyed jobOverdueListKey naming
// the failure, and no error, so the item clears on the next tick the list
// reads. With no source wired it raises nothing.
func (s *attentionSources) collectJobOverdue(context.Context) ([]attention.Item, error) {
	if s.jobList == nil {
		return nil, nil
	}
	readings, err := s.jobList(s.now)
	if err != nil {
		return []attention.Item{{
			Key:      jobOverdueListKey,
			Kind:     attention.KindJobOverdue,
			Severity: attention.SeverityLow,
			Summary:  "maintenance job list cannot be read: " + err.Error(),
		}}, nil
	}
	var out []attention.Item
	for _, r := range readings {
		if !jobOverdueRaises(r) {
			continue
		}
		out = append(out, attention.Item{
			Key:      jobOverdueKey(r, s.now),
			Kind:     attention.KindJobOverdue,
			Severity: attention.SeverityLow,
			Summary:  jobOverdueSummary(r, s.now),
		})
	}
	return out, nil
}

// jobOverdueRaises reports whether one reading raises an item: the job is
// overdue, or its newest outcome failed whatever its state (gt-utf4tf). The
// rest are named skips: no_cadence, never_run (a patrol the town has disabled
// stays here), and unknown with no failed outcome.
func jobOverdueRaises(r JobReading) bool {
	if r.Outcome == jobOutcomeFailed {
		return true
	}
	switch r.State {
	case jobStateOverdue:
		return true
	case jobStateNeverRun, jobStateNoCadence, jobStateUnknown:
		return false
	}
	return false
}

// jobOverdueSummary is the item's text: a newest failed run says so and when
// it ran; an overdue job names its last run age and cadence.
func jobOverdueSummary(r JobReading, now time.Time) string {
	if r.Outcome == jobOutcomeFailed {
		if r.LastRun.IsZero() {
			return fmt.Sprintf("maintenance job %s failed: no run time recorded", r.Name)
		}
		return fmt.Sprintf("maintenance job %s failed: last run %s ago", r.Name, parkedAge(now.Sub(r.LastRun)))
	}
	return fmt.Sprintf("maintenance job %s is overdue: last run %s ago, cadence %s",
		r.Name, parkedAge(now.Sub(r.LastRun)), r.Cadence)
}

// jobOverdueKey is the item key one overdue job raises. A job in
// jobOverdueWakeJobs raises the wake key once now is more than twice its
// cadence past its last run; every other job, and a wake job short of that,
// raises the info key. A cadence that says nothing (zero or negative) proves
// no missed cadence, so it stays the info key.
func jobOverdueKey(r JobReading, now time.Time) string {
	if jobOverdueWakeJobs[r.Name] && r.Cadence > 0 && now.Sub(r.LastRun) > 2*r.Cadence {
		return jobOverdueWakePrefix + r.Name
	}
	return "job-overdue:" + r.Name
}

// jobOverdueItem picks the job-overdue items out of the kind's previous items,
// so a failed collector keeps only its own (collectAttention).
func jobOverdueItem(it attention.Item) bool {
	return it.Kind == attention.KindJobOverdue
}
