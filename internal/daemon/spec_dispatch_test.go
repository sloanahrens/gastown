package daemon

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

func TestSpecDispatchDefaultsOff(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "spec_dispatch") {
		t.Error("spec_dispatch must be off with no config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "spec_dispatch") {
		t.Error("spec_dispatch must be off with no spec_dispatch entry")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "spec_dispatch") {
		t.Error("spec_dispatch enabled:true must be on")
	}
}

func TestSpecDispatchInterval(t *testing.T) {
	t.Parallel()
	if got := specDispatchInterval(nil); got != 60*time.Second {
		t.Errorf("default = %v", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{IntervalStr: "90s"}}}
	if got := specDispatchInterval(cfg); got != 90*time.Second {
		t.Errorf("configured = %v", got)
	}
	cfg.Patrols.SpecDispatch.IntervalStr = "bogus"
	if got := specDispatchInterval(cfg); got != 60*time.Second {
		t.Errorf("bad interval = %v", got)
	}
}

func TestFormatSpecDispatchReport(t *testing.T) {
	t.Parallel()
	out := []byte("  ✓ Work attached to p\n{\n  \"template\": \"built-in\",\n  \"roster\": \"hooked 1/2, hookless 2/2\",\n  \"candidates\": 3,\n" +
		"  \"dispatched\": [{\"bead\": \"gt-a\", \"line\": \"gt-a: slung to gastown/p on claude-sonnet (hooked seat 2/2)\"}],\n" +
		"  \"refused\": [{\"bead\": \"gt-b\", \"line\": \"gt-b: spec lint refused: ## Gate: section missing\"}],\n" +
		"  \"planning\": null, \"skipped\": [{\"bead\": \"gt-c\", \"line\": \"x\"}], \"failed\": null\n}\n")
	lines := formatSpecDispatchReport(out)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"tick: 3 candidate(s), roster hooked 1/2, hookless 2/2, 1 dispatched, 1 refused, 0 planning, 1 skipped, 0 failed",
		"dispatched: gt-a: slung to gastown/p",
		"refused: gt-b: spec lint refused: ## Gate",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if got := formatSpecDispatchReport([]byte(`{"hold":"town ESTOP active"}`)); len(got) != 1 || got[0] != "held: town ESTOP active" {
		t.Errorf("hold = %v", got)
	}
	if got := formatSpecDispatchReport([]byte("garbage")); !strings.Contains(got[0], "unparseable") {
		t.Errorf("garbage = %v", got)
	}
}

func TestTriggerSpecDispatchSingleFlight(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	d.specDispatchRunning.Store(true)
	if d.triggerSpecDispatch() {
		t.Fatal("a second tick started while one was running")
	}
	d.specDispatchRunning.Store(false)
	// No config: the tick returns without shelling out.
	if !d.triggerSpecDispatch() {
		t.Fatal("tick did not start")
	}
	d.specDispatchCycles.Wait()
	if d.specDispatchRunning.Load() {
		t.Fatal("guard not released")
	}
}
