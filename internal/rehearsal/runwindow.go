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
// HostLoad reader Collect fills Load from.
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
}

// runWindow releases ids beside a side load, then waits for them to close. It
// returns one Sling per id in ids order and one SideRun per worktree. Ids still
// open at the deadline are not an error. The side load always ends before it
// returns, and Wait and side load errors come back wrapped and joined with what
// the window gathered.
func runWindow(ctx context.Context, o Options, ids []string) ([]Sling, []SideRun, error) {
	deadline := o.Deadline(windowBound)
	var runs []SideRun
	var sideErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runs, sideErr = SideLoad(ctx, o.Worktrees, o.CacheRoot, o.SideRun, o.Now)
	}()
	slings := Release(ctx, ids, releaseInFlight, func(ctx context.Context, id string) error {
		return o.Town.Sling(ctx, id, o.Agent)
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
	return slings, runs, errors.Join(errs...)
}
