package runnerroll

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// releaseBound bounds the hold's release. The release runs after a signal may
// have canceled the roll's context, so it gets a context of its own.
const releaseBound = time.Minute

// Target is what a roll changes: the gate runner's Deployment, or a cluster's.
type Target interface {
	// Name names the target in the roll's messages, as the noun after "the".
	Name() string
	// Diff returns the lines where the live target differs from its render. A
	// dry run prints them and applies nothing.
	Diff(ctx context.Context) ([]string, error)
	// Apply writes the target's manifests. It does not wait for them.
	Apply(ctx context.Context) error
	// WaitReady waits until the target is ready within the bound its caller gave
	// it.
	WaitReady(ctx context.Context) error
}

// DiffNamer is an optional Target interface: a target whose Diff compares only
// part of what it applies names what it compared, so the roll's summary lines
// never report a wider match than the compare made. A target without it is
// described as "the live <name>".
type DiffNamer interface {
	// DiffMatch says what matched, as a sentence: "the live Deployments match
	// the rendered Deployments".
	DiffMatch() string
	// DiffHeading introduces the difference lines, ending with a colon.
	DiffHeading() string
}

// RollDeps is what a roll runs with: the landing hold and its owner, the two
// waits' reads, the clock they poll on, the reading the hold is stamped with,
// and where the roll prints.
type RollDeps struct {
	Store    HoldStore
	HoldPath string
	Owner    string
	Reason   string
	Landing  func(ctx context.Context) (pause string, passes int, known bool, err error)
	Runs     func(ctx context.Context) ([]WorkflowRun, error)
	Clock    Clock
	Now      func() time.Time
	Out      io.Writer
}

// RollOptions are the roll's flags: the bounds of its waits, whether this is a
// dry run, and the name of each step.
type RollOptions struct {
	LandingWait time.Duration
	RunsWait    time.Duration
	// RolloutWait is the bound Run refuses when it is not positive. The target's
	// WaitReady enforces it, since the target owns that wait.
	RolloutWait time.Duration
	DryRun      bool
	// Step names step n, 1 to 5, for printing.
	Step func(n int) string
}

// Run runs the roll's five steps against t: take the landing hold, wait for the
// landing pass, wait for the gate and post-land runs, apply, wait for the target
// to be ready, and release the hold.
//
// Every exit after the take releases the hold, a canceled context and a failing
// step included; the release runs on a context the cancel does not reach. A
// release that fails is returned unless a step already failed. A dry run takes
// no hold, applies nothing, and prints the steps and t's diff lines.
func Run(ctx context.Context, deps RollDeps, t Target, opts RollOptions) (err error) {
	if err := checkRoll(deps, t, opts); err != nil {
		return err
	}
	if opts.DryRun {
		return dryRun(ctx, deps, t, opts)
	}

	fmt.Fprintf(deps.Out, "step 1/5: %s\n", opts.Step(1))
	hold, err := Take(ctx, deps.Store, deps.Owner, deps.Reason, deps.Now())
	if err != nil {
		return fmt.Errorf("taking the landing hold: %w", err)
	}
	defer func() {
		fmt.Fprintf(deps.Out, "step 5/5: %s\n", opts.Step(5))
		if rerr := releaseHold(ctx, deps, hold); rerr != nil && err == nil {
			err = rerr
		}
	}()

	fmt.Fprintf(deps.Out, "step 2/5: %s\n", opts.Step(2))
	if err := WaitLandingIdle(ctx, deps.Landing, deps.Clock, opts.LandingWait, deps.Out); err != nil {
		return fmt.Errorf("waiting for the landing pass: %w", err)
	}

	fmt.Fprintf(deps.Out, "step 3/5: %s\n", opts.Step(3))
	if err := WaitRunsIdle(ctx, deps.Runs, deps.Clock, opts.RunsWait, deps.Out); err != nil {
		return fmt.Errorf("waiting for the gate and post-land runs: %w", err)
	}

	fmt.Fprintf(deps.Out, "step 4/5: %s\n", opts.Step(4))
	if err := t.Apply(ctx); err != nil {
		return fmt.Errorf("applying the %s: %w", t.Name(), err)
	}
	if err := t.WaitReady(ctx); err != nil {
		return fmt.Errorf("waiting for the %s to be ready: %w", t.Name(), err)
	}
	return nil
}

// dryRun prints the steps and the target's drift lines. It takes no hold, so a
// read that fails needs no release.
func dryRun(ctx context.Context, deps RollDeps, t Target, opts RollOptions) error {
	fmt.Fprintf(deps.Out, "dry run: no hold is taken and nothing is applied; the %s is compared with the live cluster\n", t.Name())
	for n := 1; n <= 5; n++ {
		fmt.Fprintf(deps.Out, "step %d/5: %s\n", n, opts.Step(n))
	}
	diffs, err := t.Diff(ctx)
	if err != nil {
		return fmt.Errorf("comparing the %s with the live cluster: %w", t.Name(), err)
	}
	match := fmt.Sprintf("the live %s matches the rendered manifests", t.Name())
	heading := fmt.Sprintf("the live %s differs from the rendered manifests:", t.Name())
	if named, ok := t.(DiffNamer); ok {
		// A target that names what it compared wins over the roll's default, which
		// would otherwise claim a whole-render match for a partial compare.
		match, heading = named.DiffMatch(), named.DiffHeading()
	}
	if len(diffs) == 0 {
		fmt.Fprintf(deps.Out, "no differences: %s\n", match)
		return nil
	}
	fmt.Fprintln(deps.Out, heading)
	for _, line := range diffs {
		fmt.Fprintln(deps.Out, line)
	}
	return nil
}

// checkRoll refuses a roll that cannot run before it takes the hold: a bound
// that cannot end, a step name or a target that is missing, and, for a real
// roll, every seam its steps read.
func checkRoll(deps RollDeps, t Target, opts RollOptions) error {
	if opts.LandingWait <= 0 || opts.RunsWait <= 0 || opts.RolloutWait <= 0 {
		return fmt.Errorf("the waits must be longer than zero: --landing-wait %s, --runs-wait %s, --rollout-wait %s", opts.LandingWait, opts.RunsWait, opts.RolloutWait)
	}
	if opts.Step == nil {
		return errors.New("the roll has no step names, so it cannot print what it does")
	}
	if t == nil {
		return errors.New("the roll has no target, so there is nothing to apply")
	}
	if deps.Out == nil {
		return errors.New("the roll has nowhere to print, so an operator could not see the steps it took")
	}
	if opts.DryRun {
		return nil
	}
	switch {
	case deps.Store == nil:
		return errors.New("the roll has no landing hold store, so it cannot take the hold")
	case deps.Landing == nil:
		return errors.New("the roll has no landing health read, so it cannot tell a quiet town from a busy one")
	case deps.Runs == nil:
		return errors.New("the roll has no run list, so it cannot tell a finished gate from one in flight")
	case deps.Clock == nil:
		return errors.New("the roll has no clock, so its waits cannot be bounded")
	case deps.Now == nil:
		return errors.New("the roll has no clock reading, so the hold it takes cannot be stamped")
	}
	return nil
}

// releaseHold gives the hold back on a context the roll's cancellation does not
// reach, with its own bound. A failed release reads the hold again and prints
// what to do by whose hold it is now.
func releaseHold(ctx context.Context, deps RollDeps, hold *Hold) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseBound)
	defer cancel()
	err := hold.Release(rctx)
	if err == nil {
		return nil
	}
	printHoldAdvice(ctx, deps, hold)
	return fmt.Errorf("releasing the landing hold: %w", err)
}

// printHoldAdvice reads the landing hold again and prints what is there and what
// to do about it. Only a hold this roll still owns gets a removal command:
// another roll's hold, a file that is not a whole hold, a file already gone and
// an unreadable file are each reported as they were found.
func printHoldAdvice(ctx context.Context, deps RollDeps, hold *Hold) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseBound)
	defer cancel()
	view, err := hold.Inspect(rctx)
	switch {
	case err != nil:
		fmt.Fprintf(deps.Out, "the landing hold could not be read back: %v; it was left in place\n", err)
	case view.State == HoldOwn:
		fmt.Fprintf(deps.Out, "the landing hold is still this roll's; remove it with: rm -f -- %s\n", deps.HoldPath)
	case view.State == HoldOther:
		fmt.Fprintf(deps.Out, "the landing hold is held by owner %q with reason %q; do not remove it; ask that roll\n", view.Owner, view.Reason)
	case view.State == HoldMalformed:
		fmt.Fprintf(deps.Out, "the landing hold is not a whole hold (its first line is %q); it was left in place\n", view.Reason)
	case view.State == HoldMissing:
		fmt.Fprintln(deps.Out, "the landing hold file is already gone")
	default:
		fmt.Fprintln(deps.Out, "the landing hold's state is unknown; it was left in place")
	}
}
