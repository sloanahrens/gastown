package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestClusterDriftCacheKeepsTheAnswerForFifteenMinutes: the first call runs the
// compare; a call 14 minutes after the run returns its answer without running
// again; a call at 15 minutes runs it again, and the answer from that run is the
// one the calls after it return (gt-0cdmgq).
func TestClusterDriftCacheKeepsTheAnswerForFifteenMinutes(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 25, 17, 0, 0, 0, time.UTC)
	var c clusterDriftCache
	runs := 0
	run := func(context.Context) (clusterDriftResult, error) {
		runs++
		return clusterDriftResult{Differing: []string{fmt.Sprintf("run %d", runs)}}, nil
	}

	steps := []struct {
		name     string
		at       time.Time
		wantRuns int
		wantLine string
	}{
		{"first call runs the compare", base, 1, "run 1"},
		{"14 minutes later keeps the answer", base.Add(14 * time.Minute), 1, "run 1"},
		{"15 minutes after the run runs again", base.Add(15 * time.Minute), 2, "run 2"},
		{"14 minutes after that run keeps its answer", base.Add(29 * time.Minute), 2, "run 2"},
	}
	for _, step := range steps {
		res, err := c.Answer(context.Background(), step.at, run)
		if err != nil {
			t.Fatalf("%s: Answer returned error %v, want nil", step.name, err)
		}
		if runs != step.wantRuns {
			t.Errorf("%s: run called %d times, want %d", step.name, runs, step.wantRuns)
		}
		if len(res.Differing) != 1 || res.Differing[0] != step.wantLine {
			t.Errorf("%s: answer = %+v, want the single line %q", step.name, res, step.wantLine)
		}
	}
}

// TestClusterDriftCacheKeepsAFailedRunUntilTheNextRun: a failed run's error is
// returned unchanged by every call within the interval, with no second run; the
// run at the interval that succeeds replaces the failure, and the calls after it
// return the success (gt-0cdmgq).
func TestClusterDriftCacheKeepsAFailedRunUntilTheNextRun(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 25, 17, 0, 0, 0, time.UTC)
	errCompare := errors.New("the compare could not read the live objects")
	var c clusterDriftCache
	runs := 0
	run := func(context.Context) (clusterDriftResult, error) {
		runs++
		if runs == 1 {
			return clusterDriftResult{}, errCompare
		}
		return clusterDriftResult{Differing: []string{"run 2"}}, nil
	}

	steps := []struct {
		name     string
		at       time.Time
		wantRuns int
		wantErr  error
		wantLine string
	}{
		{"the failed run returns its error", base, 1, errCompare, ""},
		{"14 minutes later the error comes back unchanged", base.Add(14 * time.Minute), 1, errCompare, ""},
		{"15 minutes later a successful run replaces it", base.Add(15 * time.Minute), 2, nil, "run 2"},
		{"a call after that keeps the success", base.Add(16 * time.Minute), 2, nil, "run 2"},
	}
	for _, step := range steps {
		res, err := c.Answer(context.Background(), step.at, run)
		if !errors.Is(err, step.wantErr) {
			t.Errorf("%s: error = %v, want %v", step.name, err, step.wantErr)
		}
		if runs != step.wantRuns {
			t.Errorf("%s: run called %d times, want %d", step.name, runs, step.wantRuns)
		}
		switch {
		case step.wantLine == "" && len(res.Differing) != 0:
			t.Errorf("%s: answer = %+v, want the failed run's empty answer", step.name, res)
		case step.wantLine != "" && (len(res.Differing) != 1 || res.Differing[0] != step.wantLine):
			t.Errorf("%s: answer = %+v, want the single line %q", step.name, res, step.wantLine)
		}
	}
}
