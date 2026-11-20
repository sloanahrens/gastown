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

// jobStateOverdue is the state internal/jobs gives a job past its cadence and
// grace. It is spelled here for the reason JobReading exists.
const jobStateOverdue = "overdue"

// JobListSource lists the town's maintenance jobs at now. It is nil until the
// daemon's start code sets it; the collector then raises nothing.
var JobListSource func(townRoot string, now time.Time) ([]JobReading, error)

// collectJobOverdue raises one item per job whose state is overdue. The key is
// the job's name, so one job has one item however many ticks it stays overdue,
// and the item clears on the tick after the job runs again. A job list that
// cannot be read returns the source's error, so the previous items stay
// (collectAttention); with no source wired it raises nothing.
func (s *attentionSources) collectJobOverdue(context.Context) ([]attention.Item, error) {
	if s.jobList == nil {
		return nil, nil
	}
	readings, err := s.jobList(s.now)
	if err != nil {
		return nil, fmt.Errorf("reading the maintenance job list: %w", err)
	}
	var out []attention.Item
	for _, r := range readings {
		if r.State != jobStateOverdue {
			continue
		}
		out = append(out, attention.Item{
			Key:      "job-overdue:" + r.Name,
			Kind:     attention.KindJobOverdue,
			Severity: attention.SeverityLow,
			Summary: fmt.Sprintf("maintenance job %s is overdue: last run %s ago, cadence %s",
				r.Name, parkedAge(s.now.Sub(r.LastRun)), r.Cadence),
		})
	}
	return out, nil
}

// jobOverdueItem picks the job-overdue items out of the kind's previous items,
// so a failed collector keeps only its own (collectAttention).
func jobOverdueItem(it attention.Item) bool {
	return it.Kind == attention.KindJobOverdue
}
