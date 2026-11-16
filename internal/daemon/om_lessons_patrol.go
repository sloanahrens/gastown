package daemon

// The om_lessons patrol (gt-vdy9do): rework-program lever 3, the lessons loop
// that runs without a bird (docs/design/rework-program.md). Its own ticker runs
// on the cadence daemon.patrols.om_lessons names, and each tick calls one run
// seam that builds the week's proposals (internal/omlessons).
//
// This bead is the skeleton: the seam returns no proposals until gt-qkn1lv
// builds them from the review records and gt-pawdfa files them. The cadence
// lives in the process, so a restart restarts it; gt-tcg1er's weekly last-run
// record is what keeps a restart from rerunning a pass the town already had.

import (
	"context"
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
	ticks, stop := newTicker(every)
	d.logger.Printf("om_lessons ticker started (interval %v)", every)
	return ticks, stop
}

// startOmLessonsTick runs one pass on its own goroutine: a pass reads the week's
// review records, which does not belong on the main select loop. A tick that
// arrives while the previous pass still runs is skipped and logged, since two
// passes at once would propose the same window twice.
func (d *Daemon) startOmLessonsTick() {
	if !d.omLessonsRunning.CompareAndSwap(false, true) {
		d.logger.Printf("om_lessons: previous pass still running, skipping")
		return
	}
	d.omLessonsCycles.Add(1)
	go func() {
		defer d.omLessonsCycles.Done()
		defer d.omLessonsRunning.Store(false)
		d.triggerOmLessons()
	}()
}

// triggerOmLessons runs one pass over the town's own run seam and logs what it
// built, or the error that ended it.
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
	proposals, err := d.runOmLessonsTick(ctx, d.clk().Now())
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
