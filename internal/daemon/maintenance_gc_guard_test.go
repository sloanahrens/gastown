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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Fix round 1 guards: restart suppression, the Dolt-task lock, the Convoy
// pause, discovery, external servers, and the skipped-window streak. Fakes
// only; no Dolt, no ~/gt, no Docker.

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
	d, _ := gcTestDaemon(t)
	withGCFakes(t)

	// The escalation sink blocks until released, as a slow gt escalate would.
	escalated := make(chan string, 4)
	unblock := make(chan struct{})
	maintenanceEscalateFn = func(_ *Daemon, source, message string) {
		<-unblock
		escalated <- source + "|" + message
	}
	defer close(unblock)

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

	// Past timeout + grace: release the restart promptly even though the
	// escalation blocks, and escalate exactly once per call.
	d.maintenanceGCCallStartedAt.Store(time.Now().Add(-(maintenanceGCTimeout + maintenanceGCRestartGrace + time.Minute)).UnixNano())
	returned := make(chan bool, 2)
	go func() {
		returned <- d.doltRestartHeldForGC()
		returned <- d.doltRestartHeldForGC()
	}()
	for i := 0; i < 2; i++ {
		select {
		case held := <-returned:
			if held {
				t.Error("still held for a gc call past its timeout")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("doltRestartHeldForGC blocked on the escalation (it runs under the Dolt manager lock)")
		}
	}

	unblock <- struct{}{}
	select {
	case msg := <-escalated:
		if !strings.Contains(msg, "past its") {
			t.Errorf("overdue escalation = %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("overdue escalation never sent")
	}
	select {
	case msg := <-escalated:
		t.Errorf("second escalation for the same call: %q", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDoctorDogSkipsWhileGCHoldsLock(t *testing.T) {
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
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 300 * mib}

	release, ok := d.tryDoltTask("dolt_backup")
	if !ok {
		t.Fatal("a task could not take the read side with no gc running")
	}
	res := d.maintenanceGCCycle([]string{"hq"}, "/unused", testGCPolicy)
	release()

	if res.outcome != gcOutcomeDeferred || res.reason != "daemon Dolt task in flight" {
		t.Fatalf("result = %v/%q, want deferred/'daemon Dolt task in flight'", res.outcome, res.reason)
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran while a daemon Dolt task held the lock: %v", f.gcCalls)
	}

	// Once the task is done the lock is free and gc runs.
	res = d.maintenanceGCCycle([]string{"hq"}, "/unused", testGCPolicy)
	if res.outcome != gcOutcomeCompleted || len(f.gcCalls) != 1 {
		t.Errorf("after release: outcome %v, gc calls %v", res.outcome, f.gcCalls)
	}
}

func TestDaemonDoltTasksSkipWhileGCHoldsLock(t *testing.T) {
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 300 * mib}

	var taskRan []bool
	maintenanceGCExecFn = func(_ context.Context, d *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		for _, name := range []string{"dolt_backup", "dolt_remotes", "wisp_reaper", "jsonl_git_backup", "compactor_dog"} {
			release, ok := d.tryDoltTask(name)
			taskRan = append(taskRan, ok)
			if ok {
				release()
			}
		}
		return nil
	}

	d.maintenanceGCCycle([]string{"hq"}, "/unused", testGCPolicy)

	for i, ok := range taskRan {
		if ok {
			t.Errorf("task %d took the lock while a gc held it", i)
		}
	}
	if !strings.Contains(logs.String(), "dolt_backup: skipped: gc in flight") {
		t.Errorf("skip not logged:\n%s", logs.String())
	}
	// The gc released the write side.
	if release, ok := d.tryDoltTask("dolt_backup"); !ok {
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
	d, logs := gcTestDaemon(t)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		WispReaper:     &WispReaperConfig{Enabled: true},
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true},
		DoltRemotes:    &DoltRemotesConfig{Enabled: true},
	}}
	d.doltMaintMu.Lock()
	d.reapWisps()
	d.syncJsonlGitBackup()
	d.pushDoltRemotes()
	d.doltMaintMu.Unlock()
	for _, want := range []string{"wisp_reaper: skipped: gc in flight", "jsonl_git_backup: skipped: gc in flight", "dolt_remotes: skipped: gc in flight"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q:\n%s", want, logs.String())
		}
	}
}

// triggerCompactorDog's goroutine takes the read side before its cycle: with
// a gc holding the write side, the cycle neither runs nor records a run, and
// it runs on the next check once the gc is done.
func TestCompactorDogSkipsWhileGCHoldsLock(t *testing.T) {
	origCycle := compactorDogCycleFn
	t.Cleanup(func() { compactorDogCycleFn = origCycle })
	var mu sync.Mutex
	cycles := 0
	compactorDogCycleFn = func(*Daemon) {
		mu.Lock()
		cycles++
		mu.Unlock()
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return cycles
	}

	var logs bytes.Buffer
	d := compactorDogTestDaemon(t.TempDir(), &logs)
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

// syncDoltBackups skips before pouring its molecule or touching the data dir
// while a gc holds the write side. macOS only: the backup patrol returns
// before the guard on every other OS.
func TestDoltBackupSkipsWhileGCHoldsLock(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("dolt_backup runs only on darwin")
	}
	d, logs := gcTestDaemon(t)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		DoltBackup: &DoltBackupConfig{Enabled: true},
	}}
	var bdCalls []string
	d.dogPourBdFn = func(args ...string) (string, error) {
		bdCalls = append(bdCalls, strings.Join(args, " "))
		return "", errors.New("fake bd: unavailable")
	}
	d.doltMaintMu.Lock()
	d.syncDoltBackups()
	d.doltMaintMu.Unlock()

	if !strings.Contains(logs.String(), "dolt_backup: skipped: gc in flight") {
		t.Errorf("skip not logged:\n%s", logs.String())
	}
	if len(bdCalls) != 0 {
		t.Errorf("dolt_backup poured its molecule while a gc held the lock: %v", bdCalls)
	}
	if strings.Contains(logs.String(), "data dir") {
		t.Errorf("dolt_backup reached its data dir while a gc held the lock:\n%s", logs.String())
	}
}

func TestGCPausesConvoyAroundEachDatabase(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 300 * mib, "gt": 600 * mib}

	d.maintenanceGCCycle([]string{"hq", "gt"}, "/unused", testGCPolicy)
	if f.pauses != 2 || f.resumes != 2 {
		t.Errorf("pauses=%d resumes=%d, want 2/2 (once around each gc)", f.pauses, f.resumes)
	}

	d, _ = gcTestDaemon(t) // fresh state: no baseline for hq
	f2 := withGCFakes(t)
	f2.sizes = map[string]int64{"hq": 300 * mib}
	f2.pauseFails = true
	res := d.maintenanceGCCycle([]string{"hq"}, "/unused", testGCPolicy)
	if res.outcome != gcOutcomeDeferred || res.reason != "convoy poll busy" || len(f2.gcCalls) != 0 {
		t.Errorf("pause failure: outcome %v reason %q gc %v, want deferred without gc", res.outcome, res.reason, f2.gcCalls)
	}
	// The lock was released on the pause-failure path.
	if release, ok := d.tryDoltTask("x"); !ok {
		t.Error("doltMaintMu leaked on the convoy-pause failure path")
	} else {
		release()
	}
}

func TestConvoyManagerPauseResume(t *testing.T) {
	var logged []string
	m := &ConvoyManager{logger: func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}}

	// A tick in flight holds the read side: Pause times out and holds nothing.
	m.pollGate.RLock()
	if m.Pause(120 * time.Millisecond) {
		t.Fatal("Pause succeeded while a tick was in flight")
	}
	m.pollGate.RUnlock()

	if !m.Pause(time.Second) {
		t.Fatal("Pause failed with nothing in flight")
	}
	// While paused, a scan skips instead of reading Dolt.
	m.scan()
	if len(logged) != 1 || !strings.Contains(logged[0], "paused for scheduled gc") {
		t.Errorf("scan while paused logged %v", logged)
	}
	if m.pollGate.TryRLock() {
		t.Error("a poll tick could start while paused")
	}
	m.Resume()
	if !m.pollGate.TryRLock() {
		t.Error("poll tick blocked after Resume")
	} else {
		m.pollGate.RUnlock()
	}
}

// Back-to-back ticks (a tick slower than its interval re-takes the read side
// the moment it releases it) must not starve Pause: while Pause waits, no new
// tick may start, so the one in flight drains and Pause wins.
func TestConvoyManagerPauseNotStarvedByBackToBackTicks(t *testing.T) {
	m := &ConvoyManager{logger: func(string, ...interface{}) {}}

	stop := make(chan struct{})
	done := make(chan struct{})
	started := make(chan struct{})
	go func() {
		defer close(done)
		once := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			if m.tryBeginTick() {
				if !once {
					once = true
					close(started)
				}
				time.Sleep(20 * time.Millisecond) // the tick's Dolt reads
				m.pollGate.RUnlock()
				continue // the next tick is already due
			}
			time.Sleep(time.Millisecond)
		}
	}()
	<-started

	ok := m.Pause(time.Second)
	close(stop)
	if ok {
		m.Resume()
	}
	<-done
	if !ok {
		t.Fatal("Pause timed out under back-to-back ticks; want it to drain the in-flight tick and win")
	}
	if m.pausing.Load() {
		t.Error("pausing still set after Pause returned")
	}
}

// --- discovery and external servers ------------------------------------------------

func TestDiscoverMaintenanceDatabases(t *testing.T) {
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
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 300 * mib, "gt": 600 * mib}
	// The compactor list names only hq; discovery finds both.
	d.patrolConfig = gcModeConfig([]string{"hq"}, MaintenanceModeGC)

	d.runScheduledMaintenance()
	if strings.Join(f.gcCalls, ",") != "hq,gt" {
		t.Errorf("gc calls = %v, want hq,gt from discovery", f.gcCalls)
	}
}

func TestScheduledMaintenanceGCSkipsExternalServer(t *testing.T) {
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 300 * mib}
	maintenanceGCExternalFn = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
	d.patrolConfig = gcModeConfig([]string{"hq"}, MaintenanceModeGC)

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran against an external server: %v", f.gcCalls)
	}
	if !strings.Contains(logs.String(), "externally managed") {
		t.Errorf("external skip not logged:\n%s", logs.String())
	}
}

func TestMaintenanceGCExternal(t *testing.T) {
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
	if ext, why := mk(&DoltServerConfig{Enabled: true, Host: "10.0.0.5"}).maintenanceGCExternal(); !ext || !strings.Contains(why, "10.0.0.5") {
		t.Errorf("remote host: external=%v why=%q", ext, why)
	}
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false", h)
		}
	}
}

func TestDoltGCFullRefusesInvalidNameBeforeConnecting(t *testing.T) {
	d, _ := gcTestDaemon(t)
	for _, db := range []string{"", "../hq", "hq?allowAllFiles=true", ".dolt", "a b", "hq/x"} {
		if err := d.doltGCFull(context.Background(), db); err == nil || !strings.Contains(err.Error(), "invalid database name") {
			t.Errorf("doltGCFull(%q) = %v, want invalid-name error", db, err)
		}
	}
}

// --- skipped-window streak ------------------------------------------------------

func TestDeferredWindowThresholds(t *testing.T) {
	cases := map[string]int{"daily": 3, "": 3, "48h": 3, "weekly": 2, "monthly": 2, "168h": 2}
	for interval, want := range cases {
		if got := maintenanceDeferredWindowsBeforeEscalation(interval); got != want {
			t.Errorf("threshold(%q) = %d, want %d", interval, got, want)
		}
	}
	var fired []int
	for n := 1; n <= 10; n++ {
		if shouldEscalateDeferredWindows(n, 3) {
			fired = append(fired, n)
		}
	}
	if fmt.Sprint(fired) != "[3 6 9]" {
		t.Errorf("daily escalations at windows %v, want [3 6 9]", fired)
	}
}

func TestDeferredWindowStreakEscalates(t *testing.T) {
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	base := time.Date(2026, 9, 26, 4, 0, 0, 0, time.Local)
	pending := []gcCandidate{{name: "gt", size: 900 * mib}}

	window := func(i int) time.Time { return base.Add(time.Duration(i) * 24 * time.Hour) }
	for i := 0; i < 6; i++ {
		d.recordGCDeferral(window(i), "slot held by gastown/refinery", pending)
		// Still inside the window: not counted.
		d.closeDeferredGCWindow(window(i).Add(-time.Minute), "daily")
		st, _ := loadMaintenanceGCState(d.config.TownRoot)
		if st.ConsecutiveDeferredWindows != i {
			t.Fatalf("window %d counted before it closed (count=%d)", i, st.ConsecutiveDeferredWindows)
		}
		d.closeDeferredGCWindow(window(i).Add(time.Minute), "daily")
		// A second tick after close does not double count.
		d.closeDeferredGCWindow(window(i).Add(6*time.Minute), "daily")
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
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t)
	f.sizes = map[string]int64{"hq": 631 * mib}
	f.quietUntil = 0 // busy
	d.patrolConfig = gcModeConfig([]string{"hq"}, MaintenanceModeGC)

	d.runScheduledMaintenance()
	st, _ := loadMaintenanceGCState(d.config.TownRoot)
	if st.PendingDeferral == nil || len(st.PendingDeferral.Databases) != 1 || st.PendingDeferral.Databases[0].Name != "hq" {
		t.Fatalf("deferral not recorded: %+v", st.PendingDeferral)
	}
	if !st.PendingDeferral.WindowEnd.After(time.Now()) {
		t.Errorf("window end %v is not in the future", st.PendingDeferral.WindowEnd)
	}

	f.quietUntil = -1
	d.runScheduledMaintenance()
	st, _ = loadMaintenanceGCState(d.config.TownRoot)
	if st.PendingDeferral != nil || st.ConsecutiveDeferredWindows != 0 {
		t.Errorf("completed run did not reset the streak: %+v", st)
	}
}
