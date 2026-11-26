package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// bookQueueShutdownWait bounds shutdown's wait for the deferred-bookkeeping
// queues (gt-yzgory). It is short because a SIGTERM has already drained them
// through the in-flight count a queued job holds (gt-k7glkk): a job still
// running at the bound is left to the landings-file repair at the next start.
const bookQueueShutdownWait = 5 * time.Second

// bookOverdueAlertSource names the escalations a hung bookkeeping job raises.
const bookOverdueAlertSource = "deferred_bookkeeping"

// bookOverdueAlertKey is one hung job's alert identity: per bead, so two beads'
// hung jobs stay two escalations and a repeat for one bead is one (gt-mxfezc).
func bookOverdueAlertKey(beadID string) string {
	return bookOverdueAlertSource + ":overdue:" + beadID
}

// wireBookQueue gives a rig's landing worker deferred bookkeeping (gt-jz0tp6).
// The switch is per rig, so a rig the config does not list keeps the inline
// path with its Lander and Worker untouched. A listed rig's Lander returns
// after the durable writes, and its queue runs the rest, logging a panicked
// job through the daemon's logger (gt-maqaag).
//
// The queue counts as a landing pass in flight from its first queued job until
// its last one returns, so every hold and drain waits for the bookkeeping as
// well as for a pass (gt-k7glkk). Begin adds to the count directly instead of
// calling beginLandingPass, which refuses a slot under a hold: a job added then
// must still keep the hold waiting for it.
func (d *Daemon) wireBookQueue(rig string, cfg *config.LandingWorkerConfig, l *land.Lander, w *landworker.Worker) {
	if !cfg.DeferredBookkeepingFor(rig) {
		return
	}
	l.DeferBookkeeping = true
	books := &landworker.BookQueue{
		Logf:  d.logger.Printf,
		Begin: func() { d.landingPasses.Add(1) },
		End:   d.landingPassEnded,
		// The alert goes out on its own goroutine: gt escalate retries for
		// minutes under load, and the drain this hook runs on is what the rig's
		// holds, upgrade, rebuild and maintenance paths wait for (gt-mxfezc).
		Overdue: func(beadID string, bound time.Duration) {
			go d.escalateAlert(bookOverdueAlertKey(beadID), bookOverdueAlertSource,
				fmt.Sprintf("the bookkeeping job for %s in rig %s has run longer than %s and is still running; the rig's upgrade, rebuild, maintenance and hold paths are waiting on it", beadID, rig, bound))
		},
	}
	w.Books = books
	d.bookQueuesMu.Lock()
	defer d.bookQueuesMu.Unlock()
	if d.bookQueues == nil {
		d.bookQueues = map[*landworker.BookQueue]string{}
	}
	d.bookQueues[books] = rig
}

// waitForBookQueues waits for every rig's bookkeeping queue to drain, with one
// bound shared by all of them, and names the rigs still running a job when the
// bound passes: their bead records are left to the landings-file repair at the
// next start (gt-yzgory).
func (d *Daemon) waitForBookQueues() {
	d.bookQueuesMu.Lock()
	queues := make(map[*landworker.BookQueue]string, len(d.bookQueues))
	for books, rig := range d.bookQueues {
		queues[books] = rig
	}
	d.bookQueuesMu.Unlock()
	if len(queues) == 0 {
		return
	}
	if d.bookQueueWaitFn != nil {
		d.bookQueueWaitFn()
	}
	bound := bookQueueShutdownWait
	if d.bookQueueBound > 0 {
		bound = d.bookQueueBound
	}
	// Wait's error is the bound passing with a job still queued or running, and
	// an idle queue returns nil even then, so the queues can take turns on the
	// one context.
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	var busy []string
	for books, rig := range queues {
		if books.Wait(ctx) != nil {
			busy = append(busy, rig)
		}
	}
	if len(busy) == 0 {
		return
	}
	slices.Sort(busy)
	busy = slices.Compact(busy)
	d.logger.Printf("bookkeeping: %s still running a job after %s; shutting down without them, the landings-file repair at the next start finishes their bead records", strings.Join(busy, ", "), bound)
}
