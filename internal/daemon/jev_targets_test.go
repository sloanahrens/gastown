package daemon

import "testing"

// jev_targets is off unless daemon.json enables it: each deep-review run it
// serves pays for Jev calls.
func TestJevTargetsPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_targets") {
		t.Error("jev_targets must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_targets") {
		t.Error("jev_targets must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevTargets: &JevTargetsConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_targets") {
		t.Error("an explicit enabled=false must keep jev_targets off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevTargets: &JevTargetsConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_targets") {
		t.Error("an explicit enabled=true must turn jev_targets on")
	}
}
