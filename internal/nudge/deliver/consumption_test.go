package deliver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// probeAnswering is a consumption probe that returns verdict and err, and
// records the window it was given.
func probeAnswering(verdict tmux.InputConsumption, err error, gotWindow *time.Duration) func(string, time.Duration) (tmux.InputConsumption, error) {
	return func(_ string, window time.Duration) (tmux.InputConsumption, error) {
		if gotWindow != nil {
			*gotWindow = window
		}
		return verdict, err
	}
}

// TestConsumptionWarningForEachVerdict pins what a nudge sender is told for
// each thing the probe can see (gt-eigw, gt-7xnv): a wedged target is named
// with the recovery; an unreadable pane or an undated freeze is UNKNOWN and
// never claims a wedge; a consumed or inconclusive delivery says nothing.
func TestConsumptionWarningForEachVerdict(t *testing.T) {
	t.Parallel()
	const target = "gt-beads-refinery"
	for _, tc := range []struct {
		name    string
		verdict tmux.InputConsumption
		err     error
		want    []string
		notWant []string
	}{
		{name: "wedged", verdict: tmux.InputConsumptionNotConsumed, want: []string{target, "started no turn", "gt-eigw", "40ms", "gt status"}},
		{name: "pane unreadable", err: errors.New("tmux: no such pane"), want: []string{target, "UNKNOWN", "no such pane"}, notWant: []string{"started no turn"}},
		{name: "undated freeze", verdict: tmux.InputConsumptionUndated, want: []string{"UNKNOWN (UNDATED)", "40ms"}, notWant: []string{"started no turn"}},
		{name: "started a turn", verdict: tmux.InputConsumptionStartedTurn},
		{name: "pane changed", verdict: tmux.InputConsumptionPaneChanged},
		{name: "inconclusive", verdict: tmux.InputConsumptionInconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var window time.Duration
			got := ConsumptionWarning(probeAnswering(tc.verdict, tc.err, &window), 40*time.Millisecond, target, ModeImmediate)
			if window != 40*time.Millisecond {
				t.Errorf("probe window = %s, want the given 40ms", window)
			}
			if len(tc.want) == 0 && got != "" {
				t.Errorf("warning %q, want none", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("warning %q lacks %q", got, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("warning %q says %q", got, w)
				}
			}
		})
	}
}

// TestConsumptionWarningLabelsItsMode: the warning names the delivery mode it
// came from, so a wait-idle warning is not mistaken for an immediate one.
func TestConsumptionWarningLabelsItsMode(t *testing.T) {
	t.Parallel()
	got := ConsumptionWarning(probeAnswering(tmux.InputConsumptionNotConsumed, nil, nil), time.Millisecond, "gt-mayor", ModeWaitIdle)
	if !strings.HasPrefix(got, ModeWaitIdle+":") || strings.Contains(got, ModeImmediate+":") {
		t.Errorf("wait-idle warning = %q", got)
	}
}
