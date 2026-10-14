package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// These tests drive the stale-gc check through the recorder and a temp state
// file: the clock is gcTestNow, the databases come from a fake discovery, and
// no gt subprocess runs. The gate is `go test ./internal/daemon/ -run
// 'Maintenance|Attention|TownHealth'`.

// staleGCTestDaemon is a daemon whose alerts go to a recorder, on a town root
// the caller chooses (so a second daemon on the same root is a restart). The
// check's dispatch runs inline so a test sees its result as soon as the tick
// returns; the tests that pin the hand-off replace it.
func staleGCTestDaemon(t *testing.T, town string) (*Daemon, *notifyfake.Recorder) {
	t.Helper()
	rec := notifyfake.New()
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: town}, notifier: rec}
	d.maint.dispatch = func(fn func()) { fn() }
	return d, rec
}

// discoverStaleGCDatabases is the fake discovery: these databases exist.
func discoverStaleGCDatabases(d *Daemon, dbs ...string) {
	d.maint.gcDatabases = func(string) ([]string, error) { return dbs, nil }
}

func TestStaleMaintenanceGCs(t *testing.T) {
	t.Parallel()
	now := gcTestNow
	days := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }

	cases := []struct {
		name        string
		lastGC      map[string]time.Time
		databases   []string
		wantStale   []string
		wantCurrent []string
	}{
		{
			name:   "a gc inside the window is current",
			lastGC: map[string]time.Time{"gt": days(1)}, databases: []string{"gt"},
			wantCurrent: []string{"gt"},
		},
		{
			name:   "nine days is current",
			lastGC: map[string]time.Time{"gt": days(9)}, databases: []string{"gt"},
			wantCurrent: []string{"gt"},
		},
		{
			name:   "exactly the stale window is stale",
			lastGC: map[string]time.Time{"gt": now.Add(-maintenanceGCStaleAfter)}, databases: []string{"gt"},
			wantStale: []string{"gt"},
		},
		{
			name:   "eleven days is stale",
			lastGC: map[string]time.Time{"gt": days(11)}, databases: []string{"gt"},
			wantStale: []string{"gt"},
		},
		{
			name:   "a database with no gc of its own borrows a stale town's newest",
			lastGC: map[string]time.Time{"hq": days(12)}, databases: []string{"gt"},
			wantStale: []string{"gt"},
		},
		{
			name:   "a database with no gc of its own is current while the town gc's",
			lastGC: map[string]time.Time{"hq": days(2)}, databases: []string{"gt"},
			wantCurrent: []string{"gt"},
		},
		{
			name:   "a town with no gc recorded has no history to judge against",
			lastGC: map[string]time.Time{}, databases: []string{"gt"},
			wantCurrent: []string{"gt"},
		},
		{
			name:   "both readings at once",
			lastGC: map[string]time.Time{"gt": days(12), "hq": days(1)}, databases: []string{"gt", "hq"},
			wantStale: []string{"gt"}, wantCurrent: []string{"hq"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stale, current := staleMaintenanceGCs(now, tc.databases, maintenanceGCState{LastGC: tc.lastGC})
			if !slices.Equal(stale, tc.wantStale) {
				t.Errorf("stale = %v, want %v", stale, tc.wantStale)
			}
			if !slices.Equal(current, tc.wantCurrent) {
				t.Errorf("current = %v, want %v", current, tc.wantCurrent)
			}
		})
	}
}

func TestStaleGCMessageStatesTheReading(t *testing.T) {
	t.Parallel()
	last := gcTestNow.Add(-12 * 24 * time.Hour)

	got := staleGCMessage(gcTestNow, "gt", maintenanceGCState{LastGC: map[string]time.Time{"gt": last}})
	if !strings.Contains(got, "gt has had no completed gc for 12d") {
		t.Errorf("message = %q, want it to name gt and the 12d gap", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("message = %q; an escalation description is one line", got)
	}

	never := staleGCMessage(gcTestNow, "om", maintenanceGCState{LastGC: map[string]time.Time{"gt": last}})
	if !strings.Contains(never, "om has no completed gc recorded") {
		t.Errorf("message = %q, want the never-gc'd reading", never)
	}
}

// The acceptance: a database past the window raises its key, a stale reading
// raises once, and a completed gc clears it — including in a daemon that never
// raised it, which is what the state file's key set is for.
func TestCheckStaleMaintenanceGCRaisesOneKeyAndClearsItAfterAGc(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d, rec := staleGCTestDaemon(t, town)
	recordRun(t, d, "gt", 11, 100*mib) // stale
	recordRun(t, d, "hq", 1, 100*mib)  // current
	discoverStaleGCDatabases(d, "gt", "hq")

	d.checkStaleMaintenanceGC(gcTestNow)

	esc := rec.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %+v, want one for the stale database alone", esc)
	}
	got := esc[0].Escalation
	if want := doltGCStaleAlertKey("gt"); got.Fingerprint != want {
		t.Errorf("fingerprint = %q, want %q", got.Fingerprint, want)
	}
	if got.Severity != maintenanceGCStaleAlertSeverity {
		t.Errorf("severity = %q, want %q", got.Severity, maintenanceGCStaleAlertSeverity)
	}
	if !strings.HasPrefix(got.Description, maintenanceGCAlertSource+": gt ") {
		t.Errorf("description = %q, want it to lead with the source and the database", got.Description)
	}

	// A second tick on the same reading raises nothing more: gt escalate
	// records a repeat, one Dolt write per call.
	d.checkStaleMaintenanceGC(gcTestNow)
	if n := len(rec.Escalations()); n != 1 {
		t.Errorf("escalations after a second tick = %d, want 1", n)
	}
	if n := len(rec.Clears()); n != 0 {
		t.Errorf("clears while the database is stale = %d, want 0", n)
	}

	// The key is on disk, so a restarted daemon still knows it is open.
	st, err := loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !st.StaleGCAlerts[doltGCStaleAlertKey("gt")] {
		t.Errorf("state file holds %v, want the raised key", st.StaleGCAlerts)
	}

	// A new process on the same town does not raise it again...
	d2, rec2 := staleGCTestDaemon(t, town)
	discoverStaleGCDatabases(d2, "gt", "hq")
	d2.checkStaleMaintenanceGC(gcTestNow)
	if n := len(rec2.Escalations()); n != 0 {
		t.Errorf("a restarted daemon re-raised the open key: %+v", rec2.Escalations())
	}

	// ...and clears it once gt's gc completes, which is the evidence the old
	// process never got to see.
	if err := recordMaintenanceGCRun(town, "gt", gcMeasure{total: 633 * mib}, gcMeasure{total: 480 * mib}, gcTestNow); err != nil {
		t.Fatalf("record gc: %v", err)
	}
	d2.checkStaleMaintenanceGC(gcTestNow)

	clears := rec2.Clears()
	if len(clears) != 1 || !slices.Equal(clears[0].Fingerprints, []string{doltGCStaleAlertKey("gt")}) {
		t.Fatalf("clears = %+v, want one for gt", clears)
	}
	st, err = loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.StaleGCAlerts[doltGCStaleAlertKey("gt")] {
		t.Errorf("state file still holds the cleared key: %v", st.StaleGCAlerts)
	}
	d2.checkStaleMaintenanceGC(gcTestNow)
	if n := len(rec2.Escalations()); n != 0 {
		t.Errorf("the cleared database raised again: %+v", rec2.Escalations())
	}
}

func TestCheckStaleMaintenanceGCUnreadableStateIsOneItem(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d, rec := staleGCTestDaemon(t, town)
	discoverStaleGCDatabases(d, "gt", "hq", "om")
	path := maintenanceGCStatePath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	d.checkStaleMaintenanceGC(gcTestNow)

	esc := rec.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %+v, want one item for the unreadable file", esc)
	}
	if got := esc[0].Escalation.Fingerprint; got != doltGCStateUnreadableAlertKey {
		t.Errorf("fingerprint = %q, want %q", got, doltGCStateUnreadableAlertKey)
	}
	// One item, not one per database, and nothing is cleared: no database's
	// age was judged.
	d.checkStaleMaintenanceGC(gcTestNow)
	if n := len(rec.Escalations()); n != 1 {
		t.Errorf("escalations after a second tick = %d, want 1", n)
	}
	if n := len(rec.Clears()); n != 0 {
		t.Errorf("clears while the state file is unreadable = %d, want 0", n)
	}

	// The file reads again: the item closes and no stale key was left behind.
	if err := recordMaintenanceGCRun(town, "gt", gcMeasure{}, gcMeasure{}, gcTestNow); err != nil {
		t.Fatalf("record gc: %v", err)
	}
	d.checkStaleMaintenanceGC(gcTestNow)

	clears := rec.Clears()
	if len(clears) != 1 || !slices.Equal(clears[0].Fingerprints, []string{doltGCStateUnreadableAlertKey}) {
		t.Fatalf("clears = %+v, want one for the unreadable-state item", clears)
	}
	if n := len(rec.Escalations()); n != 1 {
		t.Errorf("escalations = %d, want no stale key raised over the unreadable read", n)
	}
}

// A key whose database is dropped from the data dir has no gc that could ever
// close it, so the tick closes it instead of holding it open forever.
func TestCheckStaleMaintenanceGCClosesAKeyWhoseDatabaseIsDropped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d, rec := staleGCTestDaemon(t, town)
	recordRun(t, d, "gt", 12, 100*mib)
	recordRun(t, d, "hq", 1, 100*mib)
	discoverStaleGCDatabases(d, "gt", "hq")

	d.checkStaleMaintenanceGC(gcTestNow)
	if n := len(rec.Escalations()); n != 1 {
		t.Fatalf("escalations = %+v, want one for gt", rec.Escalations())
	}

	discoverStaleGCDatabases(d, "hq") // gt's database is gone
	d.checkStaleMaintenanceGC(gcTestNow)

	clears := rec.Clears()
	if len(clears) != 1 || !slices.Equal(clears[0].Fingerprints, []string{doltGCStaleAlertKey("gt")}) {
		t.Fatalf("clears = %+v, want one for the dropped gt", clears)
	}
	st, err := loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.StaleGCAlerts) != 0 {
		t.Errorf("state file still holds %v, want no key", st.StaleGCAlerts)
	}
}

// The check rides the existing tick: a stale database is named on the tick that
// runs its gc, and the key closes on the tick after it completed. This is the
// wiring guard for a refactor that drops the call.
func TestScheduledMaintenanceTickCarriesTheStaleGCAlert(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d, rec := staleGCTestDaemon(t, town)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 300 * mib}}
	f.shrinkTo = map[string]gcMeasure{"gt": {480 * mib, 307 * mib}}
	recordRun(t, d, "gt", 12, 300*mib)
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()

	esc := rec.Escalations()
	if len(esc) != 1 || esc[0].Escalation.Fingerprint != doltGCStaleAlertKey("gt") {
		t.Fatalf("escalations on the tick = %+v, want one for gt", esc)
	}
	if got := strings.Join(f.gcCalls, ","); got != "gt" {
		t.Errorf("gc calls = %q, want gt", got)
	}

	// The cycle recorded gt's gc, so the tick after it closes the key.
	d.runScheduledMaintenance()

	clears := rec.Clears()
	if len(clears) != 1 || !slices.Equal(clears[0].Fingerprints, []string{doltGCStaleAlertKey("gt")}) {
		t.Fatalf("clears = %+v, want one for gt after its gc", clears)
	}
}

// stuckEscalateNotifier is a notifier whose escalations hang, as gt escalate
// does against a slow Dolt, until release is closed. entered gets one value per
// Escalate call that starts.
type stuckEscalateNotifier struct {
	notify.Notifier
	entered chan struct{}
	release chan struct{}
}

func (n *stuckEscalateNotifier) Escalate(ctx context.Context, e notify.Escalation) error {
	n.entered <- struct{}{}
	select {
	case <-n.release:
		return n.Notifier.Escalate(ctx, e)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The run loop must not wait on the alerts: with escalations that hang, a tick
// over several stale databases returns at once, the delivery carries on off the
// loop, and a tick that arrives while it is still stuck starts nothing beside
// it (gt-3v0sf).
func TestCheckStaleMaintenanceGCDoesNotWaitOnEscalations(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d, rec := staleGCTestDaemon(t, town)
	d.maint.dispatch = nil // the production default: a goroutine
	stuck := &stuckEscalateNotifier{Notifier: rec, entered: make(chan struct{}, 16), release: make(chan struct{})}
	d.notifier = stuck
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(stuck.release) }) }
	t.Cleanup(release)
	for _, db := range []string{"gt", "hq", "om"} {
		recordRun(t, d, db, 12, 100*mib)
	}
	discoverStaleGCDatabases(d, "gt", "hq", "om")

	tick := func() {
		t.Helper()
		returned := make(chan struct{})
		go func() {
			d.checkStaleMaintenanceGC(gcTestNow)
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("the stale-gc check waited on a hung escalation instead of handing it off")
		}
	}

	tick()
	select {
	case <-stuck.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached gt escalate off the loop")
	}
	tick() // Still stuck on the first alert: this one must start no second delivery.
	if n := len(stuck.entered); n != 0 {
		t.Errorf("a tick during a stuck delivery started %d more escalation(s), want 0", n)
	}

	release()
	deadline := time.Now().Add(5 * time.Second)
	for d.staleGCCheckRunning.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the delivery never finished after escalations were released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(rec.Escalations()); n != 3 {
		t.Errorf("escalations = %d, want one per stale database (3)", n)
	}
}

// A raise that fails is not recorded open, so the next tick retries it; one that
// landed is not raised again while it is open.
func TestCheckStaleMaintenanceGCRetriesAFailedRaiseOnALaterTick(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d, rec := staleGCTestDaemon(t, town)
	recordRun(t, d, "gt", 12, 100*mib)
	discoverStaleGCDatabases(d, "gt")

	rec.Fail(notifyfake.KindEscalate, errors.New("dolt is slow"))
	d.checkStaleMaintenanceGC(gcTestNow)
	st, err := loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.StaleGCAlerts[doltGCStaleAlertKey("gt")] {
		t.Fatalf("a raise that failed was recorded open: %v", st.StaleGCAlerts)
	}

	rec.Fail(notifyfake.KindEscalate, nil)
	before := len(rec.Escalations())
	d.checkStaleMaintenanceGC(gcTestNow)
	if got := len(rec.Escalations()) - before; got != 1 {
		t.Fatalf("the next tick raised %d alert(s), want the retry", got)
	}
	d.checkStaleMaintenanceGC(gcTestNow)
	if got := len(rec.Escalations()) - before; got != 1 {
		t.Errorf("an open alert was raised again: %d raises since the retry", got)
	}
}
