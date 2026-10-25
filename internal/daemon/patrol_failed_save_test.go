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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// The patrols that decide due-ness with a persisted last-run time keep an
// in-memory one as well, so a save that fails cannot make the patrol due on
// every heartbeat (gt-th2am, gt-cekkh). Each test makes the record unwritable
// and triggers the patrol twice inside its interval, then once after it. The
// jev patrols' tests are in jev_failed_save_test.go.

// unwritableLastRun makes the town's last-run record unwritable: a directory
// stands where the file belongs, so the rest of the daemon directory (the
// install lock rebuild_om takes there) still works.
func unwritableLastRun(t *testing.T, town string) {
	t.Helper()
	if err := os.MkdirAll(patrolLastRunPath(town), 0o755); err != nil {
		t.Fatal(err)
	}
}

// waitFlagClear blocks until a patrol's single-flight guard is released.
func waitFlagClear(t *testing.T, running *atomic.Bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for running.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the cycle did not finish")
		}
		runtime.Gosched()
	}
}

// ageLastRun dates a wall-clock patrol's last run back by two intervals, so its
// rerun falls due. wisp_reaper, checkpoint_dog, jsonl_git_backup and
// overrun_guard read time.Now and round the elapsed time to the minute, so the
// tests date the record back instead of waiting one out.
func ageLastRun(t *testing.T, mem *patrolLastRunMemory, interval time.Duration) {
	t.Helper()
	mem.record(time.Now().Add(-2 * interval))
}

func noFeed(string, map[string]interface{}) error { return nil }

func TestEventsPruneFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unwritableLastRun(t, town)
	path := filepath.Join(town, events.EventsFile)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	line := func(age time.Duration) string {
		return fmt.Sprintf(`{"ts":%q,"type":"mail"}`+"\n", clk.Now().Add(-age).Format(time.RFC3339))
	}
	appendLine := func(s string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clk,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{EventsPrune: &EventsPruneConfig{
			Enabled: true, IntervalStr: "1h", MaxAgeStr: "2h",
		}}},
	}

	b := line(90 * time.Minute)
	appendLine(line(3*time.Hour) + b)
	d.pruneEventsLog()
	if got := read(); got != b {
		t.Fatalf("after the first prune = %q, want %q", got, b)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	// b is now past max_age, but the interval has not elapsed.
	clk.Advance(45 * time.Minute)
	d.pruneEventsLog()
	if got := read(); got != b {
		t.Errorf("a prune ran again inside the interval after a failed save: %q", got)
	}

	clk.Advance(20 * time.Minute)
	d.pruneEventsLog()
	if got := read(); got != "" {
		t.Errorf("no prune ran once the interval had passed: %q", got)
	}
}

func TestPatrolScanGHGatesFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unwritableLastRun(t, town)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	logs := &bytes.Buffer{}
	d := &Daemon{logger: log.New(logs, "", 0), config: &Config{TownRoot: town}, clock: clk}
	checks := 0
	h := &patrolScanHost{d: d, gateCheck: func(string, string) (string, error) {
		checks++
		return "", nil
	}}

	d.patrolScanGHGates(h, []string{"myr"})
	d.patrolScanGHGates(h, []string{"myr"})

	// The town and the rig are one check each, once.
	if checks != 2 {
		t.Errorf("%d gate checks across two ticks after a failed save, want 2 (one per database)", checks)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	clk.Advance(ghGateInterval)
	d.patrolScanGHGates(h, []string{"myr"})
	if checks != 4 {
		t.Errorf("%d gate checks after the interval, want 4", checks)
	}
}

func TestPatrolScanRogueBDFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unwritableLastRun(t, town)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	logs := &bytes.Buffer{}
	d := &Daemon{logger: log.New(logs, "", 0), config: &Config{TownRoot: town}, clock: clk}

	d.patrolScanRogueBD()
	d.patrolScanRogueBD()

	if n := strings.Count(logs.String(), "rogue bd check:"); n != 1 {
		t.Errorf("the rogue bd check ran %d times inside one interval after a failed save, want 1:\n%s", n, logs)
	}

	clk.Advance(rogueBDInterval)
	d.patrolScanRogueBD()
	if n := strings.Count(logs.String(), "rogue bd check:"); n != 2 {
		t.Errorf("the rogue bd check ran %d times after the interval, want 2", n)
	}
}

func TestWispReaperFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unwritableLastRun(t, town)
	logs := &bytes.Buffer{}
	d := &Daemon{
		config:    &Config{TownRoot: town},
		logger:    log.New(logs, "", 0),
		dogFeedFn: noFeed,
		// A name the reaper refuses to open, so the cycle reaches no database.
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{Enabled: true, Databases: []string{"not a database name"}},
		}},
	}

	d.triggerWispReaper()
	waitFlagClear(t, &d.wispReaperRunning)
	d.triggerWispReaper()
	waitFlagClear(t, &d.wispReaperRunning)

	if n := strings.Count(logs.String(), "dog_cycle: wisp_reaper outcome="); n != 1 {
		t.Errorf("wisp_reaper ran %d cycles inside one interval after a failed save, want 1:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	ageLastRun(t, &d.wispReaperRun, wispReaperInterval(d.patrolConfig))
	d.triggerWispReaper()
	waitFlagClear(t, &d.wispReaperRunning)
	if n := strings.Count(logs.String(), "dog_cycle: wisp_reaper outcome="); n != 2 {
		t.Errorf("wisp_reaper ran %d cycles after the interval, want 2:\n%s", n, logs)
	}
}

func TestJsonlGitBackupFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unwritableLastRun(t, town)
	if err := os.MkdirAll(filepath.Join(town, ".dolt-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	d := &Daemon{
		config:    &Config{TownRoot: town},
		logger:    log.New(logs, "", 0),
		notifier:  notifyfake.New(),
		dogFeedFn: noFeed,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, Databases: []string{"hq"}},
		}},
	}
	useGitfake(t, d)
	exports := 0
	d.jsonlExportFn = func(string, string, string, bool) (int, error) {
		exports++
		return 0, errors.New("export skipped by the test")
	}

	d.triggerJsonlGitBackup()
	waitFlagClear(t, &d.jsonlGitBackupRunning)
	d.triggerJsonlGitBackup()
	waitFlagClear(t, &d.jsonlGitBackupRunning)

	if exports != 1 {
		t.Errorf("jsonl_git_backup exported %d times inside one interval after a failed save, want 1", exports)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	ageLastRun(t, &d.jsonlGitBackupRun, jsonlGitBackupInterval(d.patrolConfig))
	d.triggerJsonlGitBackup()
	waitFlagClear(t, &d.jsonlGitBackupRunning)
	if exports != 2 {
		t.Errorf("jsonl_git_backup exported %d times after the interval, want 2", exports)
	}
}

func TestCheckpointDogFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unwritableLastRun(t, town)
	logs := &bytes.Buffer{}
	d := &Daemon{
		config:       &Config{TownRoot: town},
		logger:       log.New(logs, "", 0),
		dogFeedFn:    noFeed,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{CheckpointDog: &CheckpointDogConfig{Enabled: true}}},
	}

	d.triggerCheckpointDog()
	waitFlagClear(t, &d.checkpointDogRunning)
	d.triggerCheckpointDog()
	waitFlagClear(t, &d.checkpointDogRunning)

	if n := strings.Count(logs.String(), "checkpoint_dog: starting cycle"); n != 1 {
		t.Errorf("checkpoint_dog ran %d cycles inside one interval after a failed save, want 1:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	ageLastRun(t, &d.checkpointDogRun, checkpointDogInterval(d.patrolConfig))
	d.triggerCheckpointDog()
	waitFlagClear(t, &d.checkpointDogRunning)
	if n := strings.Count(logs.String(), "checkpoint_dog: starting cycle"); n != 2 {
		t.Errorf("checkpoint_dog ran %d cycles after the interval, want 2:\n%s", n, logs)
	}
}

func TestGitHygieneFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unwritableLastRun(t, town)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	logs := &bytes.Buffer{}
	d := &Daemon{
		config:       &Config{TownRoot: town},
		logger:       log.New(logs, "", 0),
		clock:        clk,
		dogFeedFn:    noFeed,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{GitHygiene: &PatrolConfig{Enabled: true}}},
	}

	d.triggerGitHygiene()
	waitFlagClear(t, &d.gitHygieneRunning)
	d.triggerGitHygiene()
	waitFlagClear(t, &d.gitHygieneRunning)

	if n := strings.Count(logs.String(), "dog_cycle: git_hygiene outcome="); n != 1 {
		t.Errorf("git_hygiene ran %d cycles inside one interval after a failed save, want 1:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	clk.Advance(defaultGitHygieneInterval)
	d.triggerGitHygiene()
	waitFlagClear(t, &d.gitHygieneRunning)
	if n := strings.Count(logs.String(), "dog_cycle: git_hygiene outcome="); n != 2 {
		t.Errorf("git_hygiene ran %d cycles after the interval, want 2", n)
	}
}

func TestOverrunGuardFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unwritableLastRun(t, town)
	logs := &bytes.Buffer{}
	d := &Daemon{
		config:       &Config{TownRoot: town},
		logger:       log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{Enabled: true}}},
	}
	// A cycle records its completion time, saved or not, so a moved time is the
	// mark that a cycle ran.
	d.triggerOverrunGuard()
	waitFlagClear(t, &d.overrunGuardRunning)
	first := d.overrunGuardRun.last()
	if first.IsZero() {
		t.Fatalf("the first trigger ran no cycle:\n%s", logs)
	}
	d.triggerOverrunGuard()
	waitFlagClear(t, &d.overrunGuardRunning)
	if got := d.overrunGuardRun.last(); !got.Equal(first) {
		t.Errorf("overrun_guard ran again inside its interval after a failed save (last run %s -> %s)", first, got)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	ageLastRun(t, &d.overrunGuardRun, overrunGuardInterval(d.patrolConfig))
	aged := d.overrunGuardRun.last()
	d.triggerOverrunGuard()
	waitFlagClear(t, &d.overrunGuardRunning)
	if !d.overrunGuardRun.last().After(aged) {
		t.Error("overrun_guard did not run once the interval had passed")
	}
}

func TestTierSweepFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, rec, _, logs := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	unwritableLastRun(t, d.config.TownRoot)
	rec.green()
	var head atomic.Value
	var reads atomic.Int32
	head.Store("abc123")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
		reads.Add(1)
		return head.Load().(string), nil
	}
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	d.triggerTierSweep()
	waitFlagClear(t, &d.tierSweepRunning)
	first := reads.Load()
	if first == 0 || len(rec.stages) == 0 {
		t.Fatalf("the first trigger ran no sweep:\n%s", logs)
	}
	d.triggerTierSweep()
	waitFlagClear(t, &d.tierSweepRunning)

	// A second sweep reads origin/main's head again, whether or not it moved.
	if got := reads.Load(); got != first {
		t.Errorf("tier_sweep ran again inside its interval after a failed save (%d -> %d reads of main)", first, got)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	// A moved main, so the sweep has something new to run.
	head.Store("def456")
	d.clock.(*clockwork.FakeClock).Advance(defaultTierSweepInterval)
	d.triggerTierSweep()
	waitFlagClear(t, &d.tierSweepRunning)
	if reads.Load() == first {
		t.Error("tier_sweep did not run once the interval had passed")
	}
}

func TestRebuildOMFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, _, clk := rebuildOMTown(t)
	unwritableLastRun(t, d.config.TownRoot)
	cli := rebuildOMCli(t, d, rebuildOMHeadSHA, omInstalled(rebuildOMHeadSHA))
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildOM: &PatrolConfig{Enabled: true}}}
	logs := &bytes.Buffer{}
	d.logger = log.New(io.MultiWriter(logs), "", 0)

	d.triggerRebuildOM()
	waitFlagClear(t, &d.rebuildOMRunning)
	d.triggerRebuildOM()
	waitFlagClear(t, &d.rebuildOMRunning)

	if n := len(installCalls(cli)); n != 1 {
		t.Errorf("rebuild_om ran %d installs inside one interval after a failed save, want 1", n)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	clk.Advance(defaultRebuildOMInterval)
	d.triggerRebuildOM()
	waitFlagClear(t, &d.rebuildOMRunning)
	if n := len(installCalls(cli)); n != 2 {
		t.Errorf("rebuild_om ran %d installs after the interval, want 2", n)
	}
}
