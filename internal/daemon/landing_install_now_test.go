package daemon

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/landworker"
)

const installNowLine = "rebuild_gt: install requested: a gt:install-now bead landed"

// TestLandingInstallNowRequestsAnInstallAtOnce pins the label's shortcut: the
// pass that landed a gt:install-now bead arms the install while beads are
// still queued behind it, so no drain is waited for (gt-3qmv4.2).
func TestLandingInstallNowRequestsAnInstallAtOnce(t *testing.T) {
	t.Parallel()
	d, logs := landingDrainDaemon(t)
	var passes atomic.Int32
	pass := func(context.Context) landworker.Report {
		passes.Add(1)
		// Every pass lands one more bead, so the queue never drains and only
		// the label can arm the request.
		return landworker.Report{Landed: 1, InstallRequested: true}
	}
	go d.landingWorkerLoop("gastown", time.Hour, pass)

	awaitLog(t, logs, installNowLine)
	if !d.rebuildGTRequested.Load() {
		t.Fatal("the label logged a request it did not arm")
	}
	if n := strings.Count(logs.String(), installNowLine); n != 1 {
		t.Fatalf("request lines = %d, want 1:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), drainRequestLine) {
		t.Fatalf("an undrained queue logged a drain request:\n%s", logs.String())
	}
}

// TestLandingInstallNowOnAnotherRigRequestsNoInstall pins the rig gate: a
// gt:install-now bead landed on any rig but the gt source moves that rig's
// main, not the main the installed binary is built from (gt-3qmv4.2).
func TestLandingInstallNowOnAnotherRigRequestsNoInstall(t *testing.T) {
	t.Parallel()
	d, logs := landingDrainDaemon(t)
	done := countPasses(d)
	pass := func(context.Context) landworker.Report {
		return landworker.Report{Landed: 1, InstallRequested: true}
	}
	go d.landingWorkerLoop("agate", time.Hour, pass)

	// Three finished passes, so the gate has been through the label's request
	// three times: the rig, not an unfinished pass, is what held the install.
	settlePasses(t, done, 3)
	if d.rebuildGTRequested.Load() {
		t.Fatal("a landing on another rig armed the install request")
	}
	if strings.Contains(logs.String(), installNowLine) {
		t.Fatalf("a landing on another rig logged a request:\n%s", logs.String())
	}
}
