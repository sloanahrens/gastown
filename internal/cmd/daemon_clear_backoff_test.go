package cmd

import "testing"

// TestDaemonReloadPlan pins what clear-backoff does for each daemon state. A
// container daemon reports pid 0, which os.Process.Signal refuses, so the plan
// must skip the signal and name the container rather than fail after the
// backoff is already cleared.
func TestDaemonReloadPlan(t *testing.T) {
	t.Parallel()
	const containerNote = "daemon runs in a container and was not signaled; the change takes effect when the container restarts"
	const notRunningNote = "daemon not running, will take effect on next start"

	tests := []struct {
		name       string
		running    bool
		pid        int
		wantReload bool
		wantNote   string
	}{
		{name: "running with a pid signals and reports the reload", running: true, pid: 4242, wantReload: true, wantNote: "daemon reloaded"},
		{name: "running in a container with pid 0 does not signal", running: true, pid: 0, wantReload: false, wantNote: containerNote},
		{name: "running with a negative pid does not signal", running: true, pid: -1, wantReload: false, wantNote: containerNote},
		{name: "not running keeps the next-start wording", running: false, pid: 0, wantReload: false, wantNote: notRunningNote},
		{name: "not running ignores a stale pid and does not signal", running: false, pid: 4242, wantReload: false, wantNote: notRunningNote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reload, note := daemonReloadPlan(tt.running, tt.pid)
			if reload != tt.wantReload {
				t.Errorf("daemonReloadPlan(%v, %d) reload = %v, want %v", tt.running, tt.pid, reload, tt.wantReload)
			}
			if note != tt.wantNote {
				t.Errorf("daemonReloadPlan(%v, %d) note = %q, want %q", tt.running, tt.pid, note, tt.wantNote)
			}
		})
	}
}
