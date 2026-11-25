package dashboard

import (
	"testing"
	"time"
)

// Run starts the jobs poller when a reader is wired: a page that connects after
// Run starts reads the job list. The test waits on the read itself, not on the
// page frame, so it stays red when the wiring line in Run goes away while
// pollJobs keeps being tested directly (gt-m1tv32). The short interval is the
// poller's own clock, not a wait in the test: the hub's ticker drives the read
// once a page is watching.
func TestHubRunStartsJobsPoller(t *testing.T) {
	t.Parallel()

	read := make(chan struct{}, 1)
	h := NewHub(Config{
		Jobs: func() *Jobs {
			// Later ticks must not wedge Run's shutdown on a full channel.
			select {
			case read <- struct{}{}:
			default:
			}
			return &Jobs{Rows: []JobRow{{Name: "dolt_gc", State: "ok"}}, Worst: "ok"}
		},
		JobsEvery: time.Millisecond,
	})
	runHub(t, h)

	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	wantSignal(t, read, "Run never started the jobs poller: the wired reader was not read")
}
