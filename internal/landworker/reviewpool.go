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
// Begin, End and Hints are stored here and not yet read: the begin and end
// hooks and the hint check land with the beads that use them.
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

// aheadRun is one review under way: the head it reviews, and the channel
// closed when it ends.
type aheadRun struct {
	head string
	done chan struct{}
}

// Offer starts one review per work while fewer than Max are running, launching
// the goroutines in the order given; Go does not promise that the reviews enter
// the Reviewer in that order. A work that does not fit starts nothing and is
// not queued: the landing offers the list again after a review ends. A work
// whose bead already has a review running is a no-op: no second reviewer and
// no second goroutine. Nothing is started when no Reviewer is set, so every
// submission is a miss and the landing runs om inline.
func (p *ReviewPool) Offer(ctx context.Context, works []land.Work) {
	if p.Reviewer == nil {
		p.logf("ahead review: no reviewer set; %d submissions not reviewed ahead", len(works))
		return
	}
	for _, w := range works {
		p.start(ctx, w)
	}
}

// start registers w as running and launches its review, unless the pool is at
// Max or w's bead already has one running. The cap is checked under p.mu so
// two Offers racing cannot both start a review on the same free slot.
func (p *ReviewPool) start(ctx context.Context, w land.Work) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.free() == 0 {
		return
	}
	if _, ok := p.running[w.BeadID]; ok {
		return
	}
	if p.running == nil {
		p.running = map[string]*aheadRun{}
	}
	run := &aheadRun{head: w.Head, done: make(chan struct{})}
	p.running[w.BeadID] = run
	go p.review(ctx, w, run)
}

// review makes one review under its own child of ctx. The entry leaves the
// map before done closes, so Wait sees the review end. The guard in start
// makes the entry under w.BeadID this run's own.
func (p *ReviewPool) review(ctx context.Context, w land.Work, run *aheadRun) {
	defer func() {
		p.mu.Lock()
		delete(p.running, w.BeadID)
		p.mu.Unlock()
		close(run.done)
	}()
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, err := p.Reviewer.ReviewAhead(rctx, w); err != nil && !errors.Is(err, context.Canceled) {
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

// logf logs through Logf when one is set.
func (p *ReviewPool) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}
