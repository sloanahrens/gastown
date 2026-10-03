package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Fix round 1 guards: restart suppression, the Dolt-task lock, discovery,
// external servers, and the skipped-window streak. Fakes only; no Dolt, no
// ~/gt, no Docker.

// --- Dolt health restart suppression ---------------------------------------

func suppressionTestManager(t *testing.T, health, write, identity error) (*DoltServerManager, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var stops, starts atomic.Int32
	var running atomic.Bool
	running.Store(true)
	m := newTestManager(t)
	m.runningFn = func() (int, bool) {
		if running.Load() {
			return 1234, true
		}
		return 0, false
	}
	m.healthCheckFn = func() error { return health }
	m.writeProbeCheckFn = func() error { return write }
	m.identityCheckFn = func() error { return identity }
	m.listDatabasesFn = func() ([]string, error) { return nil, nil }
	m.stopFn = func() { stops.Add(1); running.Store(false) }
	m.startFn = func() error { starts.Add(1); running.Store(true); return nil }
	m.sleepFn = func(time.Duration) {}
	m.unhealthyAlertFn = func(error) {}
	m.readOnlyAlertFn = func(error) {}
	return m, &stops, &starts
}

func TestEnsureRunningDefersRestartWhileGCInFlight(t *testing.T) {
	t.Parallel()
	boom := errors.New("probe timed out")
	cases := []struct {
		name                    string
		health, write, identity error
	}{
		{"unhealthy", boom, nil, nil},
		{"read-only", nil, boom, nil},
		{"identity", nil, nil, boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, stops, starts := suppressionTestManager(t, tc.health, tc.write, tc.identity)
			m.SetRestartSuppressor(func() bool { return true })
			if err := m.EnsureRunning(); err != nil {
				t.Fatalf("EnsureRunning: %v", err)
			}
			if stops.Load() != 0 || starts.Load() != 0 {
				t.Errorf("restart ran during a gc: stops=%d starts=%d", stops.Load(), starts.Load())
			}

			// Without suppression the same failure restarts: the test above
			// is not passing because the probe never fails.
			m2, stops2, _ := suppressionTestManager(t, tc.health, tc.write, tc.identity)
			m2.SetRestartSuppressor(func() bool { return false })
			if err := m2.EnsureRunning(); err != nil {
				t.Fatalf("EnsureRunning: %v", err)
			}
			if stops2.Load() != 1 {
				t.Errorf("unsuppressed %s failure stopped %d time(s), want 1", tc.name, stops2.Load())
			}
		})
	}
}

func TestEnsureRunningStartsDeadServerEvenWhileSuppressed(t *testing.T) {
	t.Parallel()
	m, _, starts := suppressionTestManager(t, nil, nil, nil)
	m.runningFn = func() (int, bool) { return 0, starts.Load() > 0 }
	m.SetRestartSuppressor(func() bool { return true })
	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if starts.Load() != 1 {
		t.Errorf("a dead server was not started while a gc was marked in flight (starts=%d)", starts.Load())
	}
}

func TestDoltRestartHeldForGC(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)

	// Hold dispatched work instead of running it, as a slow gt escalate on
	// its own goroutine would: the restart decision must not wait on it.
	var dispatched []func()
	d.maint.dispatch = func(fn func()) { dispatched = append(dispatched, fn) }

	if d.doltRestartHeldForGC() {
		t.Error("held with no gc cycle running")
	}
	d.maintenanceGCRunning.Store(true)
	if d.doltRestartHeldForGC() {
		t.Error("held between gc calls (nothing in flight)")
	}
	d.maintenanceGCCallStartedAt.Store(time.Now().Add(-time.Minute).UnixNano())
	if !d.doltRestartHeldForGC() {
		t.Error("not held while a gc call is in flight and under its timeout")
	}
	if len(dispatched) != 0 {
		t.Fatalf("escalated %d time(s) for a gc call within its timeout", len(dispatched))
	}

	// Past timeout + grace: release the restart, and escalate exactly once
	// per call, off the calling goroutine.
	d.maintenanceGCCallStartedAt.Store(time.Now().Add(-(maintenanceGCTimeout + maintenanceGCRestartGrace + time.Minute)).UnixNano())
	for i := 0; i < 2; i++ {
		if d.doltRestartHeldForGC() {
			t.Error("still held for a gc call past its timeout")
		}
	}
	if len(dispatched) != 1 {
		t.Fatalf("dispatched %d escalation(s) for one overdue call, want 1", len(dispatched))
	}
	if len(f.escalations) != 0 {
		t.Fatal("the escalation ran on the calling goroutine instead of being dispatched")
	}
	dispatched[0]()
	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "past its") {
		t.Errorf("overdue escalation = %q", f.escalations)
	}
}

// The overdue escalation's production dispatch runs it off the calling
// goroutine: the wiring guard for maintenance's dispatch default.
func TestMaintenanceDispatchDefaultsToAGoroutine(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	hold := make(chan struct{})
	ran := make(chan struct{})
	d.maintenance().dispatch(func() {
		<-hold
		close(ran)
	})
	// Reaching here means dispatch did not run fn inline, where it would
	// have blocked on hold forever.
	close(hold)
	<-ran
}

func TestDoctorDogSkipsWhileGCHoldsLock(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		DoctorDog: &DoctorDogConfig{Enabled: true},
	}}
	d.doltMaintMu.Lock()
	d.runDoctorDog()
	d.doltMaintMu.Unlock()
	if !strings.Contains(logs.String(), "doctor_dog: skipped: gc in flight") {
		t.Errorf("doctor_dog did not skip while a gc held the lock:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "all clear") || strings.Contains(logs.String(), "finding(s)") {
		t.Errorf("doctor_dog probed Dolt while a gc held the lock:\n%s", logs.String())
	}
}

// --- the Dolt-task lock ---------------------------------------------------------

func TestGCDefersWhileDaemonDoltTaskHoldsLock(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}

	release, ok := d.tryDoltTask("doctor_dog")
	if !ok {
		t.Fatal("a task could not take the read side with no gc running")
	}
	res := d.maintenanceGCCycle([]string{"hq"}, "/unused")
	release()

	if res.outcome != gcOutcomeDeferred || res.reason != "daemon Dolt task in flight" {
		t.Fatalf("result = %v/%q, want deferred/'daemon Dolt task in flight'", res.outcome, res.reason)
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran while a daemon Dolt task held the lock: %v", f.gcCalls)
	}

	// Once the task is done the lock is free and gc runs.
	res = d.maintenanceGCCycle([]string{"hq"}, "/unused")
	if res.outcome != gcOutcomeCompleted || len(f.gcCalls) != 1 {
		t.Errorf("after release: outcome %v, gc calls %v", res.outcome, f.gcCalls)
	}
}

func TestDaemonDoltTasksSkipWhileGCHoldsLock(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}

	var taskRan []bool
	d.maint.gcExec = func(_ context.Context, d *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		for _, name := range []string{"doctor_dog", "wisp_reaper", "jsonl_git_backup", "compactor_dog"} {
			release, ok := d.tryDoltTask(name)
			taskRan = append(taskRan, ok)
			if ok {
				release()
			}
		}
		return nil
	}

	d.maintenanceGCCycle([]string{"hq"}, "/unused")

	for i, ok := range taskRan {
		if ok {
			t.Errorf("task %d took the lock while a gc held it", i)
		}
	}
	if !strings.Contains(logs.String(), "doctor_dog: skipped: gc in flight") {
		t.Errorf("skip not logged:\n%s", logs.String())
	}
	// The gc released the write side.
	if release, ok := d.tryDoltTask("doctor_dog"); !ok {
		t.Error("lock still held after the gc returned")
	} else {
		release()
	}
	// Tasks do not exclude each other: two readers at once.
	r1, ok1 := d.tryDoltTask("a")
	r2, ok2 := d.tryDoltTask("b")
	if !ok1 || !ok2 {
		t.Error("two daemon Dolt tasks excluded each other")
	}
	if ok1 {
		r1()
	}
	if ok2 {
		r2()
	}
}

func TestWrappedDoltTasksSkipWhileGCHoldsLock(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		WispReaper:     &WispReaperConfig{Enabled: true},
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true},
	}}
	d.doltMaintMu.Lock()
	d.reapWisps()
	d.syncJsonlGitBackup()
	d.doltMaintMu.Unlock()
	for _, want := range []string{"wisp_reaper: skipped: gc in flight", "jsonl_git_backup: skipped: gc in flight"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q:\n%s", want, logs.String())
		}
	}
}

// triggerCompactorDog's goroutine takes the read side before its cycle: with
// a gc holding the write side, the cycle neither runs nor records a run, and
// it runs on the next check once the gc is done.
func TestCompactorDogSkipsWhileGCHoldsLock(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	cycles := 0
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return cycles
	}

	var logs bytes.Buffer
	d := compactorDogTestDaemon(t.TempDir(), &logs)
	d.compactorDogCycleFn = func() bool {
		mu.Lock()
		cycles++
		mu.Unlock()
		return true
	}
	d.doltMaintMu.Lock()
	d.triggerCompactorDog()
	awaitCompactorDogIdle(t, d)
	d.doltMaintMu.Unlock()

	if n := count(); n != 0 {
		t.Fatalf("compactor_dog cycle ran %d time(s) while a gc held the lock", n)
	}
	if !strings.Contains(logs.String(), "compactor_dog: skipped: gc in flight") {
		t.Errorf("skip not logged:\n%s", logs.String())
	}
	d.compactorDogMu.Lock()
	recorded := d.lastCompactorDogRun
	d.compactorDogMu.Unlock()
	if !recorded.IsZero() {
		t.Errorf("a skipped cycle recorded a run at %v; the next check would not retry", recorded)
	}

	d.triggerCompactorDog()
	awaitCompactorDogIdle(t, d)
	if n := count(); n != 1 {
		t.Errorf("after the gc: %d cycle(s), want 1", n)
	}
}

// --- discovery and external servers ------------------------------------------------

func TestDiscoverMaintenanceDatabases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, p := range []string{"hq/.dolt", "gt/.dolt", "notadb", ".hidden/.dolt"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := discoverMaintenanceDatabases(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "gt,hq" {
		t.Errorf("discovered %v, want [gt hq]", got)
	}
	if _, err := discoverMaintenanceDatabases(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing data dir returned no error")
	}
}

func TestScheduledMaintenanceGCUsesDiscoveryNotCompactorList(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}, "gt": {600 * mib, 0}}
	// The compactor list names only hq; discovery finds both.
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	if strings.Join(f.gcCalls, ",") != "hq,gt" {
		t.Errorf("gc calls = %v, want hq,gt from discovery", f.gcCalls)
	}
}

func TestScheduledMaintenanceGCSkipsExternalServer(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran against an external server: %v", f.gcCalls)
	}
	if !strings.Contains(logs.String(), "externally managed") {
		t.Errorf("external skip not logged:\n%s", logs.String())
	}
}

func TestMaintenanceGCExternal(t *testing.T) {
	t.Parallel()
	mk := func(cfg *DoltServerConfig) *Daemon {
		return &Daemon{
			config:     &Config{TownRoot: t.TempDir()},
			logger:     log.New(io.Discard, "", 0),
			doltServer: &DoltServerManager{config: cfg},
		}
	}
	if ext, _ := mk(&DoltServerConfig{Enabled: true, Host: "127.0.0.1"}).maintenanceGCExternal(); ext {
		t.Error("local managed server reported external")
	}
	if ext, _ := mk(&DoltServerConfig{Enabled: true, External: true, Host: "127.0.0.1"}).maintenanceGCExternal(); !ext {
		t.Error("external-mode server not reported external")
	}
	// The host is the town's endpoint, never the manager's daemon.json copy.
	remote := mk(&DoltServerConfig{Enabled: true})
	writeManagedDoltConfig(t, remote.config.TownRoot, "listener:\n  host: 10.0.0.5\n  port: 3307\n")
	if ext, why := remote.maintenanceGCExternal(); !ext || !strings.Contains(why, "10.0.0.5") {
		t.Errorf("remote host: external=%v why=%q", ext, why)
	}
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false", h)
		}
	}
}

func TestDoltGCFullRefusesInvalidNameBeforeConnecting(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	for _, db := range []string{"", "../hq", "hq?allowAllFiles=true", ".dolt", "a b", "hq/x"} {
		if err := d.doltGCFull(context.Background(), db); err == nil || !strings.Contains(err.Error(), "invalid database name") {
			t.Errorf("doltGCFull(%q) = %v, want invalid-name error", db, err)
		}
	}
}

// --- skipped-window streak ------------------------------------------------------

func TestDeferredWindowThresholds(t *testing.T) {
	t.Parallel()
	var fired []int
	for n := 1; n <= 10; n++ {
		if shouldEscalateDeferredWindows(n, maintenanceDeferredWindowsBeforeEscalation) {
			fired = append(fired, n)
		}
	}
	if fmt.Sprint(fired) != "[3 6 9]" {
		t.Errorf("daily escalations at windows %v, want [3 6 9]", fired)
	}
}

func TestDeferredWindowStreakEscalates(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	base := time.Date(2026, 9, 26, 4, 0, 0, 0, time.Local)
	pending := []gcCandidate{{name: "gt", before: gcMeasure{total: 900 * mib}}}

	window := func(i int) time.Time { return base.Add(time.Duration(i) * 24 * time.Hour) }
	for i := 0; i < 6; i++ {
		d.recordGCDeferral(window(i), "slot held by gastown/refinery", pending)
		// Still inside the window: not counted.
		d.closeDeferredGCWindow(window(i).Add(-time.Minute))
		st, _ := loadMaintenanceGCState(d.config.TownRoot)
		if st.ConsecutiveDeferredWindows != i {
			t.Fatalf("window %d counted before it closed (count=%d)", i, st.ConsecutiveDeferredWindows)
		}
		d.closeDeferredGCWindow(window(i).Add(time.Minute))
		// A second tick after close does not double count.
		d.closeDeferredGCWindow(window(i).Add(6 * time.Minute))
	}

	st, _ := loadMaintenanceGCState(d.config.TownRoot)
	if st.ConsecutiveDeferredWindows != 6 {
		t.Errorf("streak = %d, want 6", st.ConsecutiveDeferredWindows)
	}
	if st.LastDeferralReason != "slot held by gastown/refinery" {
		t.Errorf("last reason = %q", st.LastDeferralReason)
	}
	if len(f.escalations) != 2 {
		t.Fatalf("escalations = %d, want 2 (at windows 3 and 6): %v", len(f.escalations), f.escalations)
	}
	msg := f.escalations[0]
	for _, want := range []string{"3 consecutive", "gt: 900.0MiB", "slot held by gastown/refinery"} {
		if !strings.Contains(msg, want) {
			t.Errorf("escalation missing %q:\n%s", want, msg)
		}
	}

	// A completed run resets the streak.
	d.resetGCDeferralStreak()
	st, _ = loadMaintenanceGCState(d.config.TownRoot)
	if st.ConsecutiveDeferredWindows != 0 || st.PendingDeferral != nil || len(st.DeferralReasons) != 0 {
		t.Errorf("after reset: %+v", st)
	}
}

func TestScheduledMaintenanceGCRecordsDeferralAndCompletionResets(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	f.quietUntil = 0 // busy
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	st, _ := loadMaintenanceGCState(d.config.TownRoot)
	if st.PendingDeferral == nil || len(st.PendingDeferral.Databases) != 1 || st.PendingDeferral.Databases[0].Name != "hq" {
		t.Fatalf("deferral not recorded: %+v", st.PendingDeferral)
	}
	if !st.PendingDeferral.WindowEnd.After(gcTestNow) {
		t.Errorf("window end %v is not in the future", st.PendingDeferral.WindowEnd)
	}

	f.quietUntil = -1
	d.runScheduledMaintenance()
	st, _ = loadMaintenanceGCState(d.config.TownRoot)
	if st.PendingDeferral != nil || st.ConsecutiveDeferredWindows != 0 {
		t.Errorf("completed run did not reset the streak: %+v", st)
	}
}
