package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/steward"
	"github.com/steveyegge/gastown/internal/townhealth"
)

type escalation struct{ severity, key, source, message string }

// escalations records what the monitor raises, failing the first failFirst
// calls so the retry path is testable.
type escalations struct {
	mu        sync.Mutex
	got       []escalation
	failFirst int
}

func (e *escalations) raise(severity, key, source, message string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failFirst > 0 {
		e.failFirst--
		return errors.New("town store unreachable")
	}
	e.got = append(e.got, escalation{severity, key, source, message})
	return nil
}

func (e *escalations) keys() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, x := range e.got {
		out = append(out, x.key)
	}
	return out
}

var monitorNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func monitorDaemon(t *testing.T, rows ...steward.Job) (*Daemon, *escalations) {
	t.Helper()
	townRoot := t.TempDir()
	ledger := steward.NewLedger(steward.LedgerPath(townRoot))
	for _, j := range rows {
		if err := ledger.Append(j); err != nil {
			t.Fatal(err)
		}
	}
	esc := &escalations{}
	return &Daemon{
		config:          &Config{TownRoot: townRoot},
		logger:          discardLogger,
		clock:           clockwork.NewFakeClockAt(monitorNow),
		patrolConfig:    stewardPatrolConfig(&StewardConfig{Enabled: true, JobTimeoutStr: "45m"}),
		stewardEscalate: esc.raise,
	}, esc
}

func monitorJob(id, model string, outcome steward.Outcome, startedAgo, ranFor time.Duration) steward.Job {
	j := steward.Job{ID: id, Event: steward.KindReview, Bead: "gt-" + id, Rig: "gastown", Branch: "polecat/a/gt-" + id, Head: "c0ffee00aa", Model: model, Started: monitorNow.Add(-startedAgo)}
	if outcome != "" {
		j.Ended, j.Outcome, j.Summary = j.Started.Add(ranFor), outcome, "summary of "+id
	}
	return j
}

// TestMonitorStewardNoticesEveryProJobOnce: a job on the hard preset raises
// one low-severity escalation besides its ledger row, however many scans see
// it (gt-9bioi.3).
func TestMonitorStewardNoticesEveryProJobOnce(t *testing.T) {
	t.Parallel()
	d, esc := monitorDaemon(t,
		monitorJob("flash", steward.DefaultRoutineAgent, steward.OutcomePass, 10*time.Minute, time.Minute),
		monitorJob("pro1", steward.DefaultHardAgent, steward.OutcomePass, 20*time.Minute, time.Minute),
		monitorJob("pro2", steward.DefaultHardAgent, "", 5*time.Minute, 0),
		// Too old to notice: a daemon back from a long outage does not replay the ledger.
		monitorJob("ancient", steward.DefaultHardAgent, steward.OutcomePass, 30*time.Hour, time.Minute),
	)
	d.monitorSteward()
	d.monitorSteward()
	d.monitorSteward()
	if got := esc.keys(); len(got) != 2 || got[0] != "steward:pro:pro1" || got[1] != "steward:pro:pro2" {
		t.Fatalf("escalated %v, want one notice each for pro1 and pro2", got)
	}
	for _, e := range esc.got {
		if e.severity != "low" || e.source != "steward" {
			t.Errorf("%s: severity %q source %q, want low from steward", e.key, e.severity, e.source)
		}
	}
	if m := esc.got[0].message; !strings.Contains(m, "gt-pro1") || !strings.Contains(m, steward.DefaultHardAgent) || !strings.Contains(m, "c0ffee00") {
		t.Errorf("message = %q, want the bead, the preset and the head", m)
	}
}

func TestMonitorStewardProNoticeSaysWhyItIsHard(t *testing.T) {
	t.Parallel()
	first := monitorJob("routine", steward.DefaultRoutineAgent, steward.OutcomeError, 30*time.Minute, time.Minute)
	retry := monitorJob("retry", steward.DefaultHardAgent, "", 5*time.Minute, 0)
	retry.Bead, retry.Head = first.Bead, first.Head
	d, esc := monitorDaemon(t, first, retry)
	d.monitorSteward()
	if got := esc.keys(); len(got) != 1 || !strings.Contains(esc.got[0].message, "retries job routine, which ended error") {
		t.Fatalf("escalated %v %+v, want one notice naming the failed routine job", got, esc.got)
	}
}

// A failed escalation is not recorded, so the next scan retries it instead
// of losing the notice.
func TestMonitorStewardRetriesAnEscalationThatFailed(t *testing.T) {
	t.Parallel()
	d, esc := monitorDaemon(t, monitorJob("pro1", steward.DefaultHardAgent, steward.OutcomePass, 20*time.Minute, time.Minute))
	esc.failFirst = 1
	d.monitorSteward()
	if len(esc.got) != 0 {
		t.Fatalf("escalated %v while the store was down", esc.keys())
	}
	d.monitorSteward()
	d.monitorSteward()
	if got := esc.keys(); len(got) != 1 {
		t.Fatalf("escalated %v, want exactly one after the store came back", got)
	}
}

func TestMonitorStewardEscalatesAStuckJobOnce(t *testing.T) {
	t.Parallel()
	d, esc := monitorDaemon(t,
		monitorJob("fine", steward.DefaultRoutineAgent, "", 20*time.Minute, 0),
		monitorJob("stuck", steward.DefaultRoutineAgent, "", 50*time.Minute, 0),
	)
	d.monitorSteward()
	d.monitorSteward()
	if got := esc.keys(); len(got) != 1 || got[0] != "steward:stuck:stuck" {
		t.Fatalf("escalated %v, want one stuck alert for the job past its 45m timeout", got)
	}
	if e := esc.got[0]; e.severity != "medium" || !strings.Contains(e.message, "gt-stuck") || !strings.Contains(e.message, "45m0s") {
		t.Errorf("alert = %+v", e)
	}
}

func TestMonitorStewardEscalatesAnErrorRateOnce(t *testing.T) {
	t.Parallel()
	rows := []steward.Job{
		monitorJob("a", steward.DefaultRoutineAgent, steward.OutcomeError, 50*time.Minute, time.Minute),
		monitorJob("b", steward.DefaultRoutineAgent, steward.OutcomeTimeout, 40*time.Minute, time.Minute),
		monitorJob("c", steward.DefaultRoutineAgent, steward.OutcomePass, 30*time.Minute, time.Minute),
		monitorJob("d", steward.DefaultRoutineAgent, steward.OutcomeError, 20*time.Minute, time.Minute),
		// An interrupted job is no attempt, so it neither trips nor dilutes the rate.
		monitorJob("e", steward.DefaultRoutineAgent, steward.OutcomeInterrupted, 10*time.Minute, time.Minute),
	}
	d, esc := monitorDaemon(t, rows...)
	d.monitorSteward()
	d.monitorSteward()
	if got := esc.keys(); len(got) != 1 || got[0] != "steward:error-rate:a" {
		t.Fatalf("escalated %v, want one error-rate alert keyed to the episode's first broken job", got)
	}
	if m := esc.got[0].message; !strings.Contains(m, "3 of 4") || !strings.Contains(m, "summary of d") {
		t.Errorf("message = %q, want the counts and the latest failure", m)
	}
}

func TestMonitorStewardStaysQuietBelowTheThreshold(t *testing.T) {
	t.Parallel()
	d, esc := monitorDaemon(t,
		// Two failures in two attempts is over the rate but under the minimum sample.
		monitorJob("a", steward.DefaultRoutineAgent, steward.OutcomeError, 50*time.Minute, time.Minute),
		monitorJob("b", steward.DefaultRoutineAgent, steward.OutcomeError, 40*time.Minute, time.Minute),
		// A job whose verdict is fail is the steward working, not breaking.
		monitorJob("c", steward.DefaultRoutineAgent, steward.OutcomeFail, 30*time.Minute, time.Minute),
		monitorJob("d", steward.DefaultRoutineAgent, steward.OutcomeEscalated, 20*time.Minute, time.Minute),
	)
	d.monitorSteward()
	if got := esc.keys(); len(got) != 0 {
		t.Fatalf("escalated %v, want nothing", got)
	}
}

func TestMonitorStewardNeedsItsPatrol(t *testing.T) {
	t.Parallel()
	d, esc := monitorDaemon(t, monitorJob("pro1", steward.DefaultHardAgent, "", 5*time.Minute, 0))
	d.patrolConfig = stewardPatrolConfig(&StewardConfig{})
	d.monitorSteward()
	if got := esc.keys(); len(got) != 0 {
		t.Fatalf("a monitor with the patrol off escalated %v", got)
	}
}

func TestStewardHealthSourceCountsTheHour(t *testing.T) {
	t.Parallel()
	d, _ := monitorDaemon(t,
		monitorJob("a", steward.DefaultRoutineAgent, steward.OutcomePass, 30*time.Minute, time.Minute),
		monitorJob("b", steward.DefaultHardAgent, steward.OutcomeEscalated, 20*time.Minute, time.Minute),
		monitorJob("run", steward.DefaultRoutineAgent, "", 50*time.Minute, 0),
		monitorJob("old", steward.DefaultRoutineAgent, steward.OutcomeError, 3*time.Hour, time.Minute),
	)
	src := &healthSources{d: d, now: monitorNow}
	snap, err := src.Steward(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := townhealth.StewardCounters{Window: "1h", Running: 1, Stuck: 1, Jobs: 3, Finished: 2, Attempted: 2, Escalated: 1, Pro: 1}
	if !snap.Enabled || snap.Counters != want {
		t.Fatalf("snapshot = %+v, want %+v", snap, want)
	}
	d.patrolConfig = stewardPatrolConfig(&StewardConfig{})
	if snap, _ := src.Steward(context.Background()); snap.Enabled {
		t.Errorf("a town with the patrol off reported %+v", snap)
	}
}

func TestWriteTownHealthPublishesTheStewardCounters(t *testing.T) {
	t.Parallel()
	d, _ := healthTown(t, monitorNow)
	d.patrolConfig.Patrols.Steward = &StewardConfig{Enabled: true, JobTimeoutStr: "45m"}
	if err := steward.NewLedger(steward.LedgerPath(d.config.TownRoot)).Append(monitorJob("stuck", steward.DefaultRoutineAgent, "", 50*time.Minute, 0)); err != nil {
		t.Fatal(err)
	}
	d.writeTownHealth()
	r, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if r.Steward == nil || r.Steward.Stuck != 1 || r.Steward.Running != 1 {
		t.Fatalf("report steward = %+v, want the stuck job counted", r.Steward)
	}
	if f := healthField(t, r, "steward"); f.Verdict != townhealth.Red || f.Value != "1 stuck" {
		t.Errorf("steward field = %+v, want red 1 stuck", f)
	}
}
