package cmd

import (
	"errors"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/update"
)

const (
	// updateStatusKeep is how long a finished run stays in the feed. A run that
	// ended longer ago is history the status command shows, not something the
	// page should keep drawing as the reason nothing is being dispatched.
	updateStatusKeep = 24 * time.Hour

	// updateStatusStaleAfter is how old a running run's heartbeat may be before
	// the run is taken for dead. The job beats every 15 seconds while it works,
	// and the design's stale alert fires at three minutes without a beat
	// (docs/plans/2026-11-03-gt-update-design.md), so a record older than that
	// is not a run in flight to draw as one.
	updateStatusStaleAfter = 3 * time.Minute
)

// updateStatusReader reads the record one gt update run writes (internal/update)
// for the dashboard feed. It only reads: the dashboard never writes, locks or
// runs the job.
type updateStatusReader struct {
	store *update.Store
	// logf reports a record that cannot be read, which otherwise looks exactly
	// like a town where no run has happened.
	logf func(format string, args ...any)

	mu sync.Mutex
	// failed is the read failure already reported, so a record that stays
	// unreadable reports once rather than on every poll.
	failed string
}

// newUpdateStatusReader reads the town's update record.
func newUpdateStatusReader(townRoot string) *updateStatusReader {
	return &updateStatusReader{
		store: &update.Store{Dir: update.Dir(townRoot)},
		logf:  func(string, ...any) {},
	}
}

// withLogf reports a record that cannot be read. A nil f leaves the reader
// quiet.
func (r *updateStatusReader) withLogf(f func(format string, args ...any)) *updateStatusReader {
	if f == nil {
		f = func(string, ...any) {}
	}
	r.logf = f
	return r
}

// read is the feed's update section, or nil when there is none to draw: no run
// has written a record, the record cannot be read or is not this version, a
// running run's heartbeat is more than updateStatusStaleAfter old, or the run
// ended more than updateStatusKeep ago. ElapsedSec and HeartbeatAgeSec are
// measured against now, so a caller passes the clock it reads the section with.
func (r *updateStatusReader) read(now time.Time) *dashboard.UpdateStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.store.Read()
	if errors.Is(err, update.ErrNoState) {
		return nil
	}
	if err != nil {
		r.reportUnreadable(err)
		return nil
	}
	r.failed = ""
	running := st.Outcome == update.OutcomeRunning
	if running && now.Sub(st.HeartbeatAt) > updateStatusStaleAfter {
		return nil
	}
	if !running && now.Sub(st.EndedAt) > updateStatusKeep {
		return nil
	}
	u := &dashboard.UpdateStatus{
		Running:   running,
		RunID:     st.RunID,
		Source:    st.Source,
		Current:   string(st.Current),
		Outcome:   string(st.Outcome),
		StartedAt: st.StartedAt,
	}
	if running {
		u.ElapsedSec = updateStatusSeconds(now.Sub(st.StartedAt))
		u.HeartbeatAgeSec = updateStatusSeconds(now.Sub(st.HeartbeatAt))
	} else {
		u.EndedAt = &st.EndedAt
		u.ElapsedSec = updateStatusSeconds(st.EndedAt.Sub(st.StartedAt))
	}
	for _, step := range st.Steps {
		u.Steps = append(u.Steps, dashboard.UpdateStep{
			Name:    string(step.Name),
			WillRun: step.WillRun,
			Result:  string(step.Result),
			Detail:  step.Detail,
		})
	}
	return u
}

// reportUnreadable logs a record that cannot be read, once for the failure it
// is: the run rewrites the file at every step, so a record that stays
// unreadable has nothing new to say on the next poll. The caller holds r.mu.
func (r *updateStatusReader) reportUnreadable(err error) {
	msg := err.Error()
	if msg == r.failed {
		return
	}
	r.failed = msg
	r.logf("gt update's state record could not be read, so the feed carries no update section: %v", err)
}

// updateStatusSeconds is d in whole seconds, and 0 for a negative d: a record
// stamped by a clock ahead of the reader's says nothing has elapsed yet, not
// that it elapsed backwards.
func updateStatusSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d / time.Second)
}
