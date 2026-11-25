package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/runnerroll"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// podLandingRead reads the pod's townhealth.json for the roll. A read or parse
// error, or a report with no landing_passes key, is an error with known false.
func podLandingRead(townRoot string, run runnerroll.Runner) func(ctx context.Context) (string, int, bool, error) {
	path := townhealth.Path(townRoot)
	return func(ctx context.Context) (string, int, bool, error) {
		out, err := run(ctx, []string{"cat", "--", path}, nil)
		if err != nil {
			return "", 0, false, fmt.Errorf("reading the daemon's town health in the pod: %w", err)
		}
		r, err := townhealth.Parse(out)
		if err != nil {
			return "", 0, false, fmt.Errorf("reading the daemon's town health in the pod: %w", err)
		}
		if !r.PassesKnown {
			return "", 0, false, errors.New("the daemon's town health has no landing_passes, so the landing passes are unknown")
		}
		return r.LandingPaused, r.LandingPasses, true, nil
	}
}
