package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
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

// awaitCompactorDogIdle blocks until the cycle a trigger started, if any, has
// finished, its last-run record included.
func awaitCompactorDogIdle(t *testing.T, d *Daemon) {
	t.Helper()
	d.compactorDogCycles.Wait()
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
	t.Parallel()
	const interval = 24 * time.Hour
	const step = 3 * time.Hour

	townRoot := t.TempDir()

	var mu sync.Mutex
	var runs []time.Time

	start := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 10; i++ {
		at := start.Add(time.Duration(i) * step)
		d := compactorDogTestDaemon(townRoot, nil)
		d.clock = clockwork.NewFakeClockAt(at)
		// The cycle itself needs Dolt; the subject here is the schedule.
		d.compactorDogCycleFn = func() bool {
			mu.Lock()
			defer mu.Unlock()
			runs = append(runs, at)
			return true
		}
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
	t.Parallel()
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

	d := compactorDogTestDaemon(townRoot, &buf)
	d.compactorDogCycleFn = func() bool {
		mu.Lock()
		defer mu.Unlock()
		ran = true
		return true
	}
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
	t.Parallel()
	townRoot := t.TempDir()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := savePatrolLastRun(townRoot, "compactor_dog", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed last run: %v", err)
	}

	var buf bytes.Buffer
	var mu sync.Mutex
	ran := false

	d := compactorDogTestDaemon(townRoot, &buf)
	d.clock = clockwork.NewFakeClockAt(now)
	d.compactorDogCycleFn = func() bool {
		mu.Lock()
		defer mu.Unlock()
		ran = true
		return true
	}

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

// TestCompactorDogFailedCycleNotRecorded is the regression test for gt-4uxs: a
// cycle that finds nothing to inspect (or fails every inspection) must not be
// recorded as a completed run. Recording it anyway would hide the failure
// behind the full 24h interval before the next attempt, instead of retrying on
// the next 15-minute check.
func TestCompactorDogFailedCycleNotRecorded(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	var buf bytes.Buffer
	var mu sync.Mutex
	cycles := 0

	d := compactorDogTestDaemon(townRoot, &buf)
	d.compactorDogCycleFn = func() bool {
		mu.Lock()
		defer mu.Unlock()
		cycles++
		return false
	}

	// Two triggers in a row: with no last-run recorded after the first, the
	// second must find the patrol due again rather than waiting out the
	// interval.
	d.triggerCompactorDog()
	awaitCompactorDogIdle(t, d)
	d.triggerCompactorDog()
	awaitCompactorDogIdle(t, d)

	mu.Lock()
	gotCycles := cycles
	mu.Unlock()

	if gotCycles != 2 {
		t.Fatalf("cycles ran = %d, want 2 (a failed cycle must not suppress the next check)", gotCycles)
	}
	if _, found, err := loadPatrolLastRun(townRoot, "compactor_dog"); err != nil {
		t.Fatalf("load last run: %v", err)
	} else if found {
		t.Error("last-run record exists after a failed cycle; a failed cycle must not be recorded as completed")
	}
	if !strings.Contains(buf.String(), "cycle failed") {
		t.Errorf("no log line about the failed cycle; log:\n%s", buf.String())
	}
}

// TestRecordCompactorDogRunPersists checks the record itself: a completed cycle
// must leave a last-run time the next process can read.
func TestRecordCompactorDogRunPersists(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	at := time.Date(2026, 9, 22, 9, 30, 0, 0, time.UTC)

	d := compactorDogTestDaemon(townRoot, nil)
	d.clock = clockwork.NewFakeClockAt(at)
	d.recordCompactorDogRun()

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
	t.Parallel()
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

	d.compactorDogCycleFn = func() bool { record("cycle"); return true }

	d.triggerCompactorDog()
	awaitCompactorDogIdle(t, d)

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	if len(got) != 2 || got[0] != "dolt" || got[1] != "cycle" {
		t.Fatalf("cycle order = %v, want the Dolt bring-up before the cycle", got)
	}
}

// --- the database set, the two lines, and the size measure --------------------

// compactorRunDaemon builds a daemon whose compactor cycle files its alarms
// into a recorder rather than running gt, so a whole cycle is observable
// offline: no SQL, no server, no escalation beads.
func compactorRunDaemon(townRoot string, logBuf *bytes.Buffer) (*Daemon, *notifyfake.Recorder) {
	d := compactorDogTestDaemon(townRoot, logBuf)
	rec := notifyfake.New()
	d.notifier = rec
	return d, rec
}

// compactorClearReason returns the reason the report withdraws one alert key
// under, "" when it does not withdraw it.
func compactorClearReason(rep compactorReport, key string) string {
	for _, a := range rep.cleared {
		if a.key == key {
			return a.reason
		}
	}
	return ""
}

// TestCompactorDogFindingsChecksEveryDatabase is the case the patrol was
// missing: a database whose commit count is over the line, a database whose
// store is over the size line, and one under both, all in a single cycle. The
// database that is clean sits one byte under the size line, so "none below it
// do" is tested at the boundary rather than with a comfortable margin.
//
// The size line is compactorSizeThreshold itself, the constant production
// passes (runCompactorDogWith), so what is pinned here is the real 1 GiB line
// and not a stand-in.
//
// The commit count over its line is the run's advisory and not an alarm
// (gt-3mu6q): it is reported, and its key is withdrawn as advisory so an
// escalation an older build opened for it is closed.
func TestCompactorDogFindingsChecksEveryDatabase(t *testing.T) {
	t.Parallel()
	const commitThreshold = 20000

	// The commit count the patrol never reported: hq on 2026-10-12 (gt-7yejc).
	const commits = 31335

	p := compactorProbes{
		databases: func() ([]string, error) { return []string{"busy", "fat", "fine"}, nil },
		commits: func(db string) (int, error) {
			if db == "busy" {
				return commits, nil
			}
			return 10, nil
		},
		size: func(db string) (int64, error) {
			if db == "fat" {
				return compactorSizeThreshold, nil
			}
			return compactorSizeThreshold - 1, nil
		},
	}

	rep := compactorDogFindings(p, commitThreshold, compactorSizeThreshold)

	if got := slices.Sorted(slices.Values(compactorAlarmKeys(rep.raised))); !slices.Equal(got, []string{
		"compactor_dog:fat:size",
	}) {
		t.Errorf("raised = %v, want fat's size alarm alone: a commit count over its line is an advisory", got)
	}
	if got := slices.Sorted(slices.Values(compactorAlarmKeys(rep.cleared))); !slices.Equal(got, []string{
		"compactor_dog:busy:commits",
		"compactor_dog:busy:size",
		"compactor_dog:fat:commits",
		"compactor_dog:fine:commits",
		"compactor_dog:fine:size",
	}) {
		t.Errorf("cleared = %v", got)
	}
	// busy is over the commit line, so its withdrawal is the advisory one.
	if reason := compactorClearReason(rep, "compactor_dog:busy:commits"); reason != compactorAdvisoryReason {
		t.Errorf("busy:commits withdrawal reason = %q, want %q", reason, compactorAdvisoryReason)
	}
	if reason := compactorClearReason(rep, "compactor_dog:fine:commits"); reason != compactorUnderReason {
		t.Errorf("fine:commits withdrawal reason = %q, want %q", reason, compactorUnderReason)
	}
	if len(rep.advisories) != 1 || rep.advisories[0].db != "busy" || rep.advisories[0].commits != commits ||
		rep.advisories[0].threshold != commitThreshold {
		t.Errorf("advisories = %+v, want busy alone at %d commits against %d", rep.advisories, commits, commitThreshold)
	}
	if !slices.Equal(rep.names, []string{"busy", "fat", "fine"}) {
		t.Errorf("checked = %v, want every discovered database", rep.names)
	}
	if len(rep.unread) != 0 || rep.sizeUnchecked || rep.discoveryErr != nil {
		t.Errorf("report carries a failure: %+v", rep)
	}
}

// TestCompactorDogFindingsReportsUnreadableDatabase pins the other half of the
// contract: a database the cycle cannot read is reported, not skipped in
// silence. Its alarms must also survive — an unread measurement is not
// evidence the condition cleared, and withdrawing the alarm would hide exactly
// the database that needs looking at.
func TestCompactorDogFindingsReportsUnreadableDatabase(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection refused")

	p := compactorProbes{
		databases: func() ([]string, error) { return []string{"broken", "checked"}, nil },
		commits: func(db string) (int, error) {
			if db == "broken" {
				return 0, boom
			}
			return 10, nil
		},
		size: func(string) (int64, error) { return 1024, nil },
	}

	rep := compactorDogFindings(p, 20000, compactorSizeThreshold)

	if len(rep.unread) != 1 || !strings.Contains(rep.unread[0], "broken") ||
		!strings.Contains(rep.unread[0], boom.Error()) {
		t.Errorf("unread = %v, want broken named with its error", rep.unread)
	}
	if got := compactorAlarmKeys(rep.cleared); slices.Contains(got, "compactor_dog:broken:commits") {
		t.Errorf("cleared = %v: an unread commit count must not clear its alarm", got)
	}
	// The measurement that did read is still reported and still clears.
	if !slices.Contains(compactorAlarmKeys(rep.cleared), "compactor_dog:checked:commits") {
		t.Errorf("cleared = %v, want checked:commits", compactorAlarmKeys(rep.cleared))
	}
}

// TestCompactorDogFindingsSaysWhenSizesAreUnreadable is the report side of a
// cycle with no size probe: commits are still checked, no size alarm exists to
// raise or withdraw, and the absence is recorded rather than reading as "no
// database is large". Which town takes that branch is pinned by the probes
// tests below.
func TestCompactorDogFindingsSaysWhenSizesAreUnreadable(t *testing.T) {
	t.Parallel()
	p := compactorProbes{
		databases: func() ([]string, error) { return []string{"hq"}, nil },
		commits:   func(string) (int, error) { return 900, nil },
		size:      nil,
	}

	rep := compactorDogFindings(p, 20000, compactorSizeThreshold)

	if !rep.sizeUnchecked {
		t.Error("sizeUnchecked = false although no size was read")
	}
	if got := compactorAlarmKeys(rep.cleared); !slices.Equal(got, []string{"compactor_dog:hq:commits"}) {
		t.Errorf("cleared = %v: with no size read there is no size alarm to withdraw", got)
	}
}

// TestCompactorDogCycleAdvisesOnCommitsAndEscalatesOnSize is the cycle-level
// contract of gt-3mu6q: one cycle with the commit count over its line and the
// size under its own raises no escalation, logs the count as an advisory and
// records it for the status line; a second cycle with the size over its line
// escalates exactly as before. The withdrawals carry the per-database key
// `gt escalate` dedupes on, so the same condition over successive cycles
// leaves one open escalation, not one per cycle (gt-vwry).
func TestCompactorDogCycleAdvisesOnCommitsAndEscalatesOnSize(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	townRoot := t.TempDir()
	d, rec := compactorRunDaemon(townRoot, &buf)

	probes := func(commits int, size int64) compactorProbes {
		return compactorProbes{
			databases: func() ([]string, error) { return []string{"hq"}, nil },
			commits:   func(string) (int, error) { return commits, nil },
			size:      func(string) (int64, error) { return size, nil },
		}
	}

	// Commits over the line at 31335, the reading that opened the daily
	// escalation (hq-9kasl), with a store 1024 bytes large.
	if ok := d.runCompactorDogWith(probes(31335, 1024)); !ok {
		t.Fatalf("a cycle that read every database reported itself unclean; log:\n%s", buf.String())
	}

	if got := rec.Escalations(); len(got) != 0 {
		t.Fatalf("escalations = %+v, want none: the commit count is advisory, not an alarm", got)
	}

	// Both keys are withdrawn: the size alarm as a measurement back under its
	// line, the commit alarm as advisory — which is what closes an escalation
	// an older build opened for the count.
	clears := rec.Clears()
	if len(clears) != 2 {
		t.Fatalf("clears = %+v, want the two alarms withdrawn, one per reason", clears)
	}
	reason := map[string]string{}
	for _, c := range clears {
		if len(c.Fingerprints) == 1 {
			reason[c.Fingerprints[0]] = c.Reason
		}
	}
	if reason["compactor_dog:hq:commits"] != compactorAdvisoryReason {
		t.Errorf("commit withdrawal reason = %q, want %q", reason["compactor_dog:hq:commits"], compactorAdvisoryReason)
	}
	if reason["compactor_dog:hq:size"] != compactorUnderReason {
		t.Errorf("size withdrawal reason = %q, want %q", reason["compactor_dog:hq:size"], compactorUnderReason)
	}

	// The advisory is on the cycle's log line and in the record gt status
	// reads.
	if !strings.Contains(buf.String(), "ADVISORY") || !strings.Contains(buf.String(), "31335 commits") {
		t.Errorf("the cycle does not log the count as an advisory; log:\n%s", buf.String())
	}
	advisories := LoadCompactorAdvisories(townRoot, time.Now())
	if len(advisories) != 1 || advisories[0].Database != "hq" || advisories[0].Commits != 31335 {
		t.Errorf("recorded advisories = %+v, want hq at 31335", advisories)
	}

	// Second cycle: the store is over the size line, and the commit count is
	// still over its own. The size escalates; the count still does not.
	if ok := d.runCompactorDogWith(probes(31335, compactorSizeThreshold)); !ok {
		t.Fatalf("second cycle reported itself unclean; log:\n%s", buf.String())
	}
	got := rec.Escalations()
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want the size line's one alone", got)
	}
	if key := got[0].Escalation.Fingerprint; key != "compactor_dog:hq:size" {
		t.Errorf("escalation key = %q, want compactor_dog:hq:size", key)
	}

	// Third cycle: back under both lines, so nothing is recorded for the
	// status line and the commit alarm is withdrawn as a measurement under
	// its line again.
	if ok := d.runCompactorDogWith(probes(900, 1024)); !ok {
		t.Fatalf("third cycle reported itself unclean; log:\n%s", buf.String())
	}
	if got := LoadCompactorAdvisories(townRoot, time.Now()); len(got) != 0 {
		t.Errorf("recorded advisories = %+v, want none under the line", got)
	}
	clears = rec.Clears()
	last := clears[len(clears)-1]
	// Both keys are under their lines now and share one reason, so they are
	// one withdrawal.
	if !slices.Equal(last.Fingerprints, []string{"compactor_dog:hq:commits", "compactor_dog:hq:size"}) ||
		last.Reason != compactorUnderReason {
		t.Errorf("third cycle cleared %v as %q, want both keys under %q", last.Fingerprints, last.Reason, compactorUnderReason)
	}

	// The cycle's own log line names the size line it applied: production
	// passes compactorSizeThreshold, not a test's value.
	if !strings.Contains(buf.String(), "size_threshold=1024.0MiB") {
		t.Errorf("cycle does not log the 1 GiB size line; log:\n%s", buf.String())
	}
}

// TestCompactorAdvisoryRecordIsStaleAfterTwoRuns pins the reader's freshness
// rule: a record from a compactor that has stopped running must not keep
// claiming a database is over its line on every status line.
func TestCompactorAdvisoryRecordIsStaleAfterTwoRuns(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	err := saveCompactorAdvisories(townRoot, at, []compactorAdvisory{{db: "gt", commits: 31906, threshold: 20000}})
	if err != nil {
		t.Fatalf("saveCompactorAdvisories: %v", err)
	}

	if got := LoadCompactorAdvisories(townRoot, at.Add(time.Hour)); len(got) != 1 || got[0].Database != "gt" {
		t.Errorf("fresh record = %+v, want the gt advisory", got)
	}
	if got := LoadCompactorAdvisories(townRoot, at.Add(CompactorAdvisoryStale)); len(got) != 0 {
		t.Errorf("record at the stale age = %+v, want none", got)
	}

	// A cycle that finds nothing over the line replaces the record rather than
	// leaving the previous one to be read as current.
	if err := saveCompactorAdvisories(townRoot, at.Add(time.Hour), nil); err != nil {
		t.Fatalf("second saveCompactorAdvisories: %v", err)
	}
	if got := LoadCompactorAdvisories(townRoot, at.Add(2*time.Hour)); len(got) != 0 {
		t.Errorf("recorded advisories = %+v, want none", got)
	}
}

// TestLoadCompactorAdvisoriesWithoutARecord pins the reader's silence: no
// record, an unreadable one, or a town with no daemon directory reads as no
// advisory — never as an error a status line has to render.
func TestLoadCompactorAdvisoriesWithoutARecord(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	if got := LoadCompactorAdvisories(townRoot, time.Now()); len(got) != 0 {
		t.Errorf("advisories with no record = %+v, want none", got)
	}

	path := compactorStatePath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := LoadCompactorAdvisories(townRoot, time.Now()); len(got) != 0 {
		t.Errorf("advisories from a corrupt record = %+v, want none", got)
	}
}

// TestCompactorDogCycleFailsOnDiscoveredFailure checks that a cycle which
// could not inspect everything reports itself unclean, so runCompactorDog's
// caller does not record a last-run time and the next check retries rather
// than waiting out the interval (gt-4uxs).
func TestCompactorDogCycleFailsOnDiscoveredFailure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d, _ := compactorRunDaemon(t.TempDir(), &buf)

	p := compactorProbes{
		databases: func() ([]string, error) { return []string{"hq"}, nil },
		commits:   func(string) (int, error) { return 0, errors.New("connection refused") },
		size:      func(string) (int64, error) { return 1024, nil },
	}

	if ok := d.runCompactorDogWith(p); ok {
		t.Error("a cycle with an unreadable database reported itself clean")
	}
	if !strings.Contains(buf.String(), "UNREAD") || !strings.Contains(buf.String(), "connection refused") {
		t.Errorf("the unreadable database is not reported; log:\n%s", buf.String())
	}

	// A discovery failure is not an empty town either.
	p.databases = func() ([]string, error) { return nil, errors.New("no such directory") }
	if ok := d.runCompactorDogWith(p); ok {
		t.Error("a cycle that could not discover any database reported itself clean")
	}
}

// TestCompactorDatabasesDiscoversAndFiltersScratch pins the database set: the
// discovered databases, minus test cruft, and a configured list left alone.
func TestCompactorDatabasesDiscoversAndFiltersScratch(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := compactorDogTestDaemon(townRoot, nil)

	d.maint.gcDatabases = func(string) ([]string, error) {
		return []string{"beads_pt_x", "hq", "testdb_9f2c"}, nil
	}
	got, err := d.compactorDatabases()
	if err != nil {
		t.Fatalf("compactorDatabases: %v", err)
	}
	if !slices.Equal(got, []string{"hq"}) {
		t.Errorf("discovered = %v, want hq alone (scratch databases dropped)", got)
	}

	// Discovery failure reaches the caller; it is not an empty database set.
	d.maint.gcDatabases = func(string) ([]string, error) { return nil, errors.New("boom") }
	if _, err := d.compactorDatabases(); err == nil {
		t.Error("a discovery failure was swallowed")
	}

	// An operator's own list is used as given, and it is the whole answer: the
	// server list is not consulted for a server whose storage is elsewhere
	// either (a config that names databases is an instruction).
	d.patrolConfig.Patrols.CompactorDog.Databases = []string{"hq", "beads"}
	got, err = d.compactorDatabases()
	if err != nil {
		t.Fatalf("compactorDatabases: %v", err)
	}
	if !slices.Equal(got, []string{"hq", "beads"}) {
		t.Errorf("configured = %v, want the configured list", got)
	}
	got, err = d.compactorServerDatabases()
	if err != nil {
		t.Fatalf("compactorServerDatabases: %v", err)
	}
	if !slices.Equal(got, []string{"hq", "beads"}) {
		t.Errorf("configured server list = %v, want the configured list", got)
	}
}

// writeDoltDB lays out a Dolt database directory under dataDir: a .dolt/noms
// holding a table file of storeBytes and, when journalBytes is non-zero, the
// live journal of that size.
func writeDoltDB(t *testing.T, dataDir, db string, storeBytes, journalBytes int) {
	t.Helper()
	noms := filepath.Join(dataDir, db, ".dolt", "noms")
	if err := os.MkdirAll(noms, 0o755); err != nil {
		t.Fatalf("create %s: %v", noms, err)
	}
	write := func(name string, size int) {
		if err := os.WriteFile(filepath.Join(noms, name), bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("4vrlfm9eipve1in8d57vm4t5s2po57jq.darc", storeBytes)
	if journalBytes > 0 {
		write(doltJournalFileName, journalBytes)
		write(doltJournalIndexFileName, 128)
	}
}

// TestCompactorDogSizeLineReadsTheStoreNotTheJournal is the 2026-10-12 reading
// (gt-7yejc) as a test. It walks the production path — discovery of the data
// directory, the maintenance size measure, the 1 GiB line — over a temp tree,
// so the databases are found the way a real cycle finds them and measured by
// the code that read 1,708.9 MiB against hq's 235 MB on disk.
//
// journalled holds a 4 MiB journal over a 64-byte table file: over the 1 MiB
// line this test applies, and under it in every part that is actually the
// store. It must not raise. big is the converse, and must.
//
// The line is scaled from compactorSizeThreshold because the alternative is a
// gigabyte file; the threshold is a parameter of compactorDogFindings for
// exactly this reason, and TestCompactorDogFindingsChecksEveryDatabase pins
// the real value at the boundary.
func TestCompactorDogSizeLineReadsTheStoreNotTheJournal(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	dataDir := filepath.Join(townRoot, ".dolt-data")

	writeDoltDB(t, dataDir, "big", 2*int(mib), 0)
	writeDoltDB(t, dataDir, "journalled", 64, 4*int(mib))

	d := compactorDogTestDaemon(townRoot, nil)
	p := d.compactorDogProbes()
	p.commits = func(string) (int, error) { return 10, nil } // no SQL in a test

	rep := compactorDogFindings(p, 20000, mib)

	if rep.discoveryErr != nil {
		t.Fatalf("discovery: %v", rep.discoveryErr)
	}
	if !slices.Equal(rep.names, []string{"big", "journalled"}) {
		t.Fatalf("discovered = %v, want both databases under the data directory", rep.names)
	}
	if got := compactorAlarmKeys(rep.raised); !slices.Equal(got, []string{"compactor_dog:big:size"}) {
		t.Errorf("raised = %v, want big's size alarm alone — journalled is a journal, not a store", got)
	}
	// The measured bytes themselves, not the rounded log line: journalled is
	// its 64-byte table file and nothing else.
	for _, tc := range []struct {
		db   string
		want int64
	}{
		{"big", 2 * mib},
		{"journalled", 64},
	} {
		got, err := p.size(tc.db)
		if err != nil {
			t.Fatalf("size(%s): %v", tc.db, err)
		}
		if got != tc.want {
			t.Errorf("size(%s) = %d, want %d", tc.db, got, tc.want)
		}
	}
}

// TestCompactorDogProbesTakeTheDatabaseListFromTheServerWhenTheDataDirIsNotHere
// is the regression test for the pairing this patrol nearly shipped: with the
// size probe dropped for a server whose storage is not this host's, the
// database list still came from a walk of the data directory — the directory
// that is not here. The walk failed, the cycle failed with it, no run was
// recorded, and the commit alarm this patrol exists to raise was never
// evaluated: the patrol would have retried every 15 minutes forever.
func TestCompactorDogProbesTakeTheDatabaseListFromTheServerWhenTheDataDirIsNotHere(t *testing.T) {
	t.Parallel()
	local := func() ([]string, error) { return []string{"from-the-data-dir"}, nil }
	server := func() ([]string, error) { return []string{"from-the-server"}, nil }
	commits := func(string) (int, error) { return 0, nil }
	size := func(string) (int64, error) { return 1, nil }
	sources := compactorDatabaseSources{local: local, server: server}

	here := compactorProbesFor(false, sources, commits, size)
	got, err := here.databases()
	if err != nil {
		t.Fatalf("storage on this host: %v", err)
	}
	if !slices.Equal(got, []string{"from-the-data-dir"}) {
		t.Errorf("storage on this host: databases = %v, want the data directory's list", got)
	}
	if here.size == nil {
		t.Error("storage on this host: no size probe")
	}

	elsewhere := compactorProbesFor(true, sources, commits, size)
	got, err = elsewhere.databases()
	if err != nil {
		t.Fatalf("storage elsewhere: %v", err)
	}
	if !slices.Equal(got, []string{"from-the-server"}) {
		t.Errorf("storage elsewhere: databases = %v, want the server's own list", got)
	}
	if elsewhere.size != nil {
		t.Error("storage elsewhere: a size probe for a disk this host cannot read")
	}
}

// TestCompactorDogProbesSkipSizesForAnExternalServer drives the wiring that
// chooses between the two branches. The databases probe is not called — it
// would open a connection — so this pins the shape, and the branch above pins
// what each shape reads.
func TestCompactorDogProbesSkipSizesForAnExternalServer(t *testing.T) {
	t.Parallel()
	d := compactorDogTestDaemon(t.TempDir(), nil)
	d.doltServer = &DoltServerManager{config: &DoltServerConfig{Enabled: true, External: true}}

	if p := d.compactorDogProbes(); p.size != nil {
		t.Error("size probe present although the server's storage is not on this host")
	}
}

// TestCompactorRealDatabasesDropsCatalogsAndScratch covers the names no alarm
// may carry, from either source: the server's own catalogs, and test cruft.
func TestCompactorRealDatabasesDropsCatalogsAndScratch(t *testing.T) {
	t.Parallel()
	got := compactorRealDatabases([]string{
		"information_schema", "mysql", "dolt_cluster", "hq", "testdb_9f2c", "beads_pt_x",
	})
	if !slices.Equal(got, []string{"hq"}) {
		t.Errorf("compactorRealDatabases = %v, want hq alone", got)
	}
}

// TestCompactorDogCycleSaysWhenSizesWereNotChecked pins the one line that
// distinguishes the two reports a cycle with no size probe could otherwise
// give: "no database is large" and "no size was read" look identical in the
// counts, and only one of them is news.
func TestCompactorDogCycleSaysWhenSizesWereNotChecked(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d, _ := compactorRunDaemon(t.TempDir(), &buf)

	p := compactorProbes{
		databases: func() ([]string, error) { return []string{"hq"}, nil },
		commits:   func(string) (int, error) { return 900, nil },
	}

	if ok := d.runCompactorDogWith(p); !ok {
		t.Fatalf("a commit-only cycle reported itself unclean; log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "sizes=unchecked") {
		t.Errorf("cycle did not report the unchecked sizes; log:\n%s", buf.String())
	}
}

// compactorOpenDB authenticates with the town's configured Dolt password; a
// DSN that drops it makes a password-protected server answer access denied
// (gt-napz3).
func TestCompactorOpenDBCarriesPassword(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings string
		want     string
	}{
		{"password set", `{"password":"secret"}`, "root:secret@tcp("},
		{"no password", `{}`, "root@tcp("},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeTownDoltSettings(t, townRoot, tc.settings)
			d := compactorDogTestDaemon(townRoot, nil)
			if got := d.compactorDSN("hq"); !strings.HasPrefix(got, tc.want) {
				t.Errorf("compactorDSN = %q, want prefix %q", got, tc.want)
			}
		})
	}
}

// compactorDSN reaches the town's Dolt host through the same bracketed
// host:port as the other Dolt clients, so an IPv6 host such as ::1 dials
// instead of failing with "too many colons" (gt-80xqv).
func TestCompactorDSNBracketsIPv6Host(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		host string
		want string
	}{
		{"ipv4", "127.0.0.1", "root@tcp(127.0.0.1:3307)/hq?"},
		{"ipv6 loopback", "::1", "root@tcp([::1]:3307)/hq?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
				t.Fatal(err)
			}
			townJSON := fmt.Sprintf(`{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","registry":{"rigs":{}},"dolt":{"host":%q,"port":3307}}`, tc.host)
			if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(townJSON), 0o644); err != nil {
				t.Fatal(err)
			}
			d := compactorDogTestDaemon(townRoot, nil)
			if got := d.compactorDSN("hq"); !strings.HasPrefix(got, tc.want) {
				t.Errorf("compactorDSN = %q, want prefix %q", got, tc.want)
			}
		})
	}
}
