package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/scheduler/capacity"
	"github.com/steveyegge/gastown/internal/schedulerrun"
)

// The daemon's automatic dispatchers — the stranded-convoy feeder and the
// scheduled_slings patrol — stand down while the operator's town-wide hold
// file exists (gt-ifijm). Each test uses a temp town root; the real
// ~/gt/seat-refill.hold is never read.

func writeOperatorHold(t *testing.T, townRoot string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(townRoot, "seat-refill.hold"), nil, 0644); err != nil {
		t.Fatalf("write hold: %v", err)
	}
}

func TestFeedFirstReady_OperatorHold_SlingsNothing(t *testing.T) {
	t.Parallel()
	// A gt that would succeed: only the hold can stop the sling.
	townRoot, gt, _ := backpressureFeedRig(t)
	writeOperatorHold(t, townRoot)

	var logged []string
	logger := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }
	m := newFeedManager(townRoot, logger, gt)

	m.feedFirstReady(strandedConvoyInfo{ID: "hq-cv-held", ReadyCount: 1, ReadyIssues: []string{"gt-issue1"}})

	if calls := gt.argvs(); len(calls) != 0 {
		t.Errorf("gt invoked during an operator hold: %q", calls)
	}
	found := false
	for _, l := range logged {
		if strings.Contains(l, "seat-refill.hold") {
			found = true
		}
	}
	if !found {
		t.Errorf("no log line naming the hold; got %v", logged)
	}
}

func TestRunScheduledSlings_OperatorHold_DispatchesNothingAndCountsNoFailure(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	writeOperatorHold(t, d.config.TownRoot)

	for i := 0; i < 3; i++ {
		d.runScheduledSlings()
	}

	if len(f.created) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("scheduled sling ran during an operator hold: create=%v sling=%v", f.created, f.slungIDs)
	}
	if n := d.scheduledSlingFailures["doc-audit"]; n != 0 {
		t.Errorf("a hold counted as %d failure(s); it is not a failure", n)
	}
	if len(*esc) != 0 {
		t.Errorf("a hold escalated: %v", *esc)
	}

	// Lifting the hold resumes dispatch on the next tick.
	if err := os.Remove(filepath.Join(d.config.TownRoot, "seat-refill.hold")); err != nil {
		t.Fatal(err)
	}
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.slungIDs) != 1 {
		t.Errorf("after the hold lifted: sling calls = %v, want one", f.slungIDs)
	}
}

// countContaining returns how many lines contain every fragment.
func countContaining(lines []string, fragments ...string) int {
	n := 0
	for _, l := range lines {
		ok := true
		for _, f := range fragments {
			if !strings.Contains(l, f) {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}

// lineLogger is a *log.Logger whose output is split into lines on read.
func lineLogger() (*log.Logger, func() []string) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	return log.New(w, "", 0), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Split(strings.TrimSpace(buf.String()), "\n")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestDispatchQueuedWork_OperatorHold_SkipsDispatchAndLogsOnce covers
// heartbeat step 14: the scheduled-dispatch pass slings queued beads
// (executeSling) and must stand down under the hold, logging once per
// hold-state change. It runs in process since gt-638go.9, so the tick forks
// nothing: the assertion that no gt subprocess appears is the point.
func TestDispatchQueuedWork_OperatorHold_SkipsDispatchAndLogsOnce(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeOperatorHold(t, townRoot)
	logger, lines := lineLogger()
	gt := newFakeCLI(nil)
	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        logger,
		execCmd:       gt.run,
		schedulerDeps: schedulerDepsForTest(),
	}

	for i := 0; i < 3; i++ {
		d.dispatchQueuedWork()
	}
	if calls := gt.argvs(); len(calls) != 0 {
		t.Fatalf("the daemon forked gt during an operator hold: %q", calls)
	}
	if n := countContaining(lines(), "scheduler dispatch", "seat-refill.hold"); n != 1 {
		t.Errorf("hold logged %d times over 3 ticks, want once; log: %v", n, lines())
	}

	if err := os.Remove(filepath.Join(townRoot, "seat-refill.hold")); err != nil {
		t.Fatal(err)
	}
	// A malformed scheduler state file stops the pass inside planning, before
	// it reads any store: this is the unit tier, where reaching bd at all is a
	// tripwire failure. What the tick proves here is that it runs the pass in
	// this process — the log line is the run, and the empty gt argv is the
	// absence of the subprocess this change removed.
	if err := os.MkdirAll(filepath.Join(townRoot, ".runtime"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".runtime", "scheduler-state.json"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	d.dispatchQueuedWork()
	if calls := gt.argvs(); len(calls) != 0 {
		t.Errorf("scheduled dispatch still shells out to gt: %q", calls)
	}
	if n := countContaining(lines(), "Scheduler dispatch failed"); n != 1 {
		t.Errorf("after the hold lifted, the tick did not run a dispatch pass; log: %v", lines())
	}
	if n := countContaining(lines(), "scheduler dispatch", "hold lifted"); n != 1 {
		t.Errorf("hold lift logged %d times, want once; log: %v", n, lines())
	}
}

// The hold-lifted line names the dispatchers that came back and whether each
// is on, so the operator reading daemon.log learns what resumed without a
// second lookup (gt-xiw7o).
func TestDispatchQueuedWork_HoldLiftedLineNamesTheDispatchers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		spec    bool
		feeding bool
		want    string
	}{
		{"both on", true, true, "(spec_dispatch on, convoy feed on)"},
		{"spec_dispatch on, feeder stopped", true, false, "(spec_dispatch on, convoy feed off)"},
		{"both off", false, false, "(spec_dispatch off, convoy feed off)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			writeOperatorHold(t, townRoot)
			logger, lines := lineLogger()
			feed := NewConvoyManager(townRoot, logger.Printf, nil, 0, nil, nil, nil)
			// A running feeder without its goroutines: FeedActive reads the
			// state Start sets, not the work the goroutines do.
			feed.started.Store(tc.feeding)
			d := &Daemon{
				config: &Config{TownRoot: townRoot},
				logger: logger,
				patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
					SpecDispatch: &SpecDispatchConfig{Enabled: tc.spec},
				}},
				schedulerDeps: schedulerDepsForTest(),
				convoyManager: feed,
			}

			d.dispatchQueuedWork() // logs the hold once
			if err := os.Remove(filepath.Join(townRoot, "seat-refill.hold")); err != nil {
				t.Fatal(err)
			}
			// A malformed scheduler state file stops the pass inside planning,
			// before it reads any store: this is the unit tier.
			if err := os.MkdirAll(filepath.Join(townRoot, ".runtime"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(townRoot, ".runtime", "scheduler-state.json"), []byte("{not json"), 0644); err != nil {
				t.Fatal(err)
			}
			d.dispatchQueuedWork()

			want := "Resuming scheduler dispatch: operator dispatch hold lifted " + tc.want
			if n := countContaining(lines(), want); n != 1 {
				t.Errorf("hold lift logged %q %d times, want once; log: %v", want, n, lines())
			}
		})
	}
}

// schedulerDepsForTest is a scheduler deps set that dispatches nothing: the
// tests that drive the heartbeat tick care about the gate, not the sling.
func schedulerDepsForTest() schedulerrun.Deps {
	return schedulerrun.Deps{
		Sling: func(context.Context, string, capacity.PendingBead) (schedulerrun.SlingOutcome, error) {
			return schedulerrun.SlingOutcome{}, nil
		},
		Seats: func(string, bool) (schedulerrun.Seats, error) { return schedulerrun.Seats{}, nil },
	}
}

func TestRunScheduledSlings_OperatorHold_LogsOncePerStateChange(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	logger, lines := lineLogger()
	d.logger = logger
	writeOperatorHold(t, d.config.TownRoot)

	for i := 0; i < 4; i++ {
		d.runScheduledSlings()
	}
	if n := countContaining(lines(), "scheduled_slings", "seat-refill.hold"); n != 1 {
		t.Errorf("hold logged %d times over 4 ticks, want once; log: %v", n, lines())
	}
}

// A rig ESTOP holds that rig's scheduled entries and nothing else.
func TestRunScheduledSlings_RigEstop_SkipsOnlyThatRig(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	omEntry := ScheduledSlingEntry{Name: "om-audit", Rig: "om", Formula: "mol-doc-audit", IntervalStr: "168h"}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry, omEntry}, f)
	if err := os.WriteFile(filepath.Join(d.config.TownRoot, "ESTOP.gastown"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}

	d.runScheduledSlings()

	if len(f.slung) != 1 || f.slung[0].Rig != "om" {
		t.Errorf("slung %v, want only the om entry", f.slung)
	}
	if n := d.scheduledSlingFailures["doc-audit"]; n != 0 || len(*esc) != 0 {
		t.Errorf("a rig ESTOP counted as a failure (%d) or escalated (%v)", n, *esc)
	}
}

func TestFeedFirstReady_OperatorHold_LogsOncePerStateChange(t *testing.T) {
	t.Parallel()
	townRoot, gt, _ := backpressureFeedRig(t)
	writeOperatorHold(t, townRoot)
	var logged []string
	logger := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }
	m := newFeedManager(townRoot, logger, gt)

	for i := 0; i < 3; i++ {
		m.feedFirstReady(strandedConvoyInfo{ID: "hq-cv-held", ReadyCount: 1, ReadyIssues: []string{"gt-issue1"}})
	}
	if n := countContaining(logged, "seat-refill.hold"); n != 1 {
		t.Errorf("hold logged %d times over 3 scans, want once; log: %v", n, logged)
	}
}

func TestFeedFirstReady_RigEstop_SkipsThatRigsIssue(t *testing.T) {
	t.Parallel()
	townRoot, gt, _ := backpressureFeedRig(t)
	// routes map gt- to rig "gt".
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gt"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logger := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }
	m := newFeedManager(townRoot, logger, gt)

	m.feedFirstReady(strandedConvoyInfo{ID: "hq-cv-rig", ReadyCount: 1, ReadyIssues: []string{"gt-issue1"}})

	if calls := gt.argvs(); len(calls) != 0 {
		t.Errorf("slung into a rig under ESTOP: %q", calls)
	}
	if countContaining(logged, "gt-issue1", "ESTOP.gt") == 0 {
		t.Errorf("no log line naming the rig ESTOP; got %v", logged)
	}
}
