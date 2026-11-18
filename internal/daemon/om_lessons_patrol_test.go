package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

// omLessonsTestDaemon builds a daemon whose om_lessons patrol is enabled. Its
// in-memory last-run entry is dropped when the test ends, so no later daemon
// can inherit a completion through a recycled address.
func omLessonsTestDaemon(t *testing.T, logged io.Writer) *Daemon {
	t.Helper()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OmLessons: &agentconfig.OmLessonsConfig{Enabled: true},
		}},
	}
	t.Cleanup(d.forgetOmLessonsLastRun)
	return d
}

// TestReworkProgramOmLessonsPatrolStartsOnlyWhenEnabled: the patrol's own
// ticker exists only when daemon.patrols.om_lessons.enabled is true, and it is
// off by default. It fires on a check cadence inside the week, never on the
// 168h run interval, since a ticker set to the interval restarts its countdown
// on every daemon start (gt-ima2). A configured interval it cannot schedule
// starts no ticker and names the key once.
func TestReworkProgramOmLessonsPatrolStartsOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	var intervals []time.Duration
	ticks := make(chan time.Time)
	stops := 0
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
	}
	d.newOmLessonsTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		intervals = append(intervals, interval)
		return ticks, func() { stops++ }
	}

	if channel, stop := d.startOmLessonsTicker(); channel != nil || stop != nil {
		t.Error("a town that names no om_lessons started its ticker")
	}
	if len(intervals) != 0 {
		t.Fatalf("a town that names no om_lessons asked for tickers at %v, want none", intervals)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		OmLessons: &agentconfig.OmLessonsConfig{Enabled: false},
	}}
	if channel, stop := d.startOmLessonsTicker(); channel != nil || stop != nil {
		t.Error("a disabled om_lessons started its ticker")
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		OmLessons: &agentconfig.OmLessonsConfig{Enabled: true},
	}}
	channel, stop := d.startOmLessonsTicker()
	if channel == nil || stop == nil {
		t.Fatal("an enabled om_lessons started no ticker")
	}
	if want := shortPatrolCheckTick(168 * time.Hour); len(intervals) != 1 || intervals[0] != want {
		t.Errorf("the ticker asks for an interval of %v, want the %v check cadence of the 168h default", intervals, want)
	}
	stop()
	if stops != 1 {
		t.Errorf("the stop func ran %d times, want the ticker stopped once", stops)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		OmLessons: &agentconfig.OmLessonsConfig{Enabled: true, Interval: "48h"},
	}}
	channel, stop = d.startOmLessonsTicker()
	if channel == nil || stop == nil {
		t.Fatal("an enabled om_lessons with an interval started no ticker")
	}
	if want := shortPatrolCheckTick(48 * time.Hour); len(intervals) != 2 || intervals[1] != want {
		t.Errorf("the ticker asks for an interval of %v, want the %v check cadence of the configured 48h", intervals, want)
	}
	stop()

	// An interval under the floor starts no ticker rather than one the town did
	// not ask for, and names the key once.
	var logged bytes.Buffer
	d.logger = log.New(&logged, "", 0)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		OmLessons: &agentconfig.OmLessonsConfig{Enabled: true, Interval: "30m"},
	}}
	if channel, stop := d.startOmLessonsTicker(); channel != nil || stop != nil {
		t.Error("an interval under the floor started a ticker")
	}
	if len(intervals) != 2 {
		t.Errorf("an interval under the floor asked for a ticker at %v, want none", intervals[2:])
	}
	if !strings.Contains(logged.String(), "patrols.om_lessons.interval") {
		t.Errorf("an interval under the floor logged %q, want the interval key named", logged.String())
	}

	// The production ticker, with no seam: the patrol still gets a channel and
	// a stop func, so Run's defer has a real ticker to stop.
	plain := omLessonsTestDaemon(t, io.Discard)
	channel, stop = plain.startOmLessonsTicker()
	if channel == nil || stop == nil {
		t.Fatal("the patrol started no ticker without a ticker seam")
	}
	stop()
}

// omLessonsCountingDaemon builds a daemon over townRoot whose run seam counts
// its passes, on the clock the caller gives so a test can move the week on.
func omLessonsCountingDaemon(t *testing.T, townRoot string, clk clockwork.Clock, logged io.Writer, calls *atomic.Int32) *Daemon {
	t.Helper()
	d := omLessonsTestDaemon(t, logged)
	d.config.TownRoot = townRoot
	d.clock = clk
	d.omLessonsRun = func(context.Context, time.Time) ([]omlessons.Proposal, error) {
		calls.Add(1)
		return nil, nil
	}
	return d
}

// omLessonsOneCheck drives one tick and waits for the pass it started.
func omLessonsOneCheck(t *testing.T, d *Daemon) {
	t.Helper()
	d.startOmLessonsTick()
	d.omLessonsCycles.Wait()
}

// TestReworkProgramOmLessonsAWeekRecordSkipsTheRestOfTheWeek: a pass records
// the week it ran, so a later check inside the week does not call the run seam
// again — on the same daemon, and on a fresh patrol value over the same town,
// which can only know the week was had from the record it persisted.
func TestReworkProgramOmLessonsAWeekRecordSkipsTheRestOfTheWeek(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	fake := clockwork.NewFakeClockAt(base)
	var calls atomic.Int32

	first := omLessonsCountingDaemon(t, t.TempDir(), fake, io.Discard, &calls)
	omLessonsOneCheck(t, first)
	if got := calls.Load(); got != 1 {
		t.Fatalf("the first check called the run seam %d times, want the week's pass", got)
	}

	fake.Advance(2 * time.Hour)
	omLessonsOneCheck(t, first)
	if got := calls.Load(); got != 1 {
		t.Errorf("a check inside the week ran the pass again (%d calls), want the recorded week to hold", got)
	}

	restarted := omLessonsCountingDaemon(t, first.config.TownRoot, fake, io.Discard, &calls)
	omLessonsOneCheck(t, restarted)
	if got := calls.Load(); got != 1 {
		t.Errorf("a restarted patrol ran the week again (%d calls), want the persisted record to hold", got)
	}
}

// TestReworkProgramOmLessonsNextWeekRunsAgain: a check a full interval after
// the recorded pass runs the seam again.
func TestReworkProgramOmLessonsNextWeekRunsAgain(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	fake := clockwork.NewFakeClockAt(base)
	var calls atomic.Int32
	d := omLessonsCountingDaemon(t, t.TempDir(), fake, io.Discard, &calls)

	omLessonsOneCheck(t, d)
	if got := calls.Load(); got != 1 {
		t.Fatalf("the first check called the run seam %d times, want the week's pass", got)
	}

	fake.Advance(168 * time.Hour)
	omLessonsOneCheck(t, d)
	if got := calls.Load(); got != 2 {
		t.Errorf("a check in the next week called the run seam %d times, want the week's pass run again", got)
	}
}

// TestReworkProgramOmLessonsBrokenRecordWarnsAndRuns: a last-run record that
// cannot be read runs the week and says so, and the pass it ran keeps the check
// from repeating the week — the record it wrote replaces a corrupt file, and its
// in-memory completion holds when the write cannot land at all (gt-4nqej).
func TestReworkProgramOmLessonsBrokenRecordWarnsAndRuns(t *testing.T) {
	t.Parallel()

	t.Run("a corrupt record", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		var calls atomic.Int32
		d := omLessonsCountingDaemon(t, t.TempDir(), nil, &logged, &calls)

		path := patrolLastRunPath(d.config.TownRoot)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("make the daemon directory: %v", err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatalf("write a corrupt last-run record: %v", err)
		}

		omLessonsOneCheck(t, d)
		if got := calls.Load(); got != 1 {
			t.Errorf("a check over a corrupt record called the run seam %d times, want the pass run", got)
		}
		if !strings.Contains(logged.String(), "WARNING: last-run state unreadable") {
			t.Errorf("a corrupt record logged %q, want a WARNING naming the unreadable state", logged.String())
		}

		omLessonsOneCheck(t, d)
		if got := calls.Load(); got != 1 {
			t.Errorf("the check after the pass ran the week again (%d calls), want the repaired record to hold", got)
		}
	})

	t.Run("a record path that cannot be read", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		var calls atomic.Int32
		townRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(townRoot, "daemon"), []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("block the daemon directory: %v", err)
		}
		d := omLessonsCountingDaemon(t, townRoot, nil, &logged, &calls)

		omLessonsOneCheck(t, d)
		if got := calls.Load(); got != 1 {
			t.Errorf("a check over an unreadable record called the run seam %d times, want the pass run", got)
		}
		if !strings.Contains(logged.String(), "WARNING: last-run state unreadable") {
			t.Errorf("an unreadable record logged %q, want a WARNING naming the unreadable state", logged.String())
		}

		omLessonsOneCheck(t, d)
		if got := calls.Load(); got != 1 {
			t.Errorf("the check after the pass ran the week again (%d calls), want the in-memory completion to hold", got)
		}
	})
}

// TestReworkProgramOmLessonsTickRefusesWhatItCannotSchedule: a tick on an
// inactive patrol, or on an interval the patrol cannot schedule, runs no pass
// and names the key — rather than running on a cadence the town never asked for.
func TestReworkProgramOmLessonsTickRefusesWhatItCannotSchedule(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	counting := func(context.Context, time.Time) ([]omlessons.Proposal, error) {
		calls.Add(1)
		return nil, nil
	}

	t.Run("inactive", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		unconfigured := &Daemon{
			config: &Config{TownRoot: t.TempDir()},
			logger: log.New(&logged, "", 0),
		}
		unconfigured.omLessonsRun = counting
		omLessonsOneCheck(t, unconfigured)
		if got := calls.Load(); got != 0 {
			t.Errorf("a tick on an inactive patrol called the run seam %d times, want none", got)
		}
		// A tick can outlive the reload that turned the patrol off, and a patrol
		// the town turned off reads no record and prints no diagnosis.
		if logged.Len() != 0 {
			t.Errorf("a tick on an inactive patrol logged %q, want nothing", logged.String())
		}
	})

	t.Run("an interval under the floor", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := omLessonsTestDaemon(t, &logged)
		d.omLessonsRun = counting
		d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OmLessons: &agentconfig.OmLessonsConfig{Enabled: true, Interval: "30m"},
		}}
		omLessonsOneCheck(t, d)
		if got := calls.Load(); got != 0 {
			t.Errorf("a tick on an unschedulable interval called the run seam %d times, want none", got)
		}
		if !strings.Contains(logged.String(), "patrols.om_lessons.interval") {
			t.Errorf("a tick on an unschedulable interval logged %q, want the interval key named", logged.String())
		}
	})
}

// TestReworkProgramOmLessonsEachTickCallsTheRunSeamOnce: one started tick calls
// the run seam once, and the pass it built is what the tick reports.
func TestReworkProgramOmLessonsEachTickCallsTheRunSeamOnce(t *testing.T) {
	t.Parallel()

	t.Run("one tick, one call", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := omLessonsTestDaemon(t, &logged)
		var calls atomic.Int32
		d.omLessonsRun = func(context.Context, time.Time) ([]omlessons.Proposal, error) {
			calls.Add(1)
			return []omlessons.Proposal{{ID: "fail-open"}, {ID: "missing-refusal-test"}}, nil
		}

		d.startOmLessonsTick()
		d.omLessonsCycles.Wait()

		if got := calls.Load(); got != 1 {
			t.Errorf("a tick called the run seam %d times, want once", got)
		}
		if d.omLessonsRunning.Load() {
			t.Error("the guard stayed set after the pass ended, so no later tick could run")
		}
		if !strings.Contains(logged.String(), "the pass built 2 proposal(s)") {
			t.Errorf("the tick logged %q, want the two proposals it built", logged.String())
		}
	})

	t.Run("a tick while one runs is skipped", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := omLessonsTestDaemon(t, &logged)
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		var calls atomic.Int32
		d.omLessonsRun = func(context.Context, time.Time) ([]omlessons.Proposal, error) {
			calls.Add(1)
			reached <- struct{}{}
			<-release
			return nil, nil
		}

		d.startOmLessonsTick()
		waitForSignal(t, reached, "the first pass never reached the run seam")
		d.startOmLessonsTick()
		if !strings.Contains(logged.String(), "previous pass still running, skipping") {
			t.Errorf("the second tick was not logged as skipped: %q", logged.String())
		}
		close(release)
		d.omLessonsCycles.Wait()
		if got := calls.Load(); got != 1 {
			t.Errorf("two starting ticks called the run seam %d times, want once", got)
		}
	})
}

// TestReworkProgramOmLessonsFailedPassIsLoggedNotCounted: a pass that fails is
// logged rather than counted as a clean one.
func TestReworkProgramOmLessonsFailedPassIsLoggedNotCounted(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := omLessonsTestDaemon(t, &logged)
	d.omLessonsRun = func(context.Context, time.Time) ([]omlessons.Proposal, error) {
		return nil, errors.New("the review records are unreadable")
	}

	d.triggerOmLessons()

	if !strings.Contains(logged.String(), "the review records are unreadable") {
		t.Errorf("a failed pass logged %q, want the error named", logged.String())
	}
	if strings.Contains(logged.String(), "proposal(s)") {
		t.Errorf("a failed pass logged a proposal count: %q", logged.String())
	}
}

// TestReworkProgramOmLessonsStopJoinsTheRunningPass: Run's return stops the
// ticker and waits for the pass it started, and a pass still running at the
// bound is logged and left.
func TestReworkProgramOmLessonsStopJoinsTheRunningPass(t *testing.T) {
	t.Parallel()

	blocking := func(t *testing.T, logged io.Writer) (*Daemon, chan struct{}, func()) {
		t.Helper()
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		d := omLessonsTestDaemon(t, logged)
		d.omLessonsRun = func(context.Context, time.Time) ([]omlessons.Proposal, error) {
			reached <- struct{}{}
			<-release
			return nil, nil
		}
		t.Cleanup(func() {
			unblock()
			d.omLessonsCycles.Wait()
		})
		return d, reached, unblock
	}

	t.Run("the pass the ticker started is joined", func(t *testing.T) {
		t.Parallel()
		d, reached, unblock := blocking(t, io.Discard)
		d.omLessonsJoinGrace = time.Hour

		d.startOmLessonsTick()
		waitForSignal(t, reached, "the pass never reached the run seam")

		stopped := make(chan struct{})
		stopCalled := make(chan struct{})
		go func() {
			d.stopOmLessonsPatrol(func() { close(stopCalled) })
			close(stopped)
		}()
		waitForSignal(t, stopCalled, "the stop func never ran")
		select {
		case <-stopped:
			t.Fatal("stopOmLessonsPatrol returned while the pass still ran")
		default:
		}
		unblock()
		waitForSignal(t, stopped, "stopOmLessonsPatrol never returned after the pass finished")
		if d.omLessonsRunning.Load() {
			t.Error("the join returned with the pass's guard still set")
		}
	})

	t.Run("a pass past the bound is logged and left", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d, reached, unblock := blocking(t, &logged)
		d.omLessonsJoinGrace = 30 * time.Millisecond

		d.startOmLessonsTick()
		waitForSignal(t, reached, "the pass never reached the run seam")

		stopped := make(chan struct{})
		go func() {
			d.stopOmLessonsPatrol(nil)
			close(stopped)
		}()
		waitForSignal(t, stopped, "stopOmLessonsPatrol never returned past the bound")
		if !strings.Contains(logged.String(), "still running") {
			t.Errorf("a pass left past the bound was not logged: %q", logged.String())
		}
		unblock()
		d.omLessonsCycles.Wait()
	})
}

// omLessonsFinding is one finding of a fake review record: the severity and the
// title om writes it with.
func omLessonsFinding(severity, title string) land.ReviewFindingText {
	return land.ReviewFindingText{Severity: severity, Title: title}
}

// omLessonsFakeSources is a pass's inputs over named records and rules, so a
// test drives the pass without a review log on disk.
func omLessonsFakeSources(records []land.ReviewRecord, rules map[string]bool) omLessonsSources {
	return omLessonsSources{
		Records: func(time.Time) ([]land.ReviewRecord, error) { return records, nil },
		Rules:   func() (map[string]bool, error) { return rules, nil },
	}
}

// omLessonsReviewRecord is one fake review record: a bead, the landing attempt
// it belongs to and the findings it carries at at.
func omLessonsReviewRecord(bead string, at time.Time, attempt int, findings ...land.ReviewFindingText) land.ReviewRecord {
	return land.ReviewRecord{BeadID: bead, Rig: "gastown", At: at, Attempt: attempt, Verdict: land.VerdictApprove, FindingText: findings}
}

// TestReworkProgramOmLessonsRunBuildsOneProposalPerRejectionClass: one pass over
// a fake record source yields one proposal per class of the table, with the
// class's counts and three example beads, a too-few class carrying no coverage,
// and the coverage text each class's rule reads.
func TestReworkProgramOmLessonsRunBuildsOneProposalPerRejectionClass(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	recent := at.AddDate(0, 0, -3)
	prior := at.AddDate(0, 0, -10)

	records := []land.ReviewRecord{
		// Six findings on six beads: one bead carries a third attempt, so the
		// class burned the two rounds past its first attempt.
		omLessonsReviewRecord("gt-a", recent, 1, omLessonsFinding("major", "fail-open: a failed read reads as clean")),
		omLessonsReviewRecord("gt-b", recent, 3, omLessonsFinding("major", "fail-open: a failed read reads as clean")),
		omLessonsReviewRecord("gt-c", recent, 1, omLessonsFinding("major", "fail-open: a failed read reads as clean")),
		omLessonsReviewRecord("gt-d", recent, 1, omLessonsFinding("major", "fail-open: a failed read reads as clean")),
		omLessonsReviewRecord("gt-e", recent, 1, omLessonsFinding("major", "fail-open: a failed read reads as clean")),
		omLessonsReviewRecord("gt-f", recent, 1, omLessonsFinding("major", "fail-open: a failed read reads as clean")),
		// The prior window's one finding.
		omLessonsReviewRecord("gt-z", prior, 1, omLessonsFinding("minor", "fail-open: a failed read reads as clean")),
	}
	for _, bead := range []string{"gt-r1", "gt-r2", "gt-r3", "gt-r4", "gt-r5"} {
		records = append(records, omLessonsReviewRecord(bead, recent, 1, omLessonsFinding("minor", "the refusal branch has no test")))
	}
	for _, bead := range []string{"gt-n1", "gt-n2", "gt-n3", "gt-n4", "gt-n5"} {
		records = append(records, omLessonsReviewRecord(bead, recent, 1, omLessonsFinding("minor", "the test passes with the fix removed")))
	}
	records = append(records,
		omLessonsReviewRecord("gt-d1", recent, 1, omLessonsFinding("minor", "the doc says the pass refuses")),
		omLessonsReviewRecord("gt-d2", recent, 1, omLessonsFinding("minor", "the doc says the pass refuses")),
	)
	// A class no numbered rule covers: its coverage is a gap once it has enough
	// findings to say.
	for _, bead := range []string{"gt-w1", "gt-w2", "gt-w3", "gt-w4", "gt-w5"} {
		records = append(records, omLessonsReviewRecord(bead, recent, 1, omLessonsFinding("minor", "the loop sleeps on the wall clock")))
	}

	d := omLessonsTestDaemon(t, io.Discard)
	d.omLessonsRun = omLessonsPass(omLessonsFakeSources(records, map[string]bool{"1": true, "3": true}))

	proposals, err := d.runOmLessonsTick(context.Background(), at)
	if err != nil {
		t.Fatalf("the pass over a fake record source: %v", err)
	}
	if got, want := len(proposals), len(omlessons.ClassNames()); got != want {
		t.Fatalf("the pass built %d proposal(s), want one for each of the table's %d classes", got, want)
	}
	byID := map[string]omlessons.Proposal{}
	for _, p := range proposals {
		if _, ok := omlessons.ClassNames()[p.ID]; !ok {
			t.Errorf("the pass proposed %q, which is no class of the table", p.ID)
		}
		byID[p.ID] = p
	}

	failOpen, ok := byID["fail-open"]
	if !ok {
		t.Fatal("the pass built no proposal for fail-open")
	}
	if want := (omlessons.ClassCounts{Findings: 6, Beads: 6, Rounds: 2}); failOpen.Current != want {
		t.Errorf("fail-open's current counts are %+v, want %+v", failOpen.Current, want)
	}
	if want := (omlessons.ClassCounts{Findings: 1, Beads: 1}); failOpen.Prior != want {
		t.Errorf("fail-open's prior counts are %+v, want %+v", failOpen.Prior, want)
	}
	if got, want := strings.Join(failOpen.Examples, ","), "gt-a,gt-b,gt-c"; got != want {
		t.Errorf("fail-open's examples are %q, want the first three beads %q", got, want)
	}
	if failOpen.TooFew {
		t.Error("fail-open reads as too few with six findings in the window")
	}
	if want := "violated-anyway: rule #1 covers it and it still appears"; failOpen.Coverage != want {
		t.Errorf("fail-open's coverage is %q, want %q", failOpen.Coverage, want)
	}

	if got, want := byID["missing-refusal-test"].Coverage, "violated-anyway: rule #3 covers it and it still appears"; got != want {
		t.Errorf("missing-refusal-test's coverage is %q, want %q", got, want)
	}
	if got, want := byID["no-red-when-removed"].Coverage, "gap: the table names rule #2 and the directive has no rule #2"; got != want {
		t.Errorf("no-red-when-removed's coverage is %q, want %q", got, want)
	}
	if got, want := byID["wall-clock-wait"].Coverage, "gap: no numbered rule covers it"; got != want {
		t.Errorf("wall-clock-wait's coverage is %q, want %q", got, want)
	}

	tooFew, ok := byID["doc-contradicts-code"]
	if !ok {
		t.Fatal("the pass built no proposal for doc-contradicts-code")
	}
	if !tooFew.TooFew {
		t.Error("a class with two findings in the window does not read as too few")
	}
	if got := tooFew.Current.Findings; got != 2 {
		t.Errorf("doc-contradicts-code counts %d finding(s), want the two it has", got)
	}
	if tooFew.Coverage != "" {
		t.Errorf("a too-few class carries coverage %q, want none", tooFew.Coverage)
	}
}

// TestReworkProgramOmLessonsRunReadsTheTownsOwnRecords: a daemon with no run
// seam wired reads the town's own review log and directive files, so a pass
// over a real town builds the week's proposals from what it wrote, and a record
// outside both windows counts nothing.
func TestReworkProgramOmLessonsRunReadsTheTownsOwnRecords(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	directives := filepath.Join(town, "directives")
	if err := os.MkdirAll(directives, 0o755); err != nil {
		t.Fatalf("make the directives directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directives, "polecat.md"), []byte("# What om rejects\n\n## 1. Failure is not success (fail-open)\n\nA failure must stop the pass.\n"), 0o644); err != nil {
		t.Fatalf("write polecat.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directives, "_common.md"), []byte("## 3. Test the branch that refuses\n\nEvery refusal branch needs a row.\n"), 0o644); err != nil {
		t.Fatalf("write _common.md: %v", err)
	}

	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	file := land.TownReviewsFile(town)
	add := func(bead string, at time.Time, title string) {
		t.Helper()
		rec := omLessonsReviewRecord(bead, at, 1, omLessonsFinding("minor", title))
		if err := file.Append(rec); err != nil {
			t.Fatalf("append the record for %s: %v", bead, err)
		}
	}
	for _, bead := range []string{"gt-a", "gt-b", "gt-c", "gt-d", "gt-e"} {
		add(bead, at.AddDate(0, 0, -1), "fail-open: a failed read reads as clean")
	}
	for _, bead := range []string{"gt-r1", "gt-r2", "gt-r3", "gt-r4", "gt-r5"} {
		add(bead, at.AddDate(0, 0, -2), "the refusal branch has no test")
	}
	add("gt-prior", at.AddDate(0, 0, -10), "fail-open: a failed read reads as clean")
	add("gt-old", at.AddDate(0, 0, -30), "fail-open: a failed read reads as clean")

	d := omLessonsTestDaemon(t, io.Discard)
	d.config.TownRoot = town

	proposals, err := d.runOmLessonsTick(context.Background(), at)
	if err != nil {
		t.Fatalf("the pass over the town's own records: %v", err)
	}
	byID := map[string]omlessons.Proposal{}
	for _, p := range proposals {
		byID[p.ID] = p
	}

	if got := byID["fail-open"].Current.Findings; got != 5 {
		t.Errorf("fail-open counts %d finding(s), want the week's five and not the record 30 days old", got)
	}
	if got := byID["fail-open"].Prior.Findings; got != 1 {
		t.Errorf("fail-open counts %d finding(s) in the prior window, want the one written there", got)
	}
	if want := "violated-anyway: rule #1 covers it and it still appears"; byID["fail-open"].Coverage != want {
		t.Errorf("fail-open's coverage is %q, want %q, read from polecat.md", byID["fail-open"].Coverage, want)
	}
	if got := byID["missing-refusal-test"].Current.Findings; got != 5 {
		t.Errorf("missing-refusal-test counts %d finding(s), want the week's five", got)
	}
	if want := "violated-anyway: rule #3 covers it and it still appears"; byID["missing-refusal-test"].Coverage != want {
		t.Errorf("missing-refusal-test's coverage is %q, want %q, read from _common.md", byID["missing-refusal-test"].Coverage, want)
	}
}

// TestReworkProgramOmLessonsRunRefusesAnUnreadableInput: every input the pass
// cannot read is an error, never a week with no findings.
func TestReworkProgramOmLessonsRunRefusesAnUnreadableInput(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	readable := func() omLessonsSources {
		return omLessonsFakeSources(nil, map[string]bool{"1": true})
	}
	// town builds a daemon over a temp town that holds the two directive files
	// when their text is non-empty, and the town's review log when withLog is
	// set.
	town := func(t *testing.T, polecat, common string, withLog bool) *Daemon {
		t.Helper()
		root := t.TempDir()
		if polecat != "" || common != "" {
			dir := filepath.Join(root, "directives")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("make the directives directory: %v", err)
			}
			for name, text := range map[string]string{"polecat.md": polecat, "_common.md": common} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
		}
		if withLog {
			reviews := land.TownReviewsFile(root)
			if err := os.MkdirAll(filepath.Dir(reviews.Path), 0o755); err != nil {
				t.Fatalf("make the landings directory: %v", err)
			}
			if err := os.WriteFile(reviews.Path, nil, 0o644); err != nil {
				t.Fatalf("write the review log: %v", err)
			}
		}
		d := omLessonsTestDaemon(t, io.Discard)
		d.config.TownRoot = root
		return d
	}

	cases := []struct {
		name string
		run  func(t *testing.T) ([]omlessons.Proposal, error)
		want string
	}{
		{
			name: "no sources at all",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				return omLessonsPass(omLessonsSources{})(context.Background(), at)
			},
			want: "the pass has no sources",
		},
		{
			name: "a window under a day",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				sources := readable()
				sources.Days = -1
				return omLessonsPass(sources)(context.Background(), at)
			},
			want: "want at least one day",
		},
		{
			name: "a canceled pass",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return omLessonsPass(readable())(ctx, at)
			},
			want: "canceled",
		},
		{
			name: "the records cannot be read",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				sources := readable()
				sources.Records = func(time.Time) ([]land.ReviewRecord, error) {
					return nil, errors.New("the review log is locked")
				}
				return omLessonsPass(sources)(context.Background(), at)
			},
			want: "the review log is locked",
		},
		{
			name: "the directives cannot be read",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				sources := readable()
				sources.Rules = func() (map[string]bool, error) {
					return nil, errors.New("polecat.md is unreadable")
				}
				return omLessonsPass(sources)(context.Background(), at)
			},
			want: "polecat.md is unreadable",
		},
		{
			name: "a directive set with no numbered rule",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				sources := readable()
				sources.Rules = func() (map[string]bool, error) { return map[string]bool{}, nil }
				return omLessonsPass(sources)(context.Background(), at)
			},
			want: "no numbered rules",
		},
		{
			name: "a daemon with no config",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				return (&Daemon{logger: log.New(io.Discard, "", 0)}).runOmLessonsTick(context.Background(), at)
			},
			want: "names no town",
		},
		{
			name: "a daemon with no town root",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				d := omLessonsTestDaemon(t, io.Discard)
				d.config.TownRoot = ""
				return d.runOmLessonsTick(context.Background(), at)
			},
			want: "no town root is named",
		},
		{
			name: "a town with no review log",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				d := town(t, "## 1. Failure is not success\n", "## 3. Test the branch that refuses\n", false)
				return d.runOmLessonsTick(context.Background(), at)
			},
			want: "review log",
		},
		{
			name: "a review log below a file",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				root := t.TempDir()
				if err := os.WriteFile(filepath.Join(root, ".runtime"), []byte("not a directory"), 0o644); err != nil {
					t.Fatalf("block the town's runtime directory: %v", err)
				}
				d := omLessonsTestDaemon(t, io.Discard)
				d.config.TownRoot = root
				return d.runOmLessonsTick(context.Background(), at)
			},
			want: "not a directory",
		},
		{
			name: "a town with no directives directory",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				d := town(t, "", "", true)
				return d.runOmLessonsTick(context.Background(), at)
			},
			want: "reading the directive",
		},
		{
			name: "a directive with no numbered rule heading",
			run: func(t *testing.T) ([]omlessons.Proposal, error) {
				d := town(t, "# What om rejects\n\nNo numbered rule here.\n", "The shared rules, unnumbered.\n", true)
				return d.runOmLessonsTick(context.Background(), at)
			},
			want: "no numbered rules",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			proposals, err := c.run(t)
			if err == nil {
				t.Fatalf("the pass accepted %s and proposed %d proposal(s), want an error", c.name, len(proposals))
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the pass failed with %q, want the error to name %q", err, c.want)
			}
			if proposals != nil {
				t.Errorf("a refused pass returned %d proposal(s), want none", len(proposals))
			}
		})
	}
}
