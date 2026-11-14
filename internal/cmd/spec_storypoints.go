package cmd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/storypoints"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
)

// The story-point step (gt-5d9k2f.14, gt-5d9k2f.18) runs the estimate on each
// bead the dispatcher is about to sling, just before the sling. The shadow
// setting only observes, and a failed or timed-out estimate is one logged line.
// The split route reads the same estimate: a bead at or over its trigger is
// labeled for splitting instead of slung, and a bead the route cannot judge is
// slung because the route cannot judge it, never because it looks small.

// specStoryPointTimeout bounds one estimate as the tick sees it.
const specStoryPointTimeout = 20 * time.Second

// specStoryPointSplitNotePrefix begins the comment the split route writes on a
// bead it holds. The annotate seam keys the comment by its prefix, so a later
// tick cannot write a second copy.
const specStoryPointSplitNotePrefix = "SPLIT ROUTE v1"

// errNoStoryPointEstimator reports a tick that has the setting on and no
// estimate wired.
var errNoStoryPointEstimator = errors.New("no estimator is wired")

// specStoryPointAnswer is one estimate run as the tick saw it.
type specStoryPointAnswer struct {
	outcome  specEstimateOutcome
	err      error
	timedOut bool
}

// specStoryPointStepResult is the step's effect on one bead: the lines the tick
// logs, and the skip line when the split route keeps the bead off a seat.
type specStoryPointStepResult struct {
	Notices []string
	Errors  []string
	Hold    string
}

// specStoryPointRun answers one bead's estimate under the tick's timeout. The
// estimate runs on its own goroutine, so a timeout returns to the tick without
// waiting for it. The deferred cancel stops the estimate when this returns, so
// the timeout branch leaves it to that; a context error from the canceled
// estimate is returned unwritten (gt-floskq).
func specStoryPointRun(env specDispatchEnv, beadID string) specStoryPointAnswer {
	if env.StoryPointEstimate == nil {
		return specStoryPointAnswer{err: errNoStoryPointEstimator}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan specStoryPointAnswer, 1)
	go func() {
		outcome, err := env.StoryPointEstimate(ctx, beadID)
		done <- specStoryPointAnswer{outcome: outcome, err: err}
	}()
	after := env.After
	if after == nil {
		after = time.After
	}
	select {
	case answer := <-done:
		return answer
	case <-after(specStoryPointTimeout):
		return specStoryPointAnswer{timedOut: true}
	}
}

// specStoryPointStep runs the estimate for one bead and applies the settings
// that read it. It writes nothing when both settings are off.
func specStoryPointStep(env specDispatchEnv, beadID string, latest specdispatch.Spec) specStoryPointStepResult {
	var res specStoryPointStepResult
	splitOver := env.StoryPointSplitOver
	if splitOver > 0 && latest.HasLabel(specdispatch.SplitLabel) {
		// The ready query already excludes a split bead, so this only catches a
		// label that landed after the board read. The bead keeps the record it
		// has; the route adds none and does not sling it.
		res.Hold = fmt.Sprintf("%s: %salready labeled for splitting", beadID, specdispatch.SkipSplitOver)
		return res
	}
	answer := specStoryPointRun(env, beadID)
	if env.StoryPointShadow {
		if line := specStoryPointShadowLine(beadID, answer); line != "" {
			res.Notices = append(res.Notices, line)
		}
	}
	if splitOver <= 0 {
		return res
	}
	minutes, rung, reason := specSplitVerdict(answer)
	if reason != "" {
		res.Notices = append(res.Notices, fmt.Sprintf("%s: %s%s; slung because the route cannot judge it",
			beadID, specdispatch.SkipSplitOver, reason))
		return res
	}
	if rung == "shape" || minutes < splitOver {
		// A shape-only estimate is the weakest rung and is never acted on; an
		// estimate under the trigger is a bead the seat can take.
		return res
	}
	points := answer.outcome.Points
	if !storypoints.Valid(points) {
		// An unchanged estimate whose bead did not keep an sp: label still has
		// its minutes, so the note names the point those minutes make.
		if p, err := storypoints.FromMinutes(minutes); err == nil {
			points = p
		}
	}
	// The note goes first: the label is what the guard above reads on the next
	// tick, so a label that landed before a failed note would hold the bead
	// without its note. Annotate keys the note by its prefix, so a retry after a
	// label that did not land writes no second copy.
	if err := env.Annotate(beadID, specStoryPointSplitNotePrefix, specSplitNote(answer.outcome, points, minutes, splitOver)); err != nil {
		// A note that did not land leaves the bead unlabeled: it is held here,
		// and the next tick judges it again and writes the note.
		res.Errors = append(res.Errors, fmt.Sprintf("%s: split note: %v", beadID, err))
	} else if err := env.AddLabel(beadID, specdispatch.SplitLabel); err != nil {
		// A label that did not land does not change the estimate: the bead is
		// held here, and the next tick retries the label.
		res.Errors = append(res.Errors, fmt.Sprintf("%s: split label: %v", beadID, err))
	}
	res.Hold = fmt.Sprintf("%s: %sestimated %s min (sp:%d, rung %s), at or over the %s-minute trigger; held for splitting",
		beadID, specdispatch.SkipSplitOver, specMinutesText(minutes), points, rung, specTriggerText(splitOver))
	return res
}

// specStoryPointShadowLine is the one line the shadow setting logs for a
// failed or unusable estimate, or "" when the estimate wrote its labels and
// note.
func specStoryPointShadowLine(beadID string, answer specStoryPointAnswer) string {
	if answer.timedOut {
		return fmt.Sprintf("%s: story points shadow: no estimate within %s", beadID, specStoryPointTimeout)
	}
	if err := specShadowErr(answer.outcome, answer.err); err != nil {
		return fmt.Sprintf("%s: story points shadow: %s", beadID, firstErrLine(err))
	}
	return ""
}

// specSplitVerdict reads the estimate the split route acts on. A bead whose
// estimate could not be made, or whose labels hold no usable value, returns a
// reason: the route cannot judge it, and the caller says so as it slings.
func specSplitVerdict(answer specStoryPointAnswer) (minutes float64, rung, reason string) {
	switch {
	case answer.timedOut:
		return 0, "", fmt.Sprintf("no estimate within %s", specStoryPointTimeout)
	case answer.err != nil:
		return 0, "", firstErrLine(answer.err)
	case answer.outcome.Action == "none":
		if why := strings.TrimSpace(answer.outcome.Reason); why != "" {
			return 0, "", why
		}
		return 0, "", "the estimate records no reason"
	case answer.outcome.Rung == "" || answer.outcome.Rung == "none":
		return 0, "", "the estimate records no rung"
	case answer.outcome.Rung != "shape" && !specSplitRungActs(answer.outcome.Rung):
		return 0, "", fmt.Sprintf("the estimate's rung %q is not one the route reads", answer.outcome.Rung)
	case math.IsNaN(answer.outcome.Minutes) || math.IsInf(answer.outcome.Minutes, 0) || answer.outcome.Minutes <= 0:
		return 0, "", "the estimate records no minutes"
	}
	return answer.outcome.Minutes, answer.outcome.Rung, ""
}

// specSplitRungActs reports whether rung names at least two inputs, the rungs
// the route acts on. The design sets the shape-only rung aside until the shadow
// recompute clears its correlation bar.
func specSplitRungActs(rung string) bool {
	switch rung {
	case "all", "shape-jev", "shape-embed":
		return true
	}
	return false
}

// specSplitNote is the comment the split route leaves on a bead it holds: the
// estimate that put it over the trigger and the past beads hm found nearest.
func specSplitNote(o specEstimateOutcome, points int, minutes, trigger float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: estimated %s minutes (sp:%d, rung %s), at or over the %s-minute split trigger.\n",
		specStoryPointSplitNotePrefix, specMinutesText(minutes), points, o.Rung, specTriggerText(trigger))
	b.WriteString("The dispatcher does not sling this bead; a person or bird splits it into beads a seat can take.\n")
	if o.Neighbors != "" {
		b.WriteString(o.Neighbors)
	} else {
		b.WriteString("nearest beads: not recorded by the estimate")
	}
	return b.String()
}

// specMinutesText is an estimate's minutes as the note and the tick line show
// them: one decimal, the precision the bead's sp-minutes label keeps.
func specMinutesText(m float64) string {
	return strconv.FormatFloat(m, 'f', 1, 64)
}

// specTriggerText is the split trigger as the operator set it, without padding.
func specTriggerText(m float64) string {
	return strconv.FormatFloat(m, 'f', -1, 64)
}

// specStoryPointEstimate is the production estimate: the estimate command's own
// run, under the tick's context, writing the same labels and note gt spec
// estimate writes, so the shadow report can read the sp: label. The outcome
// carries the estimate the split route reads; a bead the shape cannot judge
// comes back with an action of none and the reason, so the tick logs why.
func specStoryPointEstimate(townRoot string) func(ctx context.Context, beadID string) (specEstimateOutcome, error) {
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
func specStoryPointEstimateFor(newStore func(beadID string) specEstimateStore, runnerFor func(beadID string) specEstimateRunner) func(ctx context.Context, beadID string) (specEstimateOutcome, error) {
	return func(ctx context.Context, beadID string) (specEstimateOutcome, error) {
		return specEstimateBead(ctx, beadID, newStore(beadID), specEstimateOpts{}, runnerFor(beadID))
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
