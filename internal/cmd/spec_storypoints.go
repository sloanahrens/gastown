package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
)

// The shadow estimate (gt-5d9k2f.14) runs the story-point estimate on each bead
// the dispatcher is about to sling, just before the sling, and changes nothing
// else. It is best effort: an estimate that fails, or runs past the timeout,
// logs one line on the tick and the sling goes ahead as it would have.

// specStoryPointTimeout bounds one shadow estimate as the tick sees it.
const specStoryPointTimeout = 20 * time.Second

// specStoryPointShadow runs the shadow estimate of one bead and returns the one
// line the tick logs, or "" when the estimate wrote its labels and note. The
// estimate runs on its own goroutine, so a timeout returns to the tick without
// waiting for it. The deferred cancel stops the estimate when this returns,
// so the timeout branch leaves it to that. An estimate that is canceled still
// writes its own outcome: the bead gets sp-rung:none with the cancellation as
// the reason, and that write can land after the sling has started.
func specStoryPointShadow(env specDispatchEnv, beadID string) string {
	if env.StoryPointEstimate == nil {
		return fmt.Sprintf("%s: story points shadow: no estimator is wired", beadID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- env.StoryPointEstimate(ctx, beadID) }()
	after := env.After
	if after == nil {
		after = time.After
	}
	select {
	case err := <-done:
		if err == nil {
			return ""
		}
		return fmt.Sprintf("%s: story points shadow: %s", beadID, firstErrLine(err))
	case <-after(specStoryPointTimeout):
		return fmt.Sprintf("%s: story points shadow: no estimate within %s", beadID, specStoryPointTimeout)
	}
}

// specStoryPointEstimate is the production shadow estimate: the estimate
// command's own run, under the tick's context, writing the same labels and note
// gt spec estimate writes, so the shadow report can read the sp: label. A bead
// the shape cannot judge is an error here, so the tick logs why; the bead still
// gets its sp-rung:none note from the command.
func specStoryPointEstimate(townRoot string) func(ctx context.Context, beadID string) error {
	return specStoryPointEstimateFor(
		func(beadID string) specEstimateStore {
			return beads.New(resolveBeadDirFromTownRoot(townRoot, beadID))
		},
		func(beadID string) specEstimateRunner {
			return specEstimateRunner{
				Estimate: estimate.Estimate,
				Deps: func() (estimate.Deps, error) {
					return specEstimateDeps(townRoot, specBeadRig(townRoot, beadID), os.Stderr)
				},
			}
		})
}

// specStoryPointEstimateFor is specStoryPointEstimate with the store and the
// runner given for a bead, so a test drives the labels and the note the shadow
// writes without bd, hm or Jev.
func specStoryPointEstimateFor(newStore func(beadID string) specEstimateStore, runnerFor func(beadID string) specEstimateRunner) func(ctx context.Context, beadID string) error {
	return func(ctx context.Context, beadID string) error {
		outcome, err := specEstimateBead(ctx, beadID, newStore(beadID), specEstimateOpts{}, runnerFor(beadID))
		return specShadowErr(outcome, err)
	}
}

// specShadowErr is the shadow estimate's verdict on one run: the run's own
// error, or the reason a bead the shape could not judge got no estimate. An
// estimate that was written, and a bead whose estimate was already current, are
// nil.
func specShadowErr(outcome specEstimateOutcome, err error) error {
	if err != nil {
		return err
	}
	if outcome.Action == "none" {
		return errors.New("no estimate: " + outcome.Reason)
	}
	return nil
}
