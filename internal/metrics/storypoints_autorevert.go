package metrics

import (
	"fmt"
	"math"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// storyPointsRevertMargin is how much higher than the weights they replaced the
// first-round rmse of an adoption may be on the landings since, before it is
// reverted.
const storyPointsRevertMargin = 0.03

// storyPointsVerdict is the week-after check's decision: revert the adoption, or keep it, or
// wait because the landings since it cannot rank. Why says what it was read from.
type storyPointsVerdict struct {
	Revert, Waits bool
	Why           string
}

// judgeStoryPointsAdoption decides an automatic adoption from the samples that landed after it.
// With storypoints.MinAdoptHoldout first-round samples or more it reverts when the adopted
// weights' first-round rmse of the log ratio is higher than the previous weights' by more than
// storyPointsRevertMargin; with fewer it reverts when the adopted weights' rank correlation over
// all the samples since is under storypoints.AdoptBar. Samples too few to rank wait.
func judgeStoryPointsAdoption(samples []storypoints.Sample, adopted, previous storypoints.Weights, adoptedAt time.Time) (storyPointsVerdict, error) {
	var actual, predicted []float64
	var firstRound int
	var squaresAdopted, squaresPrevious float64
	for _, s := range samples {
		if !s.LandedAt.After(adoptedAt) {
			continue
		}
		adoptedMinutes, err := storyPointsBlend(adopted, s)
		if err != nil {
			return storyPointsVerdict{}, err
		}
		previousMinutes, err := storyPointsBlend(previous, s)
		if err != nil {
			return storyPointsVerdict{}, err
		}
		actual, predicted = append(actual, s.Minutes), append(predicted, adoptedMinutes)
		if s.FirstRound {
			firstRound++
			a, p := math.Log(s.Minutes/adoptedMinutes), math.Log(s.Minutes/previousMinutes)
			squaresAdopted += a * a
			squaresPrevious += p * p
		}
	}
	if firstRound >= storypoints.MinAdoptHoldout {
		rmseAdopted := math.Sqrt(squaresAdopted / float64(firstRound))
		rmsePrevious := math.Sqrt(squaresPrevious / float64(firstRound))
		return storyPointsVerdict{
			Revert: rmseAdopted > rmsePrevious+storyPointsRevertMargin+1e-9,
			Why:    fmt.Sprintf("first-round rmse %.4f against %.4f for the weights it replaced, on %d beads", rmseAdopted, rmsePrevious, firstRound),
		}, nil
	}
	rho, err := Spearman(actual, predicted)
	if err != nil {
		return storyPointsVerdict{Waits: true, Why: fmt.Sprintf("%d first-round beads since it, and no rank correlation (%v)", firstRound, err)}, nil
	}
	return storyPointsVerdict{
		Revert: rho < storypoints.AdoptBar,
		Why:    fmt.Sprintf("rank correlation %.3f on %d beads, %d first round", rho, len(actual), firstRound),
	}, nil
}
