package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// timedHeartbeatDaemon is a daemon whose clock is fake and whose heartbeat
// steps are replaced by fakes that advance it by took[name] and record the
// order they ran in.
func timedHeartbeatDaemon(t *testing.T, took map[string]time.Duration) (*Daemon, *lockedBuffer, *[]string) {
	t.Helper()
	d, logs := signalTestDaemon(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	d.clock = clk
	var order []string
	d.seams.heartbeatStep = func(_ *Daemon, step heartbeatStep) {
		order = append(order, step.name)
		clk.Advance(took[step.name])
	}
	return d, logs, &order
}

// stepNames are the first n steps heartbeatWork runs with lifecycle true.
func stepNames(n int) []string {
	names := make([]string, 0, n)
	for _, s := range heartbeatSteps {
		names = append(names, s.name)
		if len(names) == n {
			break
		}
	}
	return names
}

// A step that runs 2 seconds or more logs a line of its own; a faster one does
// not (gt-1uz6p).
func TestHeartbeatLogsSlowSteps(t *testing.T) {
	t.Parallel()
	names := stepNames(3)
	d, logs, _ := timedHeartbeatDaemon(t, map[string]time.Duration{
		names[0]: 1 * time.Second,
		names[1]: 3 * time.Second,
		names[2]: 10 * time.Second,
	})

	d.heartbeatWork(&State{Running: true}, true)

	got := logs.String()
	if strings.Contains(got, "Heartbeat step "+names[0]+" took") {
		t.Errorf("%s: a 1s step logged a slow-step line:\n%s", t.Name(), got)
	}
	for _, want := range []string{
		"Heartbeat step " + names[1] + " took 3s",
		"Heartbeat step " + names[2] + " took 10s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s: log lacks %q:\n%s", t.Name(), want, got)
		}
	}
}

// The line after "Heartbeat complete" gives the total and the three slowest
// steps, longest first.
func TestHeartbeatSummaryNamesTheSlowestThree(t *testing.T) {
	t.Parallel()
	n := stepNames(5)
	d, logs, _ := timedHeartbeatDaemon(t, map[string]time.Duration{
		n[0]: 4 * time.Second,
		n[1]: 1 * time.Second,
		n[2]: 9 * time.Second,
		n[3]: 3 * time.Second,
		n[4]: 6 * time.Second,
	})

	d.heartbeatWork(&State{Running: true}, true)

	want := "Heartbeat took 23s; slowest: " + n[2] + " 9s, " + n[4] + " 6s, " + n[0] + " 4s"
	got := logs.String()
	if !strings.Contains(got, want) {
		t.Errorf("%s: log lacks %q:\n%s", t.Name(), want, got)
	}
	complete := strings.Index(got, "Heartbeat complete (#1)")
	if complete < 0 || complete > strings.Index(got, want) {
		t.Errorf("%s: the summary does not follow the completion line:\n%s", t.Name(), got)
	}
}

// Timing adds no step, drops none and keeps the order and the lifecycle rule.
func TestHeartbeatTimingChangesNothingElse(t *testing.T) {
	t.Parallel()
	var all, upkeep []string
	for _, s := range heartbeatSteps {
		all = append(all, s.name)
		if !s.lifecycle {
			upkeep = append(upkeep, s.name)
		}
	}
	for _, tc := range []struct {
		name      string
		lifecycle bool
		want      []string
	}{{"lifecycle", true, all}, {"standby", false, upkeep}} {
		d, _, order := timedHeartbeatDaemon(t, nil)
		d.heartbeatWork(&State{Running: true}, tc.lifecycle)
		if strings.Join(*order, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: %s steps ran as %v, want %v", t.Name(), tc.name, *order, tc.want)
		}
	}
}

// The slow-step line starts at exactly slowHeartbeatStep: a step one
// millisecond short of it logs nothing and a step of exactly 2s does (gt-p4ted).
func TestHeartbeatSlowStepThresholdBoundary(t *testing.T) {
	t.Parallel()
	names := stepNames(2)
	d, logs, _ := timedHeartbeatDaemon(t, map[string]time.Duration{
		names[0]: slowHeartbeatStep - time.Millisecond,
		names[1]: slowHeartbeatStep,
	})

	d.heartbeatWork(&State{Running: true}, true)

	got := logs.String()
	if strings.Contains(got, "Heartbeat step "+names[0]+" took") {
		t.Errorf("%s: a step 1ms under the threshold logged a slow-step line:\n%s", t.Name(), got)
	}
	if want := "Heartbeat step " + names[1] + " took 2s"; !strings.Contains(got, want) {
		t.Errorf("%s: log lacks %q for a step of exactly the threshold:\n%s", t.Name(), want, got)
	}
}

// A heartbeat that ran fewer than three steps names the ones it ran and no
// more (gt-p4ted).
func TestHeartbeatSummaryWithFewerThanThreeSteps(t *testing.T) {
	t.Parallel()
	timing := &heartbeatTiming{}
	if got, want := timing.summary(0), "Heartbeat took 0s; slowest: "; got != want {
		t.Errorf("no steps: summary = %q, want %q", got, want)
	}
	timing.steps = []stepTime{{name: "a", took: time.Second}, {name: "b", took: 3 * time.Second}}
	if got, want := timing.summary(4*time.Second), "Heartbeat took 4s; slowest: b 3s, a 1s"; got != want {
		t.Errorf("two steps: summary = %q, want %q", got, want)
	}
}
