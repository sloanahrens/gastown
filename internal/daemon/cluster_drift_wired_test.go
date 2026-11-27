package daemon

import "testing"

// TestClusterDriftWiredTheDaemonCarriesTheRealCompare: the wiring step New runs
// leaves the daemon a non-nil drift compare, so a daemon with the patrols
// setting on calls the real run instead of the unreadable answer a nil field
// stands for. The unit tier drives that step on a daemon it builds because New's
// success path starts tmux and writes the process environment; nothing here
// starts a process (gt-v49mtr).
func TestClusterDriftWiredTheDaemonCarriesTheRealCompare(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	d.wireClusterDriftCompare()
	if d.clusterDriftCompare == nil {
		t.Fatal("Daemon.clusterDriftCompare = nil after the production wiring step, want the real compare")
	}
}
