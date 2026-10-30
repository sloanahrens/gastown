package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/steveyegge/gastown/internal/beads"
	"log"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// Background slings (gt-5h70s): a tick plans and returns, its slings run on in
// the daemon, and the next tick does not wait for them.

// slingJob is the tick report's job object for one planned sling.
func slingJob(bead, agent string) string {
	return `{"bead":"` + bead + `","rig":"gastown","agent":"` + agent + `","paths":["internal/x.go"],"run":{"Formula":"","Vars":null,"Actor":""}}`
}

// plannedTick is the report of a tick that planned the jobs: each is in the
// tick's dispatched list, as the hand-off writes it, and in the pending jobs
// the daemon runs.
func plannedTick(jobs ...string) string {
	var dispatched []string
	for _, job := range jobs {
		var f specSlingFlight
		if err := json.Unmarshal([]byte(job), &f); err != nil {
			continue
		}
		e, _ := json.Marshal(specDispatchTickEntry{Bead: f.Bead, Rig: f.Rig, Line: f.Bead + ": sling started in the background"})
		dispatched = append(dispatched, string(e))
	}
	return `{"candidates": 2, "dispatched": [` + strings.Join(dispatched, ",") + `], "pending": [` + strings.Join(jobs, ",") + `]}`
}

// slingWait bounds every wait of these tests on a sling: a sling that never
// starts or never returns fails the one test that waits, not the gate. Nothing
// is timed against it; the tests move the fake clock themselves.
const slingWait = 30 * time.Second

// slingClock is the daemon's clock in these tests: a fake that a sling moves by
// slingTook while it runs, as a real sling takes time.
const slingTook = 90 * time.Second

func newSlingClock() *clockwork.FakeClock {
	return clockwork.NewFakeClockAt(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
}

// recvSling waits, bounded, for the next sling a blockedSlings announces.
func recvSling(t *testing.T, started <-chan string) string {
	ctx, cancel := context.WithTimeout(t.Context(), slingWait)
	defer cancel()
	t.Helper()
	select {
	case job := <-started:
		return job
	case <-ctx.Done():
		t.Fatalf("no sling started within %s", slingWait)
		return ""
	}
}

// waitSlings waits, bounded, for the daemon's slings to return.
func waitSlings(t *testing.T, d *Daemon) {
	ctx, cancel := context.WithTimeout(t.Context(), slingWait)
	defer cancel()
	t.Helper()
	done := make(chan struct{})
	go func() {
		d.specDispatchCycles.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("slings still in flight after %s: %+v", slingWait, d.specSlings.list())
	}
}

// blockedSlings answers a daemon's gt calls: every tick with the next report in
// ticks, every `gt spec sling` once release is closed, announcing each sling on
// started first. A test thereby holds a sling in flight without a timer. The
// clock moves by slingTook inside each sling.
type blockedSlings struct {
	ticks   []string
	release chan struct{}
	started chan string
	clock   *clockwork.FakeClock
	tick    int
}

func (b *blockedSlings) answer(args []string) cliReply {
	if len(args) >= 2 && args[0] == "spec" && args[1] == "sling" {
		b.started <- args[len(args)-1]
		<-b.release
		b.clock.Advance(slingTook)
		return cliReply{stdout: string(wireOut(`{"dispatched":[{"bead":"gt-a","line":"gt-a: slung to gastown/p on claude-sonnet"}]}`))}
	}
	report := b.ticks[min(b.tick, len(b.ticks)-1)]
	b.tick++
	return cliReply{stdout: string(wireOut(report))}
}

// newBlockedSlings is a blockedSlings and the daemon that runs against it, on
// the fake clock.
func newBlockedSlings(t *testing.T, ticks ...string) (*blockedSlings, *fakeCLI, *Daemon, func() []string) {
	t.Helper()
	b := &blockedSlings{
		ticks:   ticks,
		release: make(chan struct{}),
		started: make(chan string, 4),
		clock:   newSlingClock(),
	}
	fake := newFakeCLI(b.answer)
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = b.clock
	return b, fake, d, lines
}

func TestNextTickStartsWhileAnEarlierSlingIsStillRunning(t *testing.T) {
	t.Parallel()
	b, fake, d, lines := newBlockedSlings(t,
		plannedTick(slingJob("gt-a", "claude-sonnet")), plannedTick(slingJob("gt-b", "deepseek-flash")))

	d.runSpecDispatch()
	if got := recvSling(t, b.started); !strings.Contains(got, `"bead":"gt-a"`) {
		t.Fatalf("first sling job = %s, want gt-a's job as the tick printed it", got)
	}
	// gt-a's sling is blocked. The tick returned without it, and the next tick
	// runs now.
	if running := d.specSlings.list(); len(running) != 1 || running[0].Bead != "gt-a" {
		t.Fatalf("in flight = %+v, want gt-a", running)
	}
	d.runSpecDispatch()
	if got := recvSling(t, b.started); !strings.Contains(got, `"bead":"gt-b"`) {
		t.Fatalf("second sling job = %s, want gt-b's: the next tick must sling the other candidate", got)
	}

	// The second tick was told gt-a is running, with its seat and files.
	ticks := fake.argvs("spec", "dispatch")
	if len(ticks) != 2 {
		t.Fatalf("ticks = %v, want two", ticks)
	}
	if !slices.Contains(ticks[0], "--background-slings") || slices.Contains(ticks[0], "--in-flight") {
		t.Errorf("first tick argv = %v, want background slings and nothing in flight", ticks[0])
	}
	i := slices.Index(ticks[1], "--in-flight")
	if i < 0 || !strings.Contains(ticks[1][i+1], `"bead":"gt-a"`) || !strings.Contains(ticks[1][i+1], `"agent":"claude-sonnet"`) {
		t.Errorf("second tick argv = %v, want --in-flight naming gt-a and its seat", ticks[1])
	}

	close(b.release)
	waitSlings(t, d)
	if running := d.specSlings.list(); len(running) != 0 {
		t.Fatalf("in flight after the slings returned = %+v, want none", running)
	}
	got := lines()
	// The fake answers both slings with the same report.
	if n := countContaining(got, "spec_dispatch: dispatched: gt-a: slung to gastown/p", "sling took"); n != 2 {
		t.Errorf("sling result lines = %d, want one per finished sling: %v", n, got)
	}
}

// The tick is handed the running sling's bead, its seat and its start, and
// nothing else: the job's file and test lists are the sling's, and the tick
// never reads them (gt-mx656).
func TestTheInFlightPayloadCarriesOnlyWhatTheTickReads(t *testing.T) {
	t.Parallel()
	job := `{"bead":"gt-a","rig":"gastown","agent":"claude-sonnet","paths":["internal/x.go"],"tests":["TestX"],"run":{"Formula":"","Vars":null,"Actor":""}}`
	b, fake, d, _ := newBlockedSlings(t, plannedTick(job))

	d.runSpecDispatch()
	recvSling(t, b.started)
	// The second tick runs with gt-a's sling still in flight.
	d.runSpecDispatch()

	ticks := fake.argvs("spec", "dispatch")
	if len(ticks) != 2 {
		t.Fatalf("ticks = %v, want two", ticks)
	}
	i := slices.Index(ticks[1], "--in-flight")
	if i < 0 {
		t.Fatalf("second tick argv = %v, want --in-flight naming the running sling", ticks[1])
	}
	var got []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(ticks[1][i+1]), &got); err != nil {
		t.Fatalf("--in-flight is not JSON: %v (%s)", err, ticks[1][i+1])
	}
	if len(got) != 1 {
		t.Fatalf("--in-flight = %s, want gt-a alone", ticks[1][i+1])
	}
	keys := slices.Sorted(maps.Keys(got[0]))
	if want := []string{"agent", "bead", "rig", "started"}; !slices.Equal(keys, want) {
		t.Errorf("--in-flight keys = %v, want %v: the job carried a path and a test list, and the tick reads neither", keys, want)
	}

	close(b.release)
	waitSlings(t, d)
}

func TestARunningBeadIsNeverSlungTwice(t *testing.T) {
	t.Parallel()
	// Both ticks hand back gt-a: the second tick's board was read before the
	// first sling hooked it.
	b, fake, d, lines := newBlockedSlings(t, plannedTick(slingJob("gt-a", "claude-sonnet")))

	d.runSpecDispatch()
	recvSling(t, b.started)
	d.runSpecDispatch()

	if got := fake.argvs("spec", "sling"); len(got) != 1 {
		t.Fatalf("slings run = %v, want gt-a slung once while one is in flight", got)
	}
	if n := countContaining(lines(), "gt-a: sling not started: one is already running for it"); n != 1 {
		t.Errorf("duplicate lines = %d, want the refused second sling logged once: %v", n, lines())
	}
	close(b.release)
	waitSlings(t, d)

	// The second tick handed over a bead it never dispatched, so its record
	// must count the duplicate skipped (gt-pk2q8).
	recs := d.dispatchTickRecords()
	if len(recs) != 2 {
		t.Fatalf("tick records = %+v, want two", recs)
	}
	if recs[0].Dispatched != 1 {
		t.Errorf("first tick = %+v, want its started sling counted dispatched", recs[0])
	}
	if recs[1].Dispatched != 0 || recs[1].Skipped != 1 {
		t.Errorf("second tick = %+v, want the duplicate counted skipped", recs[1])
	}
}

// The tick is told when each running sling started, so the min_spawn_gap
// stagger holds across ticks before the sling has a session (gt-vames).
func TestTheTickIsToldWhenEachRunningSlingStarted(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 10, 24, 17, 0, 0, 0, time.UTC)
	d := &Daemon{}
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", Rig: "gastown", Agent: "claude-sonnet", started: started})

	args, err := d.specTickArgs()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "--in-flight")
	if i < 0 {
		t.Fatalf("args = %v, want --in-flight naming gt-a", args)
	}
	var got []struct {
		Bead    string    `json:"bead"`
		Started time.Time `json:"started"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &got); err != nil {
		t.Fatalf("--in-flight is not JSON: %v (%s)", err, args[i+1])
	}
	if len(got) != 1 || got[0].Bead != "gt-a" || !got[0].Started.Equal(started) {
		t.Fatalf("--in-flight = %+v, want gt-a started at %v", got, started)
	}
}

// A flight with no start time is still named, so it takes its seat, and it
// leaves the started key out rather than stamping it 0001-01-01T00:00:00Z
// (gt-fzcge).
func TestAFlightWithNoStartLeavesStartedOut(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", Rig: "gastown", Agent: "claude-sonnet"})

	args, err := d.specTickArgs()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "--in-flight")
	if i < 0 {
		t.Fatalf("args = %v, want --in-flight naming gt-a", args)
	}
	if !strings.Contains(args[i+1], `"bead":"gt-a"`) || strings.Contains(args[i+1], `"started"`) {
		t.Errorf("--in-flight = %s, want gt-a with no started key", args[i+1])
	}
}

func TestAFailedBackgroundSlingIsLoggedLikeAForegroundFailure(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func(args []string) cliReply {
		if args[1] == "sling" {
			return cliReply{stdout: string(wireOut(`{"failed":[{"bead":"gt-z","line":"gt-z: dispatch failed, left unassigned: boom; excluded from the queue until label spec-dispatch-failed is cleared"}]}`))}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = newSlingClock()

	d.runSpecDispatch()
	waitSlings(t, d)

	if n := countContaining(lines(), "spec_dispatch: failed: gt-z", "dispatch failed, left unassigned: boom"); n != 1 {
		t.Errorf("failed lines = %d, want one naming the bead and the reason: %v", n, lines())
	}
	if running := d.specSlings.list(); len(running) != 0 {
		t.Errorf("in flight = %+v, want a finished sling out of the set", running)
	}
	// `gt spec sling` labeled the bead itself; the daemon writes nothing more.
	if got := fake.argvs("bead"); len(got) != 0 {
		t.Errorf("daemon bead writes = %v, want none for a sling that reported its failure", got)
	}
}

// A sling whose process is killed at its bound or dies with no report never
// reached its own label and note, so the daemon labels the bead as a failed
// foreground sling would be.
func TestADeadBackgroundSlingProcessLabelsItsBead(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func(args []string) cliReply {
		if args[1] == "sling" {
			return cliReply{stderr: "signal: killed", code: 1}
		}
		if args[0] == "bead" {
			return cliReply{}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = newSlingClock()
	// The bead is unowned, so the failure label applies (gt-nosod).
	bd := newWorkBD(t)
	bd.seed("gt-z", "open", time.Now())
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	// The label goes on, and the retry state a kill at the bound would leave
	// goes off: this failure is not that kill (gt-vaivp).
	want := []string{"bead", "update", "gt-z",
		"--add-label", "spec-dispatch-failed",
		"--remove-label", specdispatch.TimeoutLabel,
		"--unset-metadata", specdispatch.TimeoutAttemptsKey}
	if got := fake.argvs("bead", "update"); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("label calls = %v, want %v", got, want)
	}
	notes := fake.argvs("bead", "note", "gt-z")
	if len(notes) != 1 || !strings.Contains(notes[0][len(notes[0])-1], "sling process failed") || !strings.Contains(notes[0][len(notes[0])-1], "spec-dispatch-failed is cleared") {
		t.Errorf("note calls = %v, want one note saying the sling process failed and how to clear the label", notes)
	}
	if n := countContaining(lines(), "spec_dispatch: gt-z: sling process failed"); n != 1 {
		t.Errorf("failure lines = %d, want one: %v", n, lines())
	}
	if running := d.specSlings.list(); len(running) != 0 {
		t.Errorf("in flight = %+v, want a dead sling out of the set", running)
	}
}

// A sling killed at its bound after it hooked the bead spawned a polecat that
// owns the work now. The failure label would hold the bead out of the queue for
// good, since nothing clears it if that polecat later dies (gt-nosod).
func TestFailSpecSlingSkipsLabelWhenBeadHooked(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func(args []string) cliReply {
		if args[1] == "sling" {
			return cliReply{stderr: "signal: killed", code: 1}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = newSlingClock()
	bd := newWorkBD(t)
	bd.seed("gt-z", "hooked", time.Now())
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("bead"); len(got) != 0 {
		t.Errorf("bead writes = %v, want none for a bead the sling hooked", got)
	}
	if n := countContaining(lines(), "spec_dispatch: gt-z: sling died after hooking it"); n != 1 {
		t.Errorf("hook line = %d, want one saying the bead was left to its polecat: %v", n, lines())
	}
}

// A read that failed says nothing about whether the sling hooked the bead, and
// the label is the one write nothing clears without an operator, so the bead is
// left unlabeled (gt-nosod).
func TestFailSpecSlingSkipsLabelWhenTheBeadCannotBeRead(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func(args []string) cliReply {
		if args[1] == "sling" {
			return cliReply{stderr: "signal: killed", code: 1}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = newSlingClock()
	bd := newWorkBD(t)
	bd.showErr = errors.New("dolt: connection refused")
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("bead"); len(got) != 0 {
		t.Errorf("bead writes = %v, want none when the bead could not be read", got)
	}
	if n := countContaining(lines(), "spec_dispatch: gt-z: the bead could not be read before the failure label"); n != 1 {
		t.Errorf("unreadable line = %d, want one saying the bead was left unlabeled: %v", n, lines())
	}
}

// A sling cut because the daemon is shutting down failed through no fault of
// its bead, which stays ready for the next daemon.
func TestAShutdownCutSlingDoesNotLabelItsBead(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(context.Background())
	fake := newFakeCLI(func(args []string) cliReply {
		if args[1] == "sling" {
			stop() // the daemon shuts down while the sling runs
			return cliReply{stderr: "signal: killed", code: 1}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, _ := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.ctx = ctx
	d.clock = newSlingClock()

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("bead"); len(got) != 0 {
		t.Errorf("bead writes = %v, want none for a sling the shutdown cut", got)
	}
}

// With a failed bead-write the failure is logged and the set still empties.
func TestAFailedFailureWriteIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func(args []string) cliReply {
		switch args[0] {
		case "bead":
			return cliReply{stderr: "dolt down", code: 1}
		case "spec":
			if args[1] == "sling" {
				return cliReply{code: 1}
			}
		}
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = newSlingClock()
	// The bead is unowned, so the failure label is attempted (gt-nosod).
	bd := newWorkBD(t)
	bd.seed("gt-z", "open", time.Now())
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	if n := countContaining(lines(), "spec_dispatch: gt-z: gt update failed", "dolt down"); n != 1 {
		t.Errorf("label failure lines = %d, want one: %v", n, lines())
	}
	if n := countContaining(lines(), "spec_dispatch: gt-z: gt note failed"); n != 1 {
		t.Errorf("note failure lines = %d, want one: %v", n, lines())
	}
}

// backgroundTick is the report of a tick that planned one background sling for
// gt-a: the bead is both in its dispatched list, as the tick hands off, and in
// the pending jobs the daemon runs.
func backgroundTick() string {
	return `{"roster":"claude-sonnet 0/2","candidates":1,
		"dispatched":[{"bead":"gt-a","rig":"gastown","line":"gt-a: sling to gastown/p started in the background"}],
		"pending":[` + slingJob("gt-a", "claude-sonnet") + `]}`
}

// staticDispatch answers townhealth with a record as the daemon holds it.
type staticDispatch struct{ rec townhealth.DispatchRecord }

func (s staticDispatch) Dispatch() (townhealth.DispatchRecord, error) { return s.rec, nil }

// dispatchVerdict is townhealth's dispatch field for rec.
func dispatchVerdict(t *testing.T, now time.Time, rec townhealth.DispatchRecord) townhealth.Field {
	t.Helper()
	rep := townhealth.Compute(context.Background(), townhealth.Inputs{Now: now, Dispatch: staticDispatch{rec: rec}})
	for _, f := range rep.Fields {
		if f.Name == townhealth.FieldDispatch {
			return f
		}
	}
	t.Fatalf("the report carries no %s field: %+v", townhealth.FieldDispatch, rep.Fields)
	return townhealth.Field{}
}

// gt-pk2q8: a background sling that comes back deferred or failed started
// nothing, so the tick that planned it must not keep counting it as a
// dispatch. Every tick then reads as a tick that started nothing: the idle run
// counts toward its alarm and the dispatch field stops reading ok, where the
// hand-off alone left both green.
func TestRunSpecSlingDeferralFeedsHealth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		sling string
	}{
		{"deferred", `{"deferred":[{"bead":"gt-a","line":"gt-a: sling deferred, the sandbox image lags the installed gt: image drift"}]}`},
		{"failed", `{"failed":[{"bead":"gt-a","line":"gt-a: dispatch failed, left unassigned: boom; excluded from the queue until label spec-dispatch-failed is cleared"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newSlingClock()
			fake := newFakeCLI(func(args []string) cliReply {
				if len(args) > 1 && args[1] == "sling" {
					return cliReply{stdout: string(wireOut(tc.sling))}
				}
				return cliReply{stdout: string(wireOut(backgroundTick()))}
			})
			logger, lines := lineLogger()
			d := specDispatchFake(t, fake, logger)
			d.clock = clock

			for i := 0; i < attentionDispatchIdleTicks; i++ {
				d.runSpecDispatch()
				waitSlings(t, d)
				clock.Advance(time.Minute)
			}

			if got := d.dispatchIdle.Status().Streak; got != attentionDispatchIdleTicks {
				t.Errorf("idle streak = %d after %d ticks whose slings never started, want %d: %v",
					got, attentionDispatchIdleTicks, attentionDispatchIdleTicks, lines())
			}
			for i, rec := range d.dispatchTickRecords() {
				if rec.Dispatched != 0 || rec.Failed != 1 {
					t.Errorf("tick %d = %+v, want the lost sling counted failed rather than dispatched", i, rec)
				}
			}
			f := dispatchVerdict(t, clock.Now(), townhealth.DispatchRecord{Active: true, Ticks: d.dispatchTickRecords()})
			if f.Verdict != townhealth.Red || f.Value != "stalled" {
				t.Errorf("dispatch field = %+v, want the stalled reading after %d ticks that started nothing", f, attentionDispatchIdleTicks)
			}
		})
	}
}

// gt-pk2q8: a sling that exits without a readable report or without naming a
// decision cannot show its bead hooked, so the tick that planned it counts the
// sling lost rather than dispatched. Counting it dispatched is the fail-open
// reading that lets every other sling in the same state hide behind it.
func TestRunSpecSlingWithoutAReadableOutcomeFeedsHealth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		sling cliReply
	}{
		{"unreadable output", cliReply{stdout: "not a report\n"}},
		{"no decision named", cliReply{stdout: string(wireOut(`{}`))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newSlingClock()
			fake := newFakeCLI(func(args []string) cliReply {
				if len(args) > 1 && args[1] == "sling" {
					return tc.sling
				}
				return cliReply{stdout: string(wireOut(backgroundTick()))}
			})
			logger, lines := lineLogger()
			d := specDispatchFake(t, fake, logger)
			d.clock = clock

			d.runSpecDispatch()
			waitSlings(t, d)

			tick := d.dispatchTickRecords()
			if len(tick) != 1 {
				t.Fatalf("tick records = %+v, want one", tick)
			}
			if tick[0].Dispatched != 0 || tick[0].Failed != 1 {
				t.Errorf("tick = %+v, want the sling counted failed rather than dispatched: %v", tick[0], lines())
			}
			if got := d.dispatchIdle.Status().Streak; got != 1 {
				t.Errorf("idle streak = %d, want the tick counted idle: %v", got, lines())
			}
		})
	}
}

// gt-pk2q8: a pending job the daemon cannot read names no bead, so it starts
// nothing; the tick that counted it dispatched must not keep it.
func TestAnUnreadableSlingJobIsCountedNotStarted(t *testing.T) {
	t.Parallel()
	clock := newSlingClock()
	const tick = `{"roster":"claude-sonnet 0/2","candidates":1,
		"dispatched":[{"bead":"gt-a","rig":"gastown","line":"gt-a: sling to gastown/p started in the background"}],
		"pending":[{"rig":"gastown"}]}`
	fake := newFakeCLI(func([]string) cliReply { return cliReply{stdout: string(wireOut(tick))} })
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = clock

	d.runSpecDispatch()

	rec := d.dispatchTickRecords()
	if len(rec) != 1 {
		t.Fatalf("tick records = %+v, want one", rec)
	}
	if rec[0].Dispatched != 0 || rec[0].Skipped != 1 {
		t.Errorf("tick = %+v, want the unreadable job counted skipped rather than dispatched: %v", rec[0], lines())
	}
	if got := d.dispatchIdle.Status().Streak; got != 1 {
		t.Errorf("idle streak = %d, want the tick counted idle: %v", got, lines())
	}
}

// A running sling holds the restart; so does a patrol scan in flight with it.
// The scan's hold must not be masked by the sling's, whose own bound is the
// sling case's, not the housekeeping cap's.
func TestDaemonWorkHoldNamesOtherSoftWorkAheadOfARunningSling(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", started: time.Now()})
	if hold := d.daemonWorkHold(); hold == nil || hold.name != specSlingHoldLabel || !hold.ownBound {
		t.Fatalf("hold = %+v, want the sling hold when nothing else runs", hold)
	}
	d.patrolScanRunning.Store(true)
	if hold := d.daemonWorkHold(); hold == nil || hold.name != "a patrol scan" || hold.ownBound {
		t.Fatalf("hold = %+v, want the patrol scan named ahead of the sling", hold)
	}
}

func TestNoSlingStartsWhileAnUpgradeRestartIsPending(t *testing.T) {
	t.Parallel()
	const tick = `{"roster":"claude-sonnet 0/2","candidates":1,
		"dispatched":[{"bead":"gt-a","rig":"gastown","line":"gt-a: sling to gastown/p started in the background"}],
		"pending":[` + `{"bead":"gt-a","rig":"gastown","agent":"claude-sonnet","paths":["internal/x.go"]}` + `]}`
	fake := newFakeCLI(func([]string) cliReply {
		return cliReply{stdout: string(wireOut(tick))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.upgradeRestartPending.Store(true)

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("spec", "sling"); len(got) != 0 {
		t.Fatalf("slings run = %v, want none under a pending restart", got)
	}
	if n := countContaining(lines(), "gt-a: sling not started: an upgrade restart is pending"); n != 1 {
		t.Errorf("lines = %v, want the held sling named", lines())
	}
	// The tick counted the job dispatched when it handed it over; a job that
	// never ran is the tick's skipped work, not its dispatch (gt-pk2q8).
	for i, rec := range d.dispatchTickRecords() {
		if rec.Dispatched != 0 || rec.Skipped != 1 {
			t.Errorf("tick %d = %+v, want the unstarted job counted skipped", i, rec)
		}
	}
	// The bead waits for the restart, not on the dispatcher, so the tick is
	// not an idle tick that the attention rule should count (gt-pk2q8).
	if got := d.dispatchIdle.Status().Streak; got != 0 {
		t.Errorf("idle streak = %d for a tick held by an upgrade restart, want 0: %v", got, lines())
	}
}

// The restart waits for the in-flight slings, bounded by the sling's own bound
// from the oldest one's start, not by the 2m housekeeping cap.
func TestUpgradeWaitsForARunningSlingToItsBound(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	bound := specSlingBound()
	if bound <= housekeepingRestartCap {
		t.Fatalf("specSlingBound = %s, want it past the housekeeping cap %s", bound, housekeepingRestartCap)
	}
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", Rig: "gastown", Agent: "claude-sonnet", started: now})

	if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(housekeepingRestartCap)) || d.checkUpgradeRestart(now.Add(bound-time.Second)) {
		t.Fatal("restarted under a running sling inside its bound")
	}
	if !strings.Contains(logs.String(), "waiting for "+specSlingHoldLabel) {
		t.Fatalf("the wait did not name the sling:\n%s", logs.String())
	}
	if !d.checkUpgradeRestart(now.Add(bound)) {
		t.Fatal("the restart must go once the sling is past its bound")
	}
	if !strings.Contains(logs.String(), specSlingHoldLabel+" is still in flight after its") {
		t.Fatalf("no cut-short line naming the sling:\n%s", logs.String())
	}
}

func TestUpgradeRestartGoesWhenTheRunningSlingsReturn(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", started: now})
	d.specSlings.begin(specSlingFlight{Bead: "gt-b", started: now})
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under running slings")
	}
	d.specSlings.end("gt-a")
	if d.checkUpgradeRestart(now.Add(time.Minute)) {
		t.Fatal("restarted while one sling still runs")
	}
	d.specSlings.end("gt-b")
	if !d.checkUpgradeRestart(now.Add(2 * time.Minute)) {
		t.Fatal("the restart must go as soon as the in-flight set is empty")
	}
}

// gt-nosod: only a status that says whether the bead is held answers the
// failure-label question. An empty or unrecognized status is no answer, so the
// label is withheld rather than written.
func TestSpecSlingBeadHeldAnswersOnlyKnownStatuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status           string
		wantHooked, know bool
	}{
		{"hooked", true, true},
		{"in_progress", true, true},
		{"closed", false, true},
		{"open", false, true},
		{"", false, false},
		{"something-new", false, false},
	}
	for _, c := range cases {
		hooked, known := specSlingBeadHeld(&beads.Issue{ID: "gt-x", Status: c.status})
		if hooked != c.wantHooked || known != c.know {
			t.Errorf("status %q: held=%v known=%v, want held=%v known=%v", c.status, hooked, known, c.wantHooked, c.know)
		}
	}
}
