package steward

import (
	"sort"
	"time"
)

// StuckGrace is how long past its timeout a job may run before it counts as
// stuck: the runner kills at the timeout, and the grace covers the kill and
// the ledger write, so a job that is merely being killed is not reported
// (gt-9bioi.3).
const StuckGrace = time.Minute

// DefaultStatusWindow is the window gt steward status and the health report
// summarize.
const DefaultStatusWindow = time.Hour

// DefaultRecent is how many of the newest jobs gt steward status lists.
const DefaultRecent = 10

// StatsOptions selects what Summarize counts.
type StatsOptions struct {
	// Since is the start of the window: a finished job counts when it ended
	// at or after Since, a running job always counts.
	Since time.Time
	Now   time.Time
	// Timeout is the job timeout; a job running past Timeout+StuckGrace is
	// stuck. Zero is DefaultJobTimeout.
	Timeout time.Duration
	// HardAgent is the preset whose jobs the Pro count reports. Zero is
	// DefaultHardAgent.
	HardAgent string
	// Last is how many of the newest jobs Recent lists. Zero is
	// DefaultRecent; negative lists none.
	Last int
}

// Stats is one window of the job ledger, as gt steward status and the
// townhealth counters report it.
type Stats struct {
	Since time.Time `json:"since"`
	Now   time.Time `json:"now"`
	// Jobs is every job in the window, running or finished.
	Jobs int `json:"jobs"`
	// Shadow is the jobs among them that ran in shadow mode, whose verdicts
	// were comments only (gt-9bioi.4).
	Shadow int `json:"shadow"`
	// Finished is the jobs that ended in the window, Attempted the ones
	// among them that got to try: an interrupted job says nothing about the
	// work, so it is in neither the failure count nor the rate.
	Finished  int `json:"finished"`
	Attempted int `json:"attempted"`
	// Outcomes counts the finished jobs by outcome.
	Outcomes map[Outcome]int `json:"outcomes"`
	// Models counts every job in the window by the preset it ran on.
	Models map[string]int `json:"models"`
	// Pro is the jobs that ran on the hard preset, the usage the overseer
	// is notified of one job at a time. A kind ProExempt runs on a preset of
	// its own choosing, not because a job escalated, so it is in Models but
	// not here (gt-4k3fj.14).
	Pro int `json:"pro"`
	// Broke is the attempted jobs that ended in error or timeout: the
	// steward failing, as opposed to a job whose verdict is fail.
	Broke int `json:"broke"`
	// Broken lists those jobs, oldest first.
	Broken []Job `json:"broken"`
	// Escalated is the jobs that ended by escalating to the overseer.
	Escalated int `json:"escalated"`
	// MedianSec and MaxSec are the durations of the attempted jobs.
	MedianSec float64 `json:"median_seconds"`
	MaxSec    float64 `json:"max_seconds"`
	// Running is every job with no end, oldest first; Stuck the ones past
	// their timeout among them.
	Running []Job `json:"running"`
	Stuck   []Job `json:"stuck"`
	// Recent is the newest jobs, newest first.
	Recent []Job `json:"recent"`
}

// ErrorRate is the share of attempted jobs that broke; 0 when none was
// attempted.
func (s Stats) ErrorRate() float64 {
	if s.Attempted == 0 {
		return 0
	}
	return float64(s.Broke) / float64(s.Attempted)
}

// Summarize reduces ledger rows to one window. rows is the raw ledger: a
// job's start row and end row collapse to its latest.
func Summarize(rows []Job, o StatsOptions) Stats {
	if o.Timeout <= 0 {
		o.Timeout = DefaultJobTimeout
	}
	if o.HardAgent == "" {
		o.HardAgent = DefaultHardAgent
	}
	if o.Last == 0 {
		o.Last = DefaultRecent
	}
	s := Stats{Since: o.Since, Now: o.Now, Outcomes: map[Outcome]int{}, Models: map[string]int{}, Running: []Job{}, Stuck: []Job{}, Broken: []Job{}, Recent: []Job{}}
	var window []Job
	var durations []time.Duration
	for _, j := range latestRows(rows) {
		running := j.Ended.IsZero()
		if !running && j.Ended.Before(o.Since) {
			continue
		}
		window = append(window, j)
		s.Jobs++
		s.Models[j.Model]++
		if j.Mode.Shadow() {
			s.Shadow++
		}
		if j.Model == o.HardAgent && !j.Event.ProExempt() {
			s.Pro++
		}
		if running {
			s.Running = append(s.Running, j)
			if o.Now.Sub(j.Started) > o.Timeout+StuckGrace {
				s.Stuck = append(s.Stuck, j)
			}
			continue
		}
		s.Finished++
		s.Outcomes[j.Outcome]++
		if j.Outcome == OutcomeEscalated {
			s.Escalated++
		}
		if j.Outcome == OutcomeInterrupted {
			continue
		}
		s.Attempted++
		if j.Outcome == OutcomeError || j.Outcome == OutcomeTimeout {
			s.Broke++
			s.Broken = append(s.Broken, j)
		}
		durations = append(durations, j.Ended.Sub(j.Started))
	}
	if len(durations) > 0 {
		sort.Slice(durations, func(i, k int) bool { return durations[i] < durations[k] })
		s.MedianSec = durations[(len(durations)-1)/2].Seconds()
		s.MaxSec = durations[len(durations)-1].Seconds()
	}
	sort.SliceStable(window, func(i, k int) bool { return window[i].Started.After(window[k].Started) })
	if o.Last > 0 {
		if len(window) > o.Last {
			window = window[:o.Last]
		}
		s.Recent = append(s.Recent, window...)
	}
	return s
}
