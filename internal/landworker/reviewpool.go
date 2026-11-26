package landworker

import (
	"context"
	"errors"
	"sync"

	"github.com/steveyegge/gastown/internal/land"
)

// AheadReviewer makes the review of one submission ahead of its landing. The
// landing's review-ahead path implements it.
type AheadReviewer interface {
	ReviewAhead(ctx context.Context, w land.Work) (land.AheadOutcome, error)
}

// AheadHints reads what the landing knows about a bead's last landing
// attempt: the head it saw and whether that head was rejected.
type AheadHints interface {
	Hint(bead string) (head string, rejecting bool, err error)
}

// ReviewPool runs reviews ahead of landings, so the serial landing can reuse
// a verdict instead of running om inline. Offer starts the reviews, up to Max
// at once, where a Max below 1 counts as 1; Wait returns when none is running.
//
// Begin, when set, reports whether a review may start: false means a landing
// hold or drain is on; End, when set, is called once by every review that
// began, before that review leaves the pool. Hints is stored here and not yet
// read: the hint check lands with the bead that uses it.
type ReviewPool struct {
	Reviewer AheadReviewer
	Hints    AheadHints
	Max      int
	Begin    func() bool
	End      func()
	Logf     func(string, ...any)

	mu      sync.Mutex
	running map[string]*aheadRun
}

// aheadRun is one review under way: the head it reviews, the cancel that stops
// it, and the channel closed when it ends. canceled records that an Offer at a
// later head stopped it, so a repeat Offer of that head logs nothing more.
type aheadRun struct {
	head     string
	cancel   context.CancelFunc
	canceled bool
	done     chan struct{}
}

// Offer starts one review per work while fewer than Max are running, launching
// the goroutines in the order given; Go does not promise that the reviews enter
// the Reviewer in that order. A work that does not fit starts nothing and is
// not queued: the landing offers the list again after a review ends. A work
// whose bead already has a review running at its head is a no-op: no second
// reviewer and no second goroutine; at another head that review is canceled and
// nothing is started in its place until the pool is offered the list again.
// Nothing is started when no Reviewer is set, so every
// submission is a miss and the landing runs om inline.
//
// A refused Begin means a landing hold or drain is on (gt-vb9qod): the call
// starts nothing more and asks no further Begin, while a work at a head its
// seat has replaced still has its stale review canceled.
func (p *ReviewPool) Offer(ctx context.Context, works []land.Work) {
	if p.Reviewer == nil {
		p.logf("ahead review: no reviewer set; %d submissions not reviewed ahead", len(works))
		return
	}
	starts := true
	for _, w := range works {
		if !p.start(ctx, w, starts) {
			starts = false
		}
	}
}

// start handles one work. A bead running at another head is reviewed at a head
// its seat has replaced, so that review is canceled (om ends with its process
// group) and the new head is left for the next Offer (gt-sor91y); the running
// check comes before the cap, so a full pool still cancels a stale review, and
// it comes before a hold, so a hold never leaves a stale review running. With
// starts false the call only reaches that cancel and begins nothing else.
//
// A review certain to start asks Begin, when set, with p.mu held: no other
// Offer can take the slot in between, so a true Begin never outlives the review
// that owes its End. Begin false means a landing hold or drain is on (gt-vb9qod):
// start returns false with nothing started and no End owed. Everything is
// decided under p.mu so two Offers racing cannot both start a review on the same
// free slot.
func (p *ReviewPool) start(ctx context.Context, w land.Work, starts bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if run, ok := p.running[w.BeadID]; ok {
		if run.head == w.Head || run.canceled {
			return true
		}
		run.canceled = true
		run.cancel()
		p.logf("ahead review of %s at %s: canceled, the bead is now at %s", w.BeadID, run.head, w.Head)
		return true
	}
	if !starts || p.free() == 0 {
		return true
	}
	if p.Begin != nil && !p.Begin() {
		return false
	}
	if p.running == nil {
		p.running = map[string]*aheadRun{}
	}
	rctx, cancel := context.WithCancel(ctx)
	run := &aheadRun{head: w.Head, cancel: cancel, done: make(chan struct{})}
	p.running[w.BeadID] = run
	go p.review(rctx, w, run)
	return true
}

// review makes one review under the child of ctx that start made. End runs
// before the entry leaves the map and before done closes, so a Wait released
// by the empty map already sees the review's End. The guard in start makes the
// entry under w.BeadID this run's own.
func (p *ReviewPool) review(ctx context.Context, w land.Work, run *aheadRun) {
	defer func() {
		p.end()
		p.mu.Lock()
		delete(p.running, w.BeadID)
		p.mu.Unlock()
		run.cancel()
		close(run.done)
	}()
	if _, err := p.Reviewer.ReviewAhead(ctx, w); err != nil && !errors.Is(err, context.Canceled) {
		p.logf("ahead review of %s at %s: %v", w.BeadID, w.Head, err)
	}
}

// Wait returns when no review is running. It waits on every review that is
// running when it looks, then looks again, so a review offered while it
// waits is waited for too.
func (p *ReviewPool) Wait() {
	for {
		p.mu.Lock()
		pending := make([]chan struct{}, 0, len(p.running))
		for _, run := range p.running {
			pending = append(pending, run.done)
		}
		p.mu.Unlock()
		if len(pending) == 0 {
			return
		}
		for _, done := range pending {
			<-done
		}
	}
}

// free returns how many more reviews may start: Max, or 1 when Max is below 1,
// less the reviews running, and never below 0. The caller holds p.mu.
func (p *ReviewPool) free() int {
	max := p.Max
	if max < 1 {
		max = 1
	}
	n := max - len(p.running)
	if n < 0 {
		return 0
	}
	return n
}

// end calls End when one is set, so a pool built without the hook stays
// valid.
func (p *ReviewPool) end() {
	if p.End != nil {
		p.End()
	}
}

// logf logs through Logf when one is set.
func (p *ReviewPool) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}
