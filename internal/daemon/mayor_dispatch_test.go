package daemon

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

func TestMayorDispatchInterval(t *testing.T) {
	if got := mayorDispatchInterval(nil); got != defaultMayorDispatchInterval {
		t.Errorf("expected default interval %v, got %v", defaultMayorDispatchInterval, got)
	}

	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			MayorDispatch: &MayorDispatchConfig{Enabled: true, IntervalStr: "15m"},
		},
	}
	if got := mayorDispatchInterval(config); got != 15*time.Minute {
		t.Errorf("expected 15m interval, got %v", got)
	}

	// An invalid or non-positive interval falls back rather than disabling the
	// patrol: a ticker built from 0 would panic, and from a negative duration
	// would spin.
	for _, invalid := range []string{"invalid", "0", "-5m"} {
		config.Patrols.MayorDispatch.IntervalStr = invalid
		if got := mayorDispatchInterval(config); got != defaultMayorDispatchInterval {
			t.Errorf("interval %q: expected default %v, got %v", invalid, defaultMayorDispatchInterval, got)
		}
	}
}

func TestIsPatrolEnabled_MayorDispatchDefaultsOn(t *testing.T) {
	// Default-ON is the point of this patrol: the failure it exists for is
	// silence on a town whose config says nothing about dispatch.
	if !IsPatrolEnabled(nil, "mayor_dispatch") {
		t.Error("expected mayor_dispatch to be enabled with a nil config")
	}
	if !IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "mayor_dispatch") {
		t.Error("expected mayor_dispatch to be enabled with no entry in daemon.json")
	}

	enabled := &DaemonPatrolConfig{Patrols: &PatrolsConfig{MayorDispatch: &MayorDispatchConfig{Enabled: true}}}
	if !IsPatrolEnabled(enabled, "mayor_dispatch") {
		t.Error("expected mayor_dispatch to be enabled when configured on")
	}

	disabled := &DaemonPatrolConfig{Patrols: &PatrolsConfig{MayorDispatch: &MayorDispatchConfig{Enabled: false}}}
	if IsPatrolEnabled(disabled, "mayor_dispatch") {
		t.Error("expected an explicit config entry to be able to disable mayor_dispatch")
	}
}

func TestParseDispatchCheck(t *testing.T) {
	// The nudge path: a decision plus the message to deliver.
	result, err := parseDispatchCheck([]byte(`{
		"seats": {"source": "polecat_pool", "capacity": 4, "occupied": 3, "free": 1},
		"actionable": 5,
		"nudge": true,
		"message": "Idle-seat check: 1 of 4 polecat seats are free."
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Nudge || result.Seats.Free != 1 || result.Actionable != 5 {
		t.Errorf("parsed check = %+v, want nudge with 1 free seat and 5 actionable", result)
	}

	// The silence path parses too, and carries no message.
	result, err = parseDispatchCheck([]byte(`{"seats": {"free": 0}, "actionable": 0, "nudge": false}`))
	if err != nil {
		t.Fatalf("unexpected error on a silent check: %v", err)
	}
	if result.Nudge {
		t.Error("expected no nudge from a silent check")
	}
}

func TestParseDispatchCheck_RejectsUnusableOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{"empty output", ""},
		{"whitespace only", "   \n"},
		{"not json", "gt: command not found"},
		{"nudge with no message", `{"nudge": true, "message": ""}`},
		{"nudge with blank message", `{"nudge": true, "message": "   "}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDispatchCheck([]byte(tc.out)); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestTriggerMayorDispatch_SingleFlight(t *testing.T) {
	// The patrol is disabled for this daemon so runMayorDispatch returns at the
	// door: this test is about the guard, and a cycle that reached the check
	// would shell out to a gt binary this test does not have.
	d := &Daemon{
		logger:          log.New(io.Discard, "", 0),
		disabledPatrols: map[string]bool{"mayor_dispatch": true},
	}

	// The first trigger claims the guard and starts the cycle; a second tick
	// arriving while it runs is skipped, not queued.
	if !d.triggerMayorDispatch() {
		t.Fatal("first trigger should start a cycle")
	}
	if !d.mayorDispatchRunning.Load() {
		t.Fatal("expected the guard to be held after the first trigger")
	}

	// The cycle clears the guard when it finishes, so the patrol is not
	// permanently single-shot.
	if !waitForMayorDispatchIdle(d, 5*time.Second) {
		t.Fatal("cycle never cleared the running guard")
	}
	if !d.triggerMayorDispatch() {
		t.Error("expected the patrol to run again after the previous cycle finished")
	}
	if !waitForMayorDispatchIdle(d, 5*time.Second) {
		t.Fatal("second cycle never cleared the running guard")
	}
}

// waitForMayorDispatchIdle blocks until the patrol's single-flight guard is
// clear, so the test observes the goroutine's exit rather than racing it.
func waitForMayorDispatchIdle(d *Daemon, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !d.mayorDispatchRunning.Load() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestNudgeMayor_RefusesEmptyMessage(t *testing.T) {
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	err := d.nudgeMayor("   ")
	if err == nil {
		t.Fatal("expected an empty nudge to be refused")
	}
	if !strings.Contains(err.Error(), "empty message") {
		t.Errorf("error should name the reason, got: %v", err)
	}
}
