package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/metrics"
)

// townMetricsSteps is the steps gt metrics carries as subcommands.
func townMetricsSteps() []string {
	return []string{
		metrics.CommandExtract,
		metrics.CommandMango,
		metrics.CommandBacktest,
		metrics.CommandReport,
		metrics.CommandAll,
		metrics.CommandStoryPoints,
		metrics.CommandStoryPointsTrain,
		metrics.CommandStoryPointsAdopt,
		metrics.CommandStoryPointsEval,
	}
}

// TestTownMetricsSubcommandsDispatch holds each subcommand to running the step it
// is named after against the command's own output.
func TestTownMetricsSubcommandsDispatch(t *testing.T) {
	t.Parallel()
	for _, name := range townMetricsSteps() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var gotName string
			var got metrics.Commands
			run := func(_ context.Context, ran string, c metrics.Commands) error {
				gotName, got = ran, c
				return nil
			}
			var out bytes.Buffer
			cmd := newTownMetricsCmd(name, "short", run)
			cmd.SetOut(&out)
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
			if gotName != name {
				t.Errorf("ran %q, want %q", gotName, name)
			}
			if _, err := time.Parse(time.DateOnly, got.Today); err != nil {
				t.Errorf("Today = %q, want a YYYY-MM-DD day", got.Today)
			}
			if got.Out != &out {
				t.Errorf("Out = %v, want the command's output", got.Out)
			}
		})
	}
}

// TestStoryPointsAdoptRevertFlagReachesTheStep: --revert belongs to
// storypoints-adopt alone, and the step it runs sees it.
func TestStoryPointsAdoptRevertFlagReachesTheStep(t *testing.T) {
	t.Parallel()
	revertSeen := false
	run := func(_ context.Context, _ string, c metrics.Commands) error {
		revertSeen = c.StoryPointsRevert
		return nil
	}
	cmd := newTownMetricsCmd(metrics.CommandStoryPointsAdopt, "short", run)
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.Flags().Set("revert", "true"); err != nil {
		t.Fatalf("set --revert: %v", err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !revertSeen {
		t.Error("StoryPointsRevert = false, want the flag passed to the step")
	}
	other := newTownMetricsCmd(metrics.CommandExtract, "short", run)
	if other.Flags().Lookup("revert") != nil {
		t.Error("metrics extract has --revert, want the flag on storypoints-adopt alone")
	}
}

// TestTownMetricsSubcommandsAreRegistered holds gt metrics to carrying each step
// as a subcommand that refuses a positional argument.
func TestTownMetricsSubcommandsAreRegistered(t *testing.T) {
	t.Parallel()
	for _, want := range townMetricsSteps() {
		sub, _, err := metricsCmd.Find([]string{want})
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if sub.Name() != want {
			t.Errorf("found %q, want %q", sub.Name(), want)
		}
		if err := sub.Args(sub, []string{"extra"}); err == nil {
			t.Errorf("%s accepts an argument, want it refused", want)
		}
	}
}
