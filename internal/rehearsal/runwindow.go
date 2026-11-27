package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// windowBound is how long a run's load window stays open; releaseInFlight is how
// many slings it runs at once.
const (
	windowBound     = 60 * time.Minute
	releaseInFlight = 3
)

// Options is what a run needs: the town and agent its slings and reads go through,
// the side load's worktrees, GOCACHE root and run function, the clock, the tick
// Wait polls on, and Deadline, which returns a channel that fires once the given
// duration has passed. Drive also takes the record's identity (RunID, Tag,
// ChangeSet), the Tasks it files, the Path it saves the record to, and the
// HostLoad reader Collect fills Load from. CacheBytes and Interventions are the
// values the operator's run command was given.
type Options struct {
	Town      Town
	Agent     string
	Worktrees []string
	CacheRoot string
	SideRun   func(ctx context.Context, dir string, env []string, argv []string) (int, error)
	Now       func() time.Time
	Tick      <-chan time.Time
	Deadline  func(after time.Duration) <-chan time.Time
	RunID     string
	Tag       string
	ChangeSet string
	Tasks     []Task
	Path      string
	HostLoad  io.Reader

	CacheBytes    int64
	Interventions []string
}

// runWindow releases ids beside a side load, then waits for them to close. It
// returns one Sling per id in ids order and one SideRun per worktree. Each sling
// waits for a free seat for o.Agent on its own deadline, so gt sling never refuses
// one past the cap; a fired seat deadline fails the ids never slung and is not a
// window error, while a failed seat read is joined as one. Ids still open at the
// deadline are not an error. The side load always ends before it returns, and
// Wait, seat and side load errors come back wrapped and joined.
func runWindow(ctx context.Context, o Options, ids []string) ([]Sling, []SideRun, error) {
	deadline := o.Deadline(windowBound)
	seatDeadline := o.Deadline(windowBound) // its own channel, so a fired one is not also Wait's
	var runs []SideRun
	var sideErr, seatErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runs, sideErr = SideLoad(ctx, o.Worktrees, o.CacheRoot, o.SideRun, o.Now)
	}()
	slings := Release(ctx, ids, releaseInFlight, func(ctx context.Context, id string) error {
		return o.Town.Sling(ctx, id, o.Agent)
	}, func(ctx context.Context, running func() int) error {
		err := awaitSeat(ctx, func(ctx context.Context) (int, error) {
			return o.Town.FreeSeats(ctx, o.Agent)
		}, running, o.Tick, seatDeadline)
		if err != nil && !errors.Is(err, errSeatDeadline) {
			seatErr = err // Release runs admit on this goroutine and stops after its first error
		}
		return err
	}, o.Now)
	_, waitErr := Wait(ctx, ids, closedState(o.Town), o.Tick, deadline)
	<-done
	var errs []error
	if waitErr != nil {
		errs = append(errs, fmt.Errorf("run window: wait: %w", waitErr))
	}
	if sideErr != nil {
		errs = append(errs, fmt.Errorf("run window: %w", sideErr))
	}
	if seatErr != nil {
		errs = append(errs, fmt.Errorf("run window: seat: %w", seatErr))
	}
	return slings, runs, errors.Join(errs...)
}
