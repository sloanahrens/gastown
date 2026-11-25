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

// Run starts no jobs poller for a hub with no jobs reader, and Run still returns
// when ctx ends. The feed poller is the control: its read shows the hub's workers
// ran with a page watching, so a jobs source left unread is one no worker was
// started for rather than one the hub had no chance to read. Deleting the nil
// check in Run starts the jobs worker, which calls the missing reader and panics
// (gt-b0dk1a).
func TestHubRunNilJobsStartsNoPoller(t *testing.T) {
	t.Parallel()

	feedRead := make(chan struct{}, 1)
	h := NewHub(Config{
		Feed: func() []Entry {
			// Later ticks must not wedge Run's shutdown on a full channel.
			select {
			case feedRead <- struct{}{}:
			default:
			}
			return []Entry{{Text: "jobs pane has no reader"}}
		},
		FeedEvery: time.Millisecond,
	})
	runHub(t, h)

	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	wantSignal(t, feedRead, "the hub's pollers never ran: the control feed was not read")

	st := h.State()
	if st.Jobs != nil {
		t.Fatalf("a hub with no jobs reader was sent a job list: %+v", st.Jobs)
	}
	if r, ok := st.Reads[SourceJobs]; ok {
		t.Fatalf("a hub with no jobs reader read the jobs source: %+v", r)
	}
}
