package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/update"
)

// updateStatusKeep is how long a finished run stays in the feed. A run that
// ended longer ago is history the status command shows, not something the page
// should keep drawing as the reason nothing is being dispatched.
const updateStatusKeep = 24 * time.Hour

// updateStatusReader reads the record one gt update run writes (internal/update)
// for the dashboard feed. It only reads: the dashboard never writes, locks or
// runs the job.
type updateStatusReader struct {
	store *update.Store
}

// newUpdateStatusReader reads the town's update record.
func newUpdateStatusReader(townRoot string) *updateStatusReader {
	return &updateStatusReader{store: &update.Store{Dir: update.Dir(townRoot)}}
}

// read is the feed's update section, or nil when there is none to draw: no run
// has written a record, the record cannot be read or is not this version, or
// the run ended more than updateStatusKeep ago. ElapsedSec and HeartbeatAgeSec
// are measured against now, so a caller passes the clock it reads the section
// with.
func (r *updateStatusReader) read(now time.Time) *dashboard.UpdateStatus {
	st, err := r.store.Read()
	if err != nil {
		return nil
	}
	running := st.Outcome == update.OutcomeRunning
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
		u.EndedAt = st.EndedAt
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

// updateStatusSeconds is d in whole seconds, and 0 for a negative d: a record
// stamped by a clock ahead of the reader's says nothing has elapsed yet, not
// that it elapsed backwards.
func updateStatusSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d / time.Second)
}
