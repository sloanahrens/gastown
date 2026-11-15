package metrics

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints"
)

// storyPointsCheckedInWeights is what the drift item names when the town runs
// the weights compiled into the binary, the weights its estimator falls back to
// with no runtime file.
const storyPointsCheckedInWeights = "the checked-in weights"

// StoryPointsDriftRead is one read of the drift measure the attention queue
// judges: the measure over the newest extract's landings, and the weights the
// town's estimator runs beside it.
type StoryPointsDriftRead struct {
	// Dir is the extract directory the landings table came from.
	Dir string
	// Drift is the measure over that table's landings: the a priori estimate
	// against the measured polecat time.
	Drift StoryPointsDrift
	// Weights names the weights the estimator runs: storyPointsCheckedInWeights,
	// or the runtime weights file's version.
	Weights string
}

// ReadStoryPointsDrift reads the drift measure from the newest extract at or
// before now: the landings table of now's day directory when it holds one, else
// of the newest day directory that does. A table, day directory or weights file
// that cannot be read is an error naming it: a measure the queue could not take
// is not a town without drift.
func ReadStoryPointsDrift(env Env, now time.Time) (StoryPointsDriftRead, error) {
	switch {
	case env.Town == "":
		return StoryPointsDriftRead{}, errors.New("story points drift: Env.Town is unset")
	case env.ReadFile == nil:
		return StoryPointsDriftRead{}, errors.New("story points drift: Env.ReadFile is unset")
	case now.IsZero():
		return StoryPointsDriftRead{}, errors.New("story points drift: the clock is unset")
	}
	// The day directory is named for the date in the clock's own location, as
	// gt metrics extract names it from the clock it runs on.
	outDir := filepath.Join(env.Town, ".runtime", "metrics", now.Format(time.DateOnly))
	dir, data, err := storyPointsExtract(env, outDir)
	if err != nil {
		return StoryPointsDriftRead{}, err
	}
	table := filepath.Join(dir, landingsName)
	_, rows, err := ReadLandings(bytes.NewReader(data))
	if err != nil {
		return StoryPointsDriftRead{}, fmt.Errorf("%s: %w", table, err)
	}
	drift, err := StoryPointsDriftOver(rows, nil)
	if err != nil {
		return StoryPointsDriftRead{}, fmt.Errorf("%s: %w", table, err)
	}
	weights, err := StoryPointsWeightsVersion(env)
	if err != nil {
		return StoryPointsDriftRead{}, err
	}
	return StoryPointsDriftRead{Dir: dir, Drift: drift, Weights: weights}, nil
}

// StoryPointsWeightsVersion names the story point weights a town's estimator
// runs, for the drift item's text: storyPointsCheckedInWeights when the town has
// adopted none, else the runtime weights file's version, such as "v1".
//
// A runtime file that is present and unreadable, or that storypoints.LoadWeights
// refuses, is an error naming the file: the town adopted weights its estimator
// cannot use, which the drift item's reader has to see rather than read as the
// checked-in name.
func StoryPointsWeightsVersion(env Env) (string, error) {
	path := storypoints.RuntimeWeightsPath(env.Town)
	switch {
	case path == "":
		return "", errors.New("story points weights: Env.Town is unset")
	case env.ReadFile == nil:
		return "", errors.New("story points weights: Env.ReadFile is unset")
	}
	data, err := env.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return storyPointsCheckedInWeights, nil
	case err != nil:
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	w, err := storypoints.LoadWeights(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return w.Version(), nil
}
