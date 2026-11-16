package daemon

// The om_lessons patrol (gt-vdy9do): rework-program lever 3, the lessons loop
// that runs without a bird (docs/design/rework-program.md). daemon.patrols.
// om_lessons.interval names the period between two passes, and each pass calls
// one run seam that builds the week's proposals (internal/omlessons).
//
// The seam returns no proposals until gt-qkn1lv builds them from the review
// records and gt-pawdfa files them.
//
// The ticker is a check cadence, not a run cadence: an in-process ticker resets
// its countdown on every daemon start, so a ticker set to the 168h interval
// would never fire in a town that restarts oftener, and the week would be
// re-run from zero (gt-ima2). Due-ness is decided from the persisted last-run
// record instead, so a restart reads what the town already had and skips it.

import (
	"context"
	"sync"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/omlessons"
)

const (
	// omLessonsPatrol is the patrol's name in daemon.patrols and in
	// daemon.container_off.
	omLessonsPatrol = "om_lessons"

	// omLessonsTickTimeout bounds a whole pass, so a run that reads a large
	// review record frees the patrol's single-flight guard at a known point.
	omLessonsTickTimeout = 10 * time.Minute

	// omLessonsJoinWait bounds how long Run waits for a pass still running
	// after the ticker stopped. A pass left past it finishes its own bounded
	// read, and the next start needs nothing it wrote.
	omLessonsJoinWait = 10 * time.Second
)

// omLessonsInterval returns the period between two passes, from the config the
// loader already judged: a value it refused never reaches the daemon.
func omLessonsInterval(cfg *DaemonPatrolConfig) (time.Duration, error) {
	var c *agentconfig.OmLessonsConfig
	if cfg != nil && cfg.Patrols != nil {
		c = cfg.Patrols.OmLessons
	}
	return c.IntervalDuration()
}

// omLessonsRunFunc is one pass: the proposals built at time at, or the error
// that ended it. The daemon calls it once per tick, and it must return when ctx
// is done, so the tick's bound frees the patrol's single-flight guard.
type omLessonsRunFunc func(ctx context.Context, at time.Time) ([]omlessons.Proposal, error)

// omLessonsRunSkeleton is this build's pass: the run body is a later bead's
// (gt-qkn1lv, gt-pawdfa), so it reads no review records and proposes nothing.
func omLessonsRunSkeleton(context.Context, time.Time) ([]omlessons.Proposal, error) {
	return nil, nil
}

// startOmLessonsTicker starts the patrol's own ticker when the patrol is
// active, and returns a nil channel and stop func when it is not: a town that
// names no key starts nothing. A configured interval the patrol cannot schedule
// starts no ticker either, and says so once, rather than running on a cadence
// the town never asked for.
//
// The ticker fires on shortPatrolCheckTick of the run interval, and each tick
// asks whether the week is due; see startOmLessonsTick.
func (d *Daemon) startOmLessonsTicker() (<-chan time.Time, func()) {
	if !d.isPatrolActive(omLessonsPatrol) {
		return nil, nil
	}
	every, err := omLessonsInterval(d.patrol())
	if err != nil {
		d.logger.Printf("om_lessons patrol not started: %v", err)
		return nil, nil
	}
	newTicker := d.newOmLessonsTicker
	if newTicker == nil {
		newTicker = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	check := shortPatrolCheckTick(every)
	ticks, stop := newTicker(check)
	d.logger.Printf("om_lessons ticker started (check every %v, run every %v)", check, every)
	return ticks, stop
}

// omLessonsRunMemo holds the last completion time each daemon itself recorded,
// so a check in this process does not re-run the week after a save that could
// not create the record (gt-4nqej). It is keyed by the daemon, not the town
// root: a freshly built patrol over the same town must read what the previous
// process persisted, not inherit a completion it never observed.
//
// A daemon's entry is dropped by stopOmLessonsPatrol, which Run defers once a
// ticker exists. A tick driven without Run leaves its entry until the process
// ends, which only a test does.
var omLessonsRunMemo struct {
	mu   sync.Mutex
	seen map[*Daemon]*patrolLastRunMemory
}

// omLessonsLastRun returns this daemon's in-memory completion time, creating it
// on first use.
func (d *Daemon) omLessonsLastRun() *patrolLastRunMemory {
	omLessonsRunMemo.mu.Lock()
	defer omLessonsRunMemo.mu.Unlock()
	if omLessonsRunMemo.seen == nil {
		omLessonsRunMemo.seen = map[*Daemon]*patrolLastRunMemory{}
	}
	mem, ok := omLessonsRunMemo.seen[d]
	if !ok {
		mem = &patrolLastRunMemory{}
		omLessonsRunMemo.seen[d] = mem
	}
	return mem
}

// forgetOmLessonsLastRun drops this daemon's in-memory completion time.
func (d *Daemon) forgetOmLessonsLastRun() {
	omLessonsRunMemo.mu.Lock()
	defer omLessonsRunMemo.mu.Unlock()
	delete(omLessonsRunMemo.seen, d)
}

// startOmLessonsTick runs one pass on its own goroutine when the week's record
// says it is due: a pass reads the week's review records, which does not belong
// on the main select loop. A tick that arrives before the record says a week has
// passed is skipped, and one that arrives while the previous pass still runs is
// skipped and logged, since two passes at once would propose the same window
// twice.
func (d *Daemon) startOmLessonsTick() {
	if d.config == nil || !d.isPatrolActive(omLessonsPatrol) {
		return
	}
	every, err := omLessonsInterval(d.patrol())
	if err != nil {
		d.logger.Printf("om_lessons: not checking: %v", err)
		return
	}
	dec := evaluatePatrolDue(d.config.TownRoot, omLessonsPatrol, d.omLessonsLastRun().last(), d.clk().Now(), every)
	if !dec.due {
		d.logger.Printf("om_lessons: not due — %s", dec.note)
		return
	}
	if !d.omLessonsRunning.CompareAndSwap(false, true) {
		d.logger.Printf("om_lessons: previous pass still running, skipping")
		return
	}
	if dec.warn != "" {
		d.logger.Printf("om_lessons: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("om_lessons: due — %s", dec.note)
	}
	d.omLessonsCycles.Add(1)
	go func() {
		defer d.omLessonsCycles.Done()
		defer d.omLessonsRunning.Store(false)
		d.triggerOmLessons()
	}()
}

// triggerOmLessons runs one pass over the town's own run seam and logs what it
// built, or the error that ended it. The pass records its start time whatever
// the outcome, the "attempted" convention checkpoint_dog and jsonl_git_backup
// follow: a broken pass costs one line per week, not one per check.
func (d *Daemon) triggerOmLessons() {
	if d.config == nil || !d.isPatrolActive(omLessonsPatrol) {
		return
	}
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, omLessonsTickTimeout)
	defer cancel()
	at := d.clk().Now()
	defer d.recordPatrolRun(d.omLessonsLastRun(), omLessonsPatrol, at)
	proposals, err := d.runOmLessonsTick(ctx, at)
	if err != nil {
		d.logger.Printf("om_lessons: %v", err)
		return
	}
	d.logger.Printf("om_lessons: the pass built %d proposal(s)", len(proposals))
}

// runOmLessonsTick runs one pass: it calls the run seam once. A daemon built
// without one runs the skeleton, which proposes nothing.
func (d *Daemon) runOmLessonsTick(ctx context.Context, at time.Time) ([]omlessons.Proposal, error) {
	run := d.omLessonsRun
	if run == nil {
		run = omLessonsRunSkeleton
	}
	return run(ctx, at)
}

// stopOmLessonsPatrol stops the patrol's ticker and joins the pass it may have
// running, bounded, so Run's return leaves no pass reading the town after the
// daemon is gone.
func (d *Daemon) stopOmLessonsPatrol(stop func()) {
	defer d.forgetOmLessonsLastRun()
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		d.omLessonsCycles.Wait()
		close(done)
	}()
	bound := d.omLessonsJoinWait
	if bound <= 0 {
		bound = omLessonsJoinWait
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		d.logger.Printf("om_lessons: a pass is still running %s after the ticker stopped; Run leaves without it", bound)
	}
}
