package daemon

import (
	"io"
	"log"
	"testing"
)

func idleTestDaemon() *Daemon {
	return &Daemon{
		config: &Config{TownRoot: "/tmp/test"},
		logger: log.New(io.Discard, "", 0),
	}
}

func TestIsIdleForUpgrade(t *testing.T) {
	cases := []struct {
		name string
		set  func(d *Daemon)
		want bool
	}{
		{"nothing in flight", func(d *Daemon) {}, true},
		{"script plugin running", func(d *Daemon) {
			d.scripts = newScriptRunner()
			d.scripts.tryStart("rebuild-gt")
		}, false},
		{"script runner finished", func(d *Daemon) {
			d.scripts = newScriptRunner()
			d.scripts.tryStart("rebuild-gt")
			d.scripts.finish("rebuild-gt")
		}, true},
		{"compactor dog running", func(d *Daemon) { d.compactorDogRunning = true }, false},
		{"boot triage in flight", func(d *Daemon) { d.bootTriageInFlight.Store(true) }, false},
		{"scheduled slings running", func(d *Daemon) { d.scheduledSlingsRunning.Store(true) }, false},
		{"mayor dispatch running", func(d *Daemon) { d.mayorDispatchRunning.Store(true) }, false},
		{"patrol watchdog running", func(d *Daemon) { d.patrolWatchdogRunning.Store(true) }, false},
		{"main branch test mid-run", func(d *Daemon) { d.mainBranchTestRunning.Store(true) }, false},
		{"main branch test waiting for a slot", func(d *Daemon) {
			d.mainBranchTestRunning.Store(true)
			d.mainBranchTestWaitingSlot.Store(true)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := idleTestDaemon()
			tc.set(d)
			if got := d.isIdleForUpgrade(); got != tc.want {
				t.Fatalf("isIdleForUpgrade() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScriptRunnerRunningCountNilSafe(t *testing.T) {
	var r *scriptRunner
	if got := r.runningCount(); got != 0 {
		t.Fatalf("nil runner runningCount() = %d, want 0", got)
	}
}
