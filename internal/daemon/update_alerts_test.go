package daemon

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/update"
)

// updateAlertTestNow is the clock every alert test measures from.
var updateAlertTestNow = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

// newUpdateAlertTestDaemon is a daemon on a temp town whose alerts go to a
// recorder and whose log goes to a buffer.
func newUpdateAlertTestDaemon(t *testing.T) (*Daemon, *notifyfake.Recorder, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	rec := notifyfake.New()
	d := &Daemon{
		config:   DefaultConfig(t.TempDir()),
		logger:   log.New(&logs, "", 0),
		notifier: rec,
	}
	return d, rec, &logs
}

// writeUpdateAlertState publishes st as the town's update state.
func writeUpdateAlertState(t *testing.T, d *Daemon, st update.State) {
	t.Helper()
	store := update.Store{Dir: update.Dir(d.config.TownRoot)}
	if err := store.Write(st); err != nil {
		t.Fatalf("write update state: %v", err)
	}
}

// findUpdateAlert returns the alert under key, or nil.
func findUpdateAlert(alerts []updateAlert, key string) *updateAlert {
	for i := range alerts {
		if alerts[i].Key == key {
			return &alerts[i]
		}
	}
	return nil
}

func TestUpdateAlertsStaleHeartbeat(t *testing.T) {
	t.Parallel()

	now := updateAlertTestNow
	running := func(heartbeat time.Time) update.State {
		return update.State{
			RunID:       "20260310T115000Z-abcd",
			Source:      update.SourceManual,
			Outcome:     update.OutcomeRunning,
			StartedAt:   now.Add(-10 * time.Minute),
			HeartbeatAt: heartbeat,
			Current:     update.StepDrain,
			Hold:        update.HoldRecord{Owned: true, Note: "dispatch held for update run 20260310T115000Z-abcd"},
		}
	}

	tests := []struct {
		name string
		st   update.State
		want bool
	}{
		{"a four minute old heartbeat is stale", running(now.Add(-4 * time.Minute)), true},
		{"a two minute old heartbeat is alive", running(now.Add(-2 * time.Minute)), false},
		{"exactly three minutes is not yet stale", running(now.Add(-updateStaleAfter)), false},
		{"a running run with no heartbeat is stale", running(time.Time{}), true},
		{
			"an ended run is not stale",
			update.State{RunID: "run-ok", Outcome: update.OutcomeOK, StartedAt: now.Add(-time.Hour), EndedAt: now.Add(-30 * time.Minute)},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alert := findUpdateAlert(updateAlertsFor(now, tt.st, 2*time.Hour), updateAlertKeyStale)
			if got := alert != nil; got != tt.want {
				t.Fatalf("update:stale raised = %v, want %v", got, tt.want)
			}
			if !tt.want {
				return
			}
			for _, want := range []string{tt.st.RunID, string(update.StepDrain), tt.st.Hold.Note, "remove the dispatch hold file"} {
				if !strings.Contains(alert.Message, want) {
					t.Errorf("stale message = %q, want it to name %q", alert.Message, want)
				}
			}
		})
	}
}

func TestUpdateAlertsOverrun(t *testing.T) {
	t.Parallel()

	now := updateAlertTestNow
	runningSince := func(started time.Time) update.State {
		return update.State{
			RunID:       "20260310T090000Z-abcd",
			Source:      update.SourceDaemon,
			Outcome:     update.OutcomeRunning,
			StartedAt:   started,
			HeartbeatAt: now,
			Current:     update.StepInstall,
		}
	}

	tests := []struct {
		name       string
		st         update.State
		maxRuntime time.Duration
		want       bool
	}{
		{"three hours past a two hour limit", runningSince(now.Add(-3 * time.Hour)), 2 * time.Hour, true},
		{"one hour inside a two hour limit", runningSince(now.Add(-time.Hour)), 2 * time.Hour, false},
		{"exactly the limit is not yet past it", runningSince(now.Add(-2 * time.Hour)), 2 * time.Hour, false},
		{"an ended run cannot overrun", update.State{RunID: "run", Outcome: update.OutcomeOK, EndedAt: now}, 2 * time.Hour, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alert := findUpdateAlert(updateAlertsFor(now, tt.st, tt.maxRuntime), updateAlertKeyOverrun)
			if got := alert != nil; got != tt.want {
				t.Fatalf("update:overrun raised = %v, want %v", got, tt.want)
			}
			if !tt.want {
				return
			}
			for _, want := range []string{tt.st.RunID, string(update.StepInstall), "max_runtime of 2h0m0s"} {
				if !strings.Contains(alert.Message, want) {
					t.Errorf("overrun message = %q, want it to name %q", alert.Message, want)
				}
			}
		})
	}

	// The default has to come from the settings resolver, not a caller: a
	// town that never configured the block still alerts on a three hour run.
	t.Run("unconfigured falls back to two hours", func(t *testing.T) {
		t.Parallel()
		d, rec, _ := newUpdateAlertTestDaemon(t)
		writeUpdateAlertState(t, d, runningSince(now.Add(-3*time.Hour)))
		d.checkUpdateAlerts(now)
		if got := escalationsUnder(rec, updateAlertKeyOverrun); len(got) != 1 {
			t.Fatalf("update:overrun escalations = %d, want the default limit to have raised it once", len(got))
		}
	})

	t.Run("an invalid max_runtime falls back to two hours", func(t *testing.T) {
		t.Parallel()
		d, rec, _ := newUpdateAlertTestDaemon(t)
		d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			Update: &config.UpdateConfig{Enabled: true, MaxRuntime: "two hours"},
		}}
		writeUpdateAlertState(t, d, runningSince(now.Add(-3*time.Hour)))
		d.checkUpdateAlerts(now)
		if got := escalationsUnder(rec, updateAlertKeyOverrun); len(got) != 1 {
			t.Fatalf("update:overrun escalations = %d, want the default limit after a bad max_runtime", len(got))
		}
	})

	t.Run("a configured max_runtime sets the limit", func(t *testing.T) {
		t.Parallel()
		d, rec, _ := newUpdateAlertTestDaemon(t)
		d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			Update: &config.UpdateConfig{Enabled: true, MaxRuntime: "90m"},
		}}
		writeUpdateAlertState(t, d, runningSince(now.Add(-2*time.Hour)))
		d.checkUpdateAlerts(now)
		if got := escalationsUnder(rec, updateAlertKeyOverrun); len(got) != 1 {
			t.Fatalf("update:overrun escalations = %d, want the configured 90m limit to have raised it", len(got))
		}
	})
}

func TestUpdateAlertsEndedRun(t *testing.T) {
	t.Parallel()

	now := updateAlertTestNow
	ended := func(outcome update.Outcome, ago time.Duration, steps ...update.StepRecord) update.State {
		return update.State{
			RunID:   "20260310T110000Z-abcd",
			Source:  update.SourceManual,
			Outcome: outcome,
			EndedAt: now.Add(-ago),
			Steps:   steps,
		}
	}
	failedInstall := update.StepRecord{
		Name:   update.StepInstall,
		Result: update.ResultFailed,
		Detail: "make gt-installer failed",
	}
	deferredMaintenance := update.StepRecord{
		Name:   update.StepMaintenance,
		Result: update.ResultDeferred,
		Detail: "the daemon declined the maintenance request",
	}

	tests := []struct {
		name string
		st   update.State
		want bool
	}{
		{"a failed run inside two hours", ended(update.OutcomeFailed, 10*time.Minute, failedInstall), true},
		{"a partial run inside two hours", ended(update.OutcomePartial, 30*time.Minute, deferredMaintenance), true},
		{"an aborted run inside two hours", ended(update.OutcomeAborted, time.Hour), true},
		{"exactly two hours is still inside the window", ended(update.OutcomeFailed, updateEndedAlertWindow), true},
		{"an ok run raises nothing", ended(update.OutcomeOK, 10*time.Minute), false},
		{"an interrupted run raises nothing", ended(update.OutcomeInterrupted, 10*time.Minute), false},
		{"a three hour old failure is history", ended(update.OutcomeFailed, 3*time.Hour, failedInstall), false},
		{"a failed run with no end time raises nothing", update.State{RunID: "run", Outcome: update.OutcomeFailed}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alert := findUpdateAlert(updateAlertsFor(now, tt.st, 2*time.Hour), updateAlertKeyLastRunFailed)
			if got := alert != nil; got != tt.want {
				t.Fatalf("update:last-run-failed raised = %v, want %v", got, tt.want)
			}
			if !tt.want {
				return
			}
			if !strings.Contains(alert.Message, tt.st.RunID) || !strings.Contains(alert.Message, string(tt.st.Outcome)) {
				t.Errorf("ended message = %q, want it to name the run and the outcome", alert.Message)
			}
			for _, step := range tt.st.Steps {
				if step.Result != update.ResultFailed && step.Result != update.ResultDeferred {
					continue
				}
				if !strings.Contains(alert.Message, string(step.Name)) || !strings.Contains(alert.Message, step.Detail) {
					t.Errorf("ended message = %q, want step %s and its detail %q", alert.Message, step.Name, step.Detail)
				}
			}
		})
	}
}

func TestCheckUpdateAlertsRaisesOnceClearsAndRunsWithTheFeatureOff(t *testing.T) {
	t.Parallel()

	now := updateAlertTestNow
	d, rec, _ := newUpdateAlertTestDaemon(t)
	// Drive the heartbeat's own entry point, so deleting the trigger's check
	// call turns this test red.
	fakeClock := clockwork.NewFakeClockAt(now)
	d.clock = fakeClock
	// The feature is off: the alerts must run anyway, because a hand-started
	// run can die too.
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		Update: &config.UpdateConfig{Enabled: false},
	}}
	stale := update.State{
		RunID:       "20260310T115000Z-abcd",
		Source:      update.SourceManual,
		Outcome:     update.OutcomeRunning,
		StartedAt:   now.Add(-10 * time.Minute),
		HeartbeatAt: now.Add(-4 * time.Minute),
		Current:     update.StepDrain,
		Hold:        update.HoldRecord{Owned: true, Note: "dispatch held for update run 20260310T115000Z-abcd"},
	}
	writeUpdateAlertState(t, d, stale)

	d.triggerUpdate()
	d.triggerUpdate()
	fakeClock.Advance(time.Minute)
	d.triggerUpdate()

	raised := escalationsUnder(rec, updateAlertKeyStale)
	if len(raised) != 1 {
		t.Fatalf("update:stale escalations = %d, want exactly one across three heartbeats with the feature off", len(raised))
	}
	if raised[0].Escalation.Severity != "MEDIUM" {
		t.Errorf("severity = %q, want MEDIUM", raised[0].Escalation.Severity)
	}

	// The heartbeat resumes: the condition ended, so the key clears.
	stale.HeartbeatAt = now
	writeUpdateAlertState(t, d, stale)
	d.triggerUpdate()

	clears := clearsOf(rec, updateAlertKeyStale)
	if len(clears) != 1 {
		t.Fatalf("update:stale clears = %d, want exactly one when the heartbeat resumed", len(clears))
	}
	if clears[0].Reason != "update job heartbeat resumed or run ended" {
		t.Errorf("clear reason = %q, want the heartbeat-resumed reason", clears[0].Reason)
	}

	// A healthy state clears once, not every heartbeat.
	d.triggerUpdate()
	if got := len(clearsOf(rec, updateAlertKeyStale)); got != 1 {
		t.Errorf("update:stale clears = %d after a steady heartbeat, want no repeat clear", got)
	}
}

func TestCheckUpdateAlertsMissingOrUnreadableState(t *testing.T) {
	t.Parallel()

	now := updateAlertTestNow
	countReadErrors := func(logs *bytes.Buffer) int {
		return strings.Count(logs.String(), "cannot read the job's state")
	}

	t.Run("no state file raises nothing", func(t *testing.T) {
		t.Parallel()
		d, rec, _ := newUpdateAlertTestDaemon(t)
		d.checkUpdateAlerts(now)
		d.checkUpdateAlerts(now)
		if got := len(rec.Escalations()); got != 0 {
			t.Errorf("escalations = %d, want none for a town that never ran an update", got)
		}
	})

	t.Run("an unreadable file is logged once and raises nothing", func(t *testing.T) {
		t.Parallel()
		d, rec, logs := newUpdateAlertTestDaemon(t)
		dir := update.Dir(d.config.TownRoot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create update dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write unreadable state: %v", err)
		}
		d.checkUpdateAlerts(now)
		d.checkUpdateAlerts(now)
		if got := len(rec.Escalations()); got != 0 {
			t.Errorf("escalations = %d, want none for an unreadable state", got)
		}
		if got := countReadErrors(logs); got != 1 {
			t.Errorf("read error log lines = %d, want exactly one", got)
		}
	})

	t.Run("a version-2 file is logged once and raises nothing", func(t *testing.T) {
		t.Parallel()
		d, rec, logs := newUpdateAlertTestDaemon(t)
		dir := update.Dir(d.config.TownRoot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create update dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"v": 2}`), 0o600); err != nil {
			t.Fatalf("write version-2 state: %v", err)
		}
		d.checkUpdateAlerts(now)
		d.checkUpdateAlerts(now)
		if got := len(rec.Escalations()); got != 0 {
			t.Errorf("escalations = %d, want none for a version this daemon cannot read", got)
		}
		if got := countReadErrors(logs); got != 1 {
			t.Errorf("read error log lines = %d, want exactly one", got)
		}
	})
}

// staleUpdateAlertState is a running record whose heartbeat is four minutes
// old: the condition update:stale announces.
func staleUpdateAlertState(now time.Time) update.State {
	return update.State{
		RunID:       "20260310T115000Z-abcd",
		Source:      update.SourceManual,
		Outcome:     update.OutcomeRunning,
		StartedAt:   now.Add(-10 * time.Minute),
		HeartbeatAt: now.Add(-4 * time.Minute),
		Current:     update.StepDrain,
	}
}

// A raise the notifier refuses is unlatched but not retried on every
// heartbeat: one key waits updateAlertRetryInterval before its next attempt, so
// a notifier that is down cannot spend its retry wait on each heartbeat.
func TestUpdateAlertsRetryBacksOffPerKey(t *testing.T) {
	t.Parallel()

	now := updateAlertTestNow
	d, rec, logs := newUpdateAlertTestDaemon(t)
	writeUpdateAlertState(t, d, staleUpdateAlertState(now))

	// The escalation helper backs off on the daemon's clock, so a refused
	// raise is driven rather than waited out.
	clk := newFixedClock()
	d.clock = clk

	rec.Fail(notifyfake.KindEscalate, errors.New("escalate refused"))
	runOnClock(t, clk, time.Second, func() { d.checkUpdateAlerts(now) })
	firstAttempts := len(escalationsUnder(rec, updateAlertKeyStale))
	if firstAttempts == 0 {
		t.Fatal("no escalation was attempted for a stale run")
	}
	for _, call := range escalationsUnder(rec, updateAlertKeyStale) {
		if call.Err == nil {
			t.Fatalf("an escalation was delivered while the notifier refused every send")
		}
	}
	if d.updateAlerts.isRaised(updateAlertKeyStale) {
		t.Error("the key was latched by a raise the notifier refused")
	}

	// Inside the window the heartbeat does not spend another attempt. Driven
	// on the fake clock like the other passes, so a regression that retries
	// here completes and the count below fails it rather than hanging.
	runOnClock(t, clk, time.Second, func() {
		d.checkUpdateAlerts(now.Add(updateAlertRetryInterval - time.Second))
	})
	if got := len(escalationsUnder(rec, updateAlertKeyStale)); got != firstAttempts {
		t.Errorf("escalation attempts = %d inside the backoff window, want still %d", got, firstAttempts)
	}

	// At the window's end the key is tried again, and a second refusal still
	// does not latch it.
	runOnClock(t, clk, time.Second, func() {
		d.checkUpdateAlerts(now.Add(updateAlertRetryInterval))
	})
	if got := len(escalationsUnder(rec, updateAlertKeyStale)); got <= firstAttempts {
		t.Errorf("escalation attempts = %d after the backoff window, want a retry past %d", got, firstAttempts)
	}
	if d.updateAlerts.isRaised(updateAlertKeyStale) {
		t.Error("the key was latched by the retried raise the notifier refused")
	}
	if !strings.Contains(logs.String(), "cannot raise update:stale") {
		t.Errorf("log = %q, want the refused raise recorded", logs.String())
	}
}

// A clear the notifier refuses leaves the key latched, so the next heartbeat
// tries again instead of reporting a closed alert as done.
func TestUpdateAlertsRefusedClearStaysLatched(t *testing.T) {
	t.Parallel()

	now := updateAlertTestNow
	d, rec, _ := newUpdateAlertTestDaemon(t)
	stale := staleUpdateAlertState(now)
	writeUpdateAlertState(t, d, stale)
	d.checkUpdateAlerts(now)
	if !d.updateAlerts.isRaised(updateAlertKeyStale) {
		t.Fatal("the stale raise was not latched before the clear")
	}

	stale.HeartbeatAt = now
	writeUpdateAlertState(t, d, stale)
	rec.Fail(notifyfake.KindClear, errors.New("clear refused"))
	d.checkUpdateAlerts(now)
	if got := delivered(clearsOf(rec, updateAlertKeyStale)); got != 0 {
		t.Fatalf("clears delivered = %d while the notifier refused them", got)
	}
	if !d.updateAlerts.isRaised(updateAlertKeyStale) {
		t.Error("the key was unlatched by a clear the notifier refused")
	}

	rec.Fail(notifyfake.KindClear, nil)
	d.checkUpdateAlerts(now)
	if got := delivered(clearsOf(rec, updateAlertKeyStale)); got != 1 {
		t.Errorf("clears delivered = %d, want exactly one once the notifier recovers", got)
	}
	if d.updateAlerts.isRaised(updateAlertKeyStale) {
		t.Error("the key stayed latched after the clear was delivered")
	}
}

// A running record that is missing the timestamp its rule measures is still
// judged by that rule: no heartbeat proves no liveness, and no start time
// leaves the run unbounded.
func TestUpdateAlertsMalformedRecord(t *testing.T) {
	t.Parallel()

	now := updateAlertTestNow
	tests := []struct {
		name string
		st   update.State
		key  string
	}{
		{
			"a running record with no heartbeat is stale",
			update.State{
				RunID:     "20260310T115000Z-abcd",
				Source:    update.SourceManual,
				Outcome:   update.OutcomeRunning,
				StartedAt: now.Add(-time.Minute),
				Current:   update.StepDrain,
			},
			updateAlertKeyStale,
		},
		{
			"a running record with no start time is an overrun",
			update.State{
				RunID:       "20260310T115000Z-abcd",
				Source:      update.SourceManual,
				Outcome:     update.OutcomeRunning,
				HeartbeatAt: now,
				Current:     update.StepDrain,
			},
			updateAlertKeyOverrun,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if findUpdateAlert(updateAlertsFor(now, tt.st, 2*time.Hour), tt.key) == nil {
				t.Fatalf("updateAlertsFor(%+v) did not raise %s for a malformed record", tt.st, tt.key)
			}
		})
	}
}

// The check is its own non-lifecycle heartbeat step, so a dead or runaway job
// still raises while the town is E-stopped or Ringed. Making it a lifecycle
// step would let the trigger's step membership silence it exactly then.
func TestUpdateAlertsRunUnderEstopAndTheRing(t *testing.T) {
	t.Parallel()

	now := updateAlertTestNow
	for _, cond := range standbyConditions {
		t.Run(cond.name, func(t *testing.T) {
			t.Parallel()
			d, rec, _ := newUpdateAlertTestDaemon(t)
			d.clock = clockwork.NewFakeClockAt(now)
			writeUpdateAlertState(t, d, staleUpdateAlertState(now))
			cond.set(t, d, now)

			var ran []string
			d.seams.heartbeatStep = func(d *Daemon, step heartbeatStep) {
				ran = append(ran, step.name)
				if step.name == "update-alerts" {
					step.run(d)
				}
			}
			d.heartbeat(&State{})

			if !slices.Contains(ran, "update-alerts") {
				t.Fatalf("steps run under %s = %v, want update-alerts among them", cond.name, ran)
			}
			if slices.Contains(ran, "update") {
				t.Errorf("the update trigger ran under %s, want it held as a lifecycle step", cond.name)
			}
			if got := escalationsUnder(rec, updateAlertKeyStale); len(got) != 1 {
				t.Fatalf("update:stale escalations = %d under %s, want the alert raised", len(got), cond.name)
			}
		})
	}

	t.Run("the alerts are upkeep and the trigger stays lifecycle", func(t *testing.T) {
		t.Parallel()
		var alerts, trigger *heartbeatStep
		for i := range heartbeatSteps {
			switch heartbeatSteps[i].name {
			case "update-alerts":
				alerts = &heartbeatSteps[i]
			case "update":
				trigger = &heartbeatSteps[i]
			}
		}
		if alerts == nil || alerts.lifecycle {
			t.Fatalf("update-alerts step = %+v, want a non-lifecycle step", alerts)
		}
		if trigger == nil || !trigger.lifecycle {
			t.Fatalf("update step = %+v, want a lifecycle step", trigger)
		}
	})
}

// delivered counts the calls whose send succeeded.
func delivered(calls []notifyfake.Call) int {
	n := 0
	for _, call := range calls {
		if call.Err == nil {
			n++
		}
	}
	return n
}

// escalationsUnder returns the escalations filed under key.
func escalationsUnder(rec *notifyfake.Recorder, key string) []notifyfake.Call {
	var out []notifyfake.Call
	for _, call := range rec.Escalations() {
		if call.Escalation.Fingerprint == key {
			out = append(out, call)
		}
	}
	return out
}

// clearsOf returns the clears that named key.
func clearsOf(rec *notifyfake.Recorder, key string) []notifyfake.Call {
	var out []notifyfake.Call
	for _, call := range rec.Clears() {
		for _, fp := range call.Fingerprints {
			if fp == key {
				out = append(out, call)
				break
			}
		}
	}
	return out
}
