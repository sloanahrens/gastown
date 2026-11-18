package daemon

import "testing"

// TestWorkspaceDriftRecordsLastGoodPush: the reconcile loop records the hash of
// each push that stored, which is the tree answers are checked against. The read
// is safe once startDriftRun returns: it waits for the tick, which the loop sets
// after it records the push.
func TestWorkspaceDriftRecordsLastGoodPush(t *testing.T) {
	t.Parallel()
	r := startDriftRun(t)
	r.d.workspaceDrift.mu.Lock()
	defer r.d.workspaceDrift.mu.Unlock()
	if got := r.d.workspaceDrift.lastGood; got != "sha" {
		t.Errorf("last good push = %q after a push that stored \"sha\", want \"sha\"", got)
	}
}
