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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

// omLessonsTestDaemon builds a daemon whose om_lessons patrol is enabled. Its
// in-memory last-run entry is dropped when the test ends, so no later daemon
// can inherit a completion through a recycled address. Its proposals are filed
// to a fresh beadsfake, so no pass reaches the town's bd.
func omLessonsTestDaemon(t *testing.T, logged io.Writer) *Daemon {
	t.Helper()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OmLessons: &agentconfig.OmLessonsConfig{Enabled: true},
		}},
		openNoticeBeads: func() notifyBeadWriter { return beadsfake.New() },
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

// omLessonsFailOpenWeek is the week of one class past the proposal floor: six
// fail-open findings on six beads, one of them on a third attempt, so the class
// burned two rounds. Every other class is too few to say.
func omLessonsFailOpenWeek(at time.Time) []land.ReviewRecord {
	recent := at.AddDate(0, 0, -3)
	var records []land.ReviewRecord
	for _, bead := range []string{"gt-a", "gt-b", "gt-c", "gt-d", "gt-e", "gt-f"} {
		attempt := 1
		if bead == "gt-b" {
			attempt = 3
		}
		records = append(records, omLessonsReviewRecord(bead, recent, attempt, omLessonsFinding("major", "fail-open: a failed read reads as clean")))
	}
	return records
}

// omLessonsRefusingWriter is a beads client that refuses every create.
type omLessonsRefusingWriter struct{ err error }

func (w omLessonsRefusingWriter) Create(beads.CreateOptions) (*beads.Issue, error) {
	return nil, w.err
}

// omLessonsSelectiveWriter is a beads client that refuses the create of one
// class's proposal and passes every other create to inner.
type omLessonsSelectiveWriter struct {
	inner  notifyBeadWriter
	refuse string
	err    error
}

func (w omLessonsSelectiveWriter) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	if strings.Contains(opts.Title, "("+w.refuse+")") {
		return nil, w.err
	}
	return w.inner.Create(opts)
}

// omLessonsWatchingWriter is a beads client that notes, at the start of each
// create, the classes the town's filed record names, so a test can see which
// filings the pass had recorded by the time it filed the next bead.
type omLessonsWatchingWriter struct {
	inner notifyBeadWriter
	town  string
	week  string
	seen  [][]string
}

func (w *omLessonsWatchingWriter) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	filed, err := loadOmLessonsFiled(w.town, w.week)
	if err != nil {
		return nil, fmt.Errorf("reading the filed record before a create: %w", err)
	}
	ids := make([]string, 0, len(filed))
	for id := range filed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	w.seen = append(w.seen, ids)
	return w.inner.Create(opts)
}

// omLessonsTwoDraftWeek is a week with two classes past the proposal floor:
// fail-open first in the table's order, then missing-refusal-test.
func omLessonsTwoDraftWeek(at time.Time) []land.ReviewRecord {
	records := omLessonsFailOpenWeek(at)
	recent := at.AddDate(0, 0, -3)
	for _, bead := range []string{"gt-r1", "gt-r2", "gt-r3", "gt-r4", "gt-r5"} {
		records = append(records, omLessonsReviewRecord(bead, recent, 1, omLessonsFinding("minor", "the refusal branch has no test")))
	}
	return records
}

// TestReworkProgramOmLessonsRefusedFilingIsReturned: a create the beads client
// refuses is returned wrapped, naming the class it was filing, so the pass
// never reads as filed.
func TestReworkProgramOmLessonsRefusedFilingIsReturned(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	refused := errors.New("the beads client refused the create")

	d := omLessonsTestDaemon(t, io.Discard)
	d.openNoticeBeads = func() notifyBeadWriter { return omLessonsRefusingWriter{err: refused} }
	d.omLessonsRun = omLessonsPass(omLessonsFakeSources(omLessonsFailOpenWeek(at), map[string]bool{"1": true}))

	_, err := d.runOmLessonsTick(context.Background(), at)
	if !errors.Is(err, refused) {
		t.Fatalf("a refused create returned %v, want the client's error", err)
	}
	if !strings.Contains(err.Error(), "fail-open") {
		t.Errorf("the refused create's error %q does not name the class it was filing", err)
	}
}

// TestReworkProgramOmLessonsFilingRefusesAnEmptyWindow: a window of no days
// names no counts, so filing refuses it and writes no bead.
func TestReworkProgramOmLessonsFilingRefusesAnEmptyWindow(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

	proposals, err := omLessonsRunFrom(context.Background(), at, omLessonsFakeSources(omLessonsFailOpenWeek(at), map[string]bool{"1": true}))
	if err != nil {
		t.Fatalf("the pass over a fake record source: %v", err)
	}
	bd := beadsfake.New()
	d := omLessonsTestDaemon(t, io.Discard)
	d.openNoticeBeads = func() notifyBeadWriter { return bd }

	if err := d.fileOmLessonsProposals(at, 0, proposals); err == nil {
		t.Fatal("a window of no days filed proposals")
	}
	filed, err := bd.List(beads.ListOptions{Status: "all"})
	if err != nil {
		t.Fatalf("listing the beads the refused filing left: %v", err)
	}
	if len(filed) != 0 {
		t.Errorf("a refused window left %d bead(s), want none", len(filed))
	}
}

// TestReworkProgramOmLessonsFilingNeedsATown: a daemon that names no town has no
// directive directory to name in a proposal bead and no directory for the filed
// record, so a pass with drafts to file refuses to file them rather than
// writing a bead with no directory in it.
func TestReworkProgramOmLessonsFilingNeedsATown(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	sources := omLessonsFakeSources(omLessonsFailOpenWeek(at), map[string]bool{"1": true})

	t.Run("no config", func(t *testing.T) {
		t.Parallel()
		d := &Daemon{logger: log.New(io.Discard, "", 0)}
		d.omLessonsRun = omLessonsPass(sources)

		if _, err := d.runOmLessonsTick(context.Background(), at); err == nil {
			t.Fatal("a pass with drafts to file filed them in a daemon that names no town")
		}
	})

	t.Run("a blank town root", func(t *testing.T) {
		t.Parallel()
		bd := beadsfake.New()
		d := omLessonsTestDaemon(t, io.Discard)
		d.config.TownRoot = "   "
		d.openNoticeBeads = func() notifyBeadWriter { return bd }
		d.omLessonsRun = omLessonsPass(sources)

		if _, err := d.runOmLessonsTick(context.Background(), at); err == nil {
			t.Fatal("a pass with drafts to file filed them in a daemon that names no town root")
		}
		filed, err := bd.List(beads.ListOptions{Status: "all"})
		if err != nil {
			t.Fatalf("listing the beads the refused filing left: %v", err)
		}
		if len(filed) != 0 {
			t.Errorf("a daemon with no town root filed %d bead(s), want none", len(filed))
		}
	})
}

// TestReworkProgramOmLessonsFilingDedupesByClassAndISOWeek: a class filed in an
// ISO week is not filed again by a later pass in the same week, even from a
// freshly built patrol that can only know the first pass from the record it
// persisted; the next ISO week files it again.
func TestReworkProgramOmLessonsFilingDedupesByClassAndISOWeek(t *testing.T) {
	t.Parallel()
	monday := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)     // 2026-W47
	thursday := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)   // 2026-W47
	nextMonday := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC) // 2026-W48
	town := t.TempDir()
	bd := beadsfake.New()

	// filing returns a daemon over the same town and beads client with a week of
	// findings at at, so each pass reads only what the previous one persisted.
	filing := func(t *testing.T, at time.Time) *Daemon {
		t.Helper()
		d := omLessonsTestDaemon(t, io.Discard)
		d.config.TownRoot = town
		d.openNoticeBeads = func() notifyBeadWriter { return bd }
		d.omLessonsRun = omLessonsPass(omLessonsFakeSources(omLessonsFailOpenWeek(at), map[string]bool{"1": true}))
		return d
	}

	if _, err := filing(t, monday).runOmLessonsTick(context.Background(), monday); err != nil {
		t.Fatalf("the week's first pass: %v", err)
	}
	if _, err := filing(t, thursday).runOmLessonsTick(context.Background(), thursday); err != nil {
		t.Fatalf("the second pass in the same ISO week: %v", err)
	}
	filed, err := bd.List(beads.ListOptions{Status: "all"})
	if err != nil {
		t.Fatalf("listing the beads the passes filed: %v", err)
	}
	if len(filed) != 1 {
		t.Fatalf("two passes in ISO week 2026-W47 filed %d bead(s), want one", len(filed))
	}

	if _, err := filing(t, nextMonday).runOmLessonsTick(context.Background(), nextMonday); err != nil {
		t.Fatalf("the next ISO week's pass: %v", err)
	}
	filed, err = bd.List(beads.ListOptions{Status: "all"})
	if err != nil {
		t.Fatalf("listing the beads the passes filed: %v", err)
	}
	if len(filed) != 2 {
		t.Errorf("a pass in ISO week 2026-W48 left %d bead(s), want the week's one filed again", len(filed))
	}
}

// TestReworkProgramOmLessonsOneFailedFilingDoesNotStopTheOthers: a create the
// beads client refuses is returned, and the proposals after it are still filed.
func TestReworkProgramOmLessonsOneFailedFilingDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	refused := errors.New("the beads client refused the create")
	bd := beadsfake.New()

	d := omLessonsTestDaemon(t, io.Discard)
	d.openNoticeBeads = func() notifyBeadWriter {
		return omLessonsSelectiveWriter{inner: bd, refuse: "fail-open", err: refused}
	}
	d.omLessonsRun = omLessonsPass(omLessonsFakeSources(omLessonsTwoDraftWeek(at), map[string]bool{"1": true, "3": true}))

	_, err := d.runOmLessonsTick(context.Background(), at)
	if !errors.Is(err, refused) {
		t.Fatalf("a refused create returned %v, want the client's error", err)
	}

	filed, err := bd.List(beads.ListOptions{Status: "all"})
	if err != nil {
		t.Fatalf("listing the beads the pass filed: %v", err)
	}
	if len(filed) != 1 {
		t.Fatalf("a pass with one refused create filed %d bead(s), want the other class's one", len(filed))
	}
	if want := "Class: a refusal branch has no test (missing-refusal-test)"; !strings.Contains(filed[0].Description, want) {
		t.Errorf("the pass filed a bead other than missing-refusal-test; text:\n%s", filed[0].Description)
	}
}

// TestReworkProgramOmLessonsAFiledClassIsRecordedBeforeTheNextCreate: the filed
// record is written as each class's bead is created, not once after the loop, so
// a pass that ends before its last class leaves the classes it already filed
// recorded and the next pass of the week files nothing twice.
func TestReworkProgramOmLessonsAFiledClassIsRecordedBeforeTheNextCreate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC) // 2026-W47
	town := t.TempDir()
	week := omLessonsISOWeek(at)
	w := &omLessonsWatchingWriter{inner: beadsfake.New(), town: town, week: week}

	d := omLessonsTestDaemon(t, io.Discard)
	d.config.TownRoot = town
	d.openNoticeBeads = func() notifyBeadWriter { return w }
	d.omLessonsRun = omLessonsPass(omLessonsFakeSources(omLessonsTwoDraftWeek(at), map[string]bool{"1": true, "3": true}))

	if _, err := d.runOmLessonsTick(context.Background(), at); err != nil {
		t.Fatalf("the pass over two draft classes: %v", err)
	}
	if len(w.seen) != 2 {
		t.Fatalf("the pass made %d create(s), want the two draft classes", len(w.seen))
	}
	if len(w.seen[0]) != 0 {
		t.Errorf("the first create saw %v already recorded, want nothing filed before it", w.seen[0])
	}
	if want := []string{"fail-open"}; !slices.Equal(w.seen[1], want) {
		t.Errorf("the second create saw %v recorded, want the class created before it (%v) recorded by then", w.seen[1], want)
	}
}

// TestReworkProgramOmLessonsOneFailedFilingIsLoggedOnce: a create the beads
// client refuses reaches the log once, through the pass's returned error, not
// also straight from the filing loop.
func TestReworkProgramOmLessonsOneFailedFilingIsLoggedOnce(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	refused := errors.New("the beads client refused the create")
	var logged bytes.Buffer

	d := omLessonsTestDaemon(t, &logged)
	d.clock = clockwork.NewFakeClockAt(at)
	d.config.TownRoot = t.TempDir()
	d.openNoticeBeads = func() notifyBeadWriter {
		return omLessonsSelectiveWriter{inner: beadsfake.New(), refuse: "fail-open", err: refused}
	}
	d.omLessonsRun = omLessonsPass(omLessonsFakeSources(omLessonsTwoDraftWeek(at), map[string]bool{"1": true, "3": true}))

	d.triggerOmLessons()

	if got := strings.Count(logged.String(), "filing the proposal for fail-open"); got != 1 {
		t.Errorf("one refused create was logged %d time(s), want once; log:\n%s", got, logged.String())
	}
}

// TestReworkProgramOmLessonsAFiledRecordThatCannotBeWrittenIsReturned: a bead
// the pass filed but could not record as filed is returned as an error after
// the create, so a record the next pass cannot trust never reads as a clean
// filing.
func TestReworkProgramOmLessonsAFiledRecordThatCannotBeWrittenIsReturned(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	town := t.TempDir()
	// A dangling symlink stands where the record's directory belongs: the read
	// finds nothing, and the save cannot create the directory.
	if err := os.Symlink(filepath.Join(town, "missing"), filepath.Join(town, "daemon")); err != nil {
		t.Fatalf("make the record's directory a dangling symlink: %v", err)
	}
	bd := beadsfake.New()
	d := omLessonsTestDaemon(t, io.Discard)
	d.config.TownRoot = town
	d.openNoticeBeads = func() notifyBeadWriter { return bd }
	d.omLessonsRun = omLessonsPass(omLessonsFakeSources(omLessonsFailOpenWeek(at), map[string]bool{"1": true}))

	_, err := d.runOmLessonsTick(context.Background(), at)
	if err == nil {
		t.Fatal("a pass that could not record its filed class returned no error")
	}
	if !strings.Contains(err.Error(), "recording the classes filed") {
		t.Errorf("the failed record write returned %q, want the recording named", err)
	}
	filed, listErr := bd.List(beads.ListOptions{Status: "all"})
	if listErr != nil {
		t.Fatalf("listing the beads the pass filed: %v", listErr)
	}
	if len(filed) != 1 {
		t.Errorf("a pass that could not record its class filed %d bead(s), want the create still made", len(filed))
	}
}

// TestReworkProgramOmLessonsFilingRefusesAnUnreadableFiledRecord: a filed-class
// record that cannot be read or parsed could have filed every class, so a pass
// over it files nothing rather than filing everything a second time.
func TestReworkProgramOmLessonsFilingRefusesAnUnreadableFiledRecord(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		prepare func(t *testing.T, townRoot string)
	}{
		{
			name: "a record that is not JSON",
			prepare: func(t *testing.T, townRoot string) {
				t.Helper()
				path := omLessonsFiledPath(townRoot)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("make the daemon directory: %v", err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatalf("write a corrupt filed record: %v", err)
				}
			},
		},
		{
			name: "a record that is a directory",
			prepare: func(t *testing.T, townRoot string) {
				t.Helper()
				if err := os.MkdirAll(omLessonsFiledPath(townRoot), 0o755); err != nil {
					t.Fatalf("make the record path a directory: %v", err)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			c.prepare(t, town)
			bd := beadsfake.New()
			d := omLessonsTestDaemon(t, io.Discard)
			d.config.TownRoot = town
			d.openNoticeBeads = func() notifyBeadWriter { return bd }
			d.omLessonsRun = omLessonsPass(omLessonsFakeSources(omLessonsFailOpenWeek(at), map[string]bool{"1": true}))

			_, err := d.runOmLessonsTick(context.Background(), at)
			if err == nil {
				t.Fatal("a pass over an unreadable filed record filed its proposals")
			}
			if !strings.Contains(err.Error(), omLessonsFiledFileName) {
				t.Errorf("the refusal %q does not name the record it could not read", err)
			}
			filed, listErr := bd.List(beads.ListOptions{Status: "all"})
			if listErr != nil {
				t.Fatalf("listing the beads the refused filing left: %v", listErr)
			}
			if len(filed) != 0 {
				t.Errorf("a refused pass filed %d bead(s), want none", len(filed))
			}
		})
	}
}

// TestReworkProgramOmLessonsOneProposalFilesOneOverseerBead: a pass over a week
// with one class past the proposal floor files one bead through the beads
// client. The bead wears overseer-only alone, and its text carries the class,
// the counts, the first three example beads and the directive edit. The classes
// with too few findings to say file nothing.
func TestReworkProgramOmLessonsOneProposalFilesOneOverseerBead(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

	bd := beadsfake.New()
	d := omLessonsTestDaemon(t, io.Discard)
	d.openNoticeBeads = func() notifyBeadWriter { return bd }
	d.omLessonsRun = omLessonsPass(omLessonsFakeSources(omLessonsFailOpenWeek(at), map[string]bool{"1": true}))

	if _, err := d.runOmLessonsTick(context.Background(), at); err != nil {
		t.Fatalf("the pass over a fake record source: %v", err)
	}
	filed, err := bd.List(beads.ListOptions{Status: "all"})
	if err != nil {
		t.Fatalf("listing the beads the pass filed: %v", err)
	}
	if len(filed) != 1 {
		t.Fatalf("the pass filed %d bead(s), want one: fail-open is the only class with enough findings", len(filed))
	}
	issue := filed[0]
	if want := []string{dispatch.OverseerOnlyLabel}; !slices.Equal(issue.Labels, want) {
		t.Errorf("the proposal bead wears labels %v, want only %v so no seat slings it", issue.Labels, want)
	}
	for _, want := range []string{
		"Class: an error reads as success (fail-open)",
		"Coverage: violated-anyway: rule #1 covers it and it still appears",
		"Counts: 6 finding(s) on 6 bead(s), 2 round(s) burned; prior window 0 finding(s) on 0 bead(s), 0 round(s)",
		"Examples: gt-a, gt-b, gt-c\n",
		"Goal: a directive edit that stops om raising this class.",
	} {
		if !strings.Contains(issue.Description, want) {
			t.Errorf("the proposal bead's text lacks %q; text:\n%s", want, issue.Description)
		}
	}
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
