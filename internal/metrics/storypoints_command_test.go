package metrics

import (
	"strings"
	"testing"
)

// TestMetricsStoryPointsReadsTodaysLandings: the storypoints step reads the
// landings table the extract wrote and prints the shadow report over it. The
// fixture's beads carry no story-point labels, so the report says it has none.
func TestMetricsStoryPointsReadsTodaysLandings(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	town.out.Reset()
	code, err := town.run(t, CommandStoryPoints)
	if err != nil {
		t.Fatalf("storypoints: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	want := "story points: 0 estimated landings with a measured time; the report needs 60\n"
	if got := town.out.String(); got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

// TestMetricsStoryPointsNeedsTheExtract: with no extract run for the day there is
// no landings table to read, and the step says which step to run first.
func TestMetricsStoryPointsNeedsTheExtract(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	code, err := town.run(t, CommandStoryPoints)
	if err == nil {
		t.Fatalf("storypoints returned no error with no landings table (code %d)", code)
	}
	if !strings.Contains(err.Error(), CommandExtract) {
		t.Errorf("err = %v, want it to name the extract step", err)
	}
}
