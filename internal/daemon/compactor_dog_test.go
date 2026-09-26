package daemon

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// compactorDogTestDaemon builds a daemon rooted at townRoot with the
// compactor_dog patrol enabled. logBuf may be nil for a silent logger.
func compactorDogTestDaemon(townRoot string, logBuf *bytes.Buffer) *Daemon {
	var logger *log.Logger
	if logBuf != nil {
		logger = log.New(logBuf, "", 0)
	} else {
		logger = log.New(io.Discard, "", 0)
	}
	return &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: logger,
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				CompactorDog: &CompactorDogConfig{Enabled: true},
			},
		},
	}
}

// awaitCompactorDogIdle blocks until no cycle is in flight. A cycle sets
// compactorDogRunning false as its last action, after the last-run time has
// been recorded, so returning from here means the record is written.
func awaitCompactorDogIdle(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d.compactorDogMu.Lock()
		running := d.compactorDogRunning
		d.compactorDogMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("compactor_dog cycle did not finish")
}

// TestCompactorDogFiresAcrossRestarts is the regression test for gt-ima2: the
// patrol used to be driven by a 24h ticker created at daemon startup, so every
// restart reset the countdown, and on a host restarting the daemon 5-12 times a
// day no cycle ever ran.
//
// The test walks 27 hours of wall clock in 3-hour steps, building a fresh
// Daemon on each step (same town root, no in-memory carry-over), so no single
// process lives anywhere near the 24h interval.
func TestCompactorDogFiresAcrossRestarts(t *testing.T) {
	const interval = 24 * time.Hour
	const step = 3 * time.Hour

	townRoot := t.TempDir()

	var mu sync.Mutex
	var runs []time.Time

	origCycle, origNow := compactorDogCycleFn, compactorDogNow
	defer func() {
		compactorDogCycleFn = origCycle
		compactorDogNow = origNow
	}()

	// The cycle itself needs Dolt; the subject here is the schedule.
	compactorDogCycleFn = func(*Daemon) {
		mu.Lock()
		defer mu.Unlock()
		runs = append(runs, compactorDogNow())
	}

	start := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 10; i++ {
		at := start.Add(time.Duration(i) * step)
		compactorDogNow = func() time.Time { return at }

		d := compactorDogTestDaemon(townRoot, nil)
		if got := compactorDogInterval(d.patrolConfig); got != interval {
			t.Fatalf("test assumes a %v run interval, config gives %v", interval, got)
		}
		d.triggerCompactorDog() // may be a no-op when not due
		awaitCompactorDogIdle(t, d)
	}

	mu.Lock()
	got := append([]time.Time(nil), runs...)
	mu.Unlock()

	// One cycle at the start (no record yet) and one when the interval elapses
	// 24h later — reached across eight intervening restarts.
	if len(got) != 2 {
		t.Fatalf("cycles over 27h of restarts = %d, want 2; runs at %v", len(got), got)
	}
	if !got[0].Equal(start) {
		t.Errorf("first cycle at %v, want %v", got[0], start)
	}
	if want := start.Add(interval); !got[1].Equal(want) {
		t.Errorf("second cycle at %v, want %v", got[1], want)
	}
}

// TestCompactorDogRunsWhenLastRunStateUnreadable pins the fail-loud rule: a
// corrupt last-run file must not be read as "not due", because a monitor that
// stays silent is the failure, whatever broke it.
func TestCompactorDogRunsWhenLastRunStateUnreadable(t *testing.T) {
	townRoot := t.TempDir()
	path := patrolLastRunPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("create daemon dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	var buf bytes.Buffer
	var mu sync.Mutex
	var ran bool

	origCycle := compactorDogCycleFn
	defer func() { compactorDogCycleFn = origCycle }()
	compactorDogCycleFn = func(*Daemon) {
		mu.Lock()
		defer mu.Unlock()
		ran = true
	}

	d := compactorDogTestDaemon(townRoot, &buf)
	d.triggerCompactorDog()
	awaitCompactorDogIdle(t, d)

	mu.Lock()
	gotRan := ran
	mu.Unlock()

	if !gotRan {
		t.Errorf("cycle did not run with an unreadable last-run state; log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "unreadable") {
		t.Errorf("no log line about the unreadable last-run state; log:\n%s", buf.String())
	}
}

// TestCompactorDogSkipsWhenNotDue covers the other half of the schedule: inside
// the interval the patrol must stay down, and say so once per process.
func TestCompactorDogSkipsWhenNotDue(t *testing.T) {
	townRoot := t.TempDir()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := savePatrolLastRun(townRoot, "compactor_dog", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed last run: %v", err)
	}

	var buf bytes.Buffer
	var mu sync.Mutex
	ran := false

	origCycle, origNow := compactorDogCycleFn, compactorDogNow
	defer func() {
		compactorDogCycleFn = origCycle
		compactorDogNow = origNow
	}()
	compactorDogCycleFn = func(*Daemon) {
		mu.Lock()
		defer mu.Unlock()
		ran = true
	}
	compactorDogNow = func() time.Time { return now }

	d := compactorDogTestDaemon(townRoot, &buf)

	// A first check, then a second one: the skip must hold across both, and the
	// first must be visible in the log.
	d.triggerCompactorDog()
	awaitCompactorDogIdle(t, d)
	d.triggerCompactorDog()
	awaitCompactorDogIdle(t, d)

	mu.Lock()
	gotRan := ran
	mu.Unlock()

	if gotRan {
		t.Error("cycle ran although the patrol was not due")
	}
	if !strings.Contains(buf.String(), "not due") {
		t.Errorf("the not-due decision is not logged; log:\n%s", buf.String())
	}
}

// TestRecordCompactorDogRunPersists checks the record itself: a completed cycle
// must leave a last-run time the next process can read.
func TestRecordCompactorDogRunPersists(t *testing.T) {
	townRoot := t.TempDir()
	at := time.Date(2026, 9, 22, 9, 30, 0, 0, time.UTC)

	origNow := compactorDogNow
	defer func() { compactorDogNow = origNow }()
	compactorDogNow = func() time.Time { return at }

	compactorDogTestDaemon(townRoot, nil).recordCompactorDogRun()

	lastRun, found, err := loadPatrolLastRun(townRoot, "compactor_dog")
	if err != nil {
		t.Fatalf("load last run: %v", err)
	}
	if !found {
		t.Fatal("no last-run record after a completed cycle")
	}
	if !lastRun.Equal(at) {
		t.Errorf("recorded %v, want %v", lastRun, at)
	}
}

// TestCompactorDogBringsDoltUpBeforeItsCycle is the regression test for
// gt-ox6c, a major finding on the gt-wisp-yi3r review: Run()'s startup catch-up
// dispatches the first cycle before the first heartbeat, which is the step that
// starts Dolt, so on a cold start the cycle opened a SQL connection to a server
// that was not listening and counted nothing. The trigger has to bring the
// server up before the cycle runs.
//
// The Dolt manager is external with a seamed health check, so the bring-up is
// observable without a server: EnsureRunning probes health instead of spawning
// one, and the probe records that it ran.
func TestCompactorDogBringsDoltUpBeforeItsCycle(t *testing.T) {
	townRoot := t.TempDir()
	d := compactorDogTestDaemon(townRoot, nil)

	var mu sync.Mutex
	var order []string
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}

	d.doltServer = &DoltServerManager{
		config:        &DoltServerConfig{Enabled: true, External: true},
		healthCheckFn: func() error { record("dolt"); return nil },
	}

	origCycle := compactorDogCycleFn
	defer func() { compactorDogCycleFn = origCycle }()
	compactorDogCycleFn = func(*Daemon) { record("cycle") }

	d.triggerCompactorDog()
	awaitCompactorDogIdle(t, d)

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	if len(got) != 2 || got[0] != "dolt" || got[1] != "cycle" {
		t.Fatalf("cycle order = %v, want the Dolt bring-up before the cycle", got)
	}
}
