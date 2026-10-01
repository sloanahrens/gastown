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

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// rigStatusAlerts records the escalations a test daemon raises and clears.
// Mutex-guarded because the daemon performs both off the calling goroutine.
type rigStatusAlerts struct {
	mu       sync.Mutex
	raised   []string
	messages []string
	cleared  []string
}

func (a *rigStatusAlerts) alert(key, source, message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.raised = append(a.raised, key)
	a.messages = append(a.messages, message)
}

func (a *rigStatusAlerts) clear(reason string, keys ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleared = append(a.cleared, keys...)
}

func (a *rigStatusAlerts) snapshot() (raised, messages, cleared []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.raised...), append([]string(nil), a.messages...), append([]string(nil), a.cleared...)
}

// drainRigEscalations waits until every escalation d has queued for rigName
// so far has run: the rig's queue is serial, so a marker queued behind them
// runs last.
func drainRigEscalations(d *Daemon, rigName string) {
	done := make(chan struct{})
	d.rigOperational.enqueueEscalation(rigName, d.logger.Printf, func() { close(done) })
	<-done
}

// waitForRigStatusAlert drains the fixture's escalation queue, where the
// escalation hooks run, and then requires cond, naming what never arrived.
func waitForRigStatusAlert(t *testing.T, f *rigStatusFixture, what string, cond func() bool) {
	t.Helper()
	drainRigEscalations(f.daemon, f.rigName)
	if !cond() {
		t.Fatalf("never saw %s", what)
	}
}

// rigStatusFixture is a town with one rig whose identity-bead reads are served
// in process (newRigStatusFakeFixture) or, in the integration tier, by a stub
// bd.
type rigStatusFixture struct {
	daemon    *Daemon
	townRoot  string
	rigName   string
	binDir    string // stub-bd fixtures only
	countPath string // stub-bd fixtures only
	logBuf    *bytes.Buffer
	alerts    *rigStatusAlerts

	// In-process fixtures (newRigStatusFakeFixture) answer the read from show,
	// on clock, instead of from a bd on PATH.
	clock *clockwork.FakeClock
	mu    sync.Mutex
	show  rigShow
	shows int
}

// rigShow is how an in-process fixture answers the identity-bead read. It may
// move clk to stand for the read's duration.
type rigShow func(clk *clockwork.FakeClock) (*beads.Issue, error)

// In-process counterparts of the stub behaviors below.
var (
	rigShowOperational rigShow = func(*clockwork.FakeClock) (*beads.Issue, error) {
		return &beads.Issue{ID: "tr-rig-testrig"}, nil
	}
	rigShowDocked rigShow = func(*clockwork.FakeClock) (*beads.Issue, error) {
		return &beads.Issue{ID: "tr-rig-testrig", Labels: []string{"status:docked"}}, nil
	}
	// rigShowTimedOut is what beads returns for a bd killed at its subprocess
	// deadline: an error wrapping context.DeadlineExceeded (pinned in
	// internal/beads by TestInitDeadlineIsReportedAsTimeout).
	rigShowTimedOut rigShow = func(*clockwork.FakeClock) (*beads.Issue, error) {
		return nil, fmt.Errorf("bd show tr-rig-testrig: %w (after 60s)", context.DeadlineExceeded)
	}
	// rigShowMissing is what beads returns when bd reports no such issue
	// (TestIntegrationIsRigOperational_MissingBeadThroughBD pins the real
	// bd read against it).
	rigShowMissing rigShow = func(*clockwork.FakeClock) (*beads.Issue, error) {
		return nil, fmt.Errorf("bd show tr-rig-testrig: %w", beads.ErrNotFound)
	}
	// rigShowSlowAnswer spends 50ms of the clock before answering.
	rigShowSlowAnswer rigShow = func(clk *clockwork.FakeClock) (*beads.Issue, error) {
		clk.Advance(50 * time.Millisecond)
		return &beads.Issue{ID: "tr-rig-testrig"}, nil
	}
)

// registerTestRigs writes the kernel files that make rigs (name -> beads
// prefix) registered and unparked: mayor/town.json and mayor/rigs.json. A rig
// missing from the registry reads as parked (fail closed, gt-y3pgh.4).
func registerTestRigs(t *testing.T, townRoot string, rigs map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town","version":2,"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	for name, prefix := range rigs {
		rc.Rigs[name] = config.RigEntry{GitURL: "x", BeadsConfig: &config.BeadsConfig{Prefix: prefix}}
	}
	if err := config.WriteConfigJSON(filepath.Join(townRoot, "mayor", "rigs.json"), &rc, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRigStatusFakeFixture is newRigStatusFixture with the identity-bead read
// answered in process by show, and the memo's clock a fake: no bd, no PATH,
// no wall-clock waits.
func newRigStatusFakeFixture(t *testing.T, show rigShow) *rigStatusFixture {
	t.Helper()
	townRoot := t.TempDir()
	const rigName = "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"beads":{"prefix":"tr"}}`), 0o644); err != nil {
		t.Fatalf("writing rig config.json: %v", err)
	}
	registerTestRigs(t, townRoot, map[string]string{rigName: "tr"})
	f := &rigStatusFixture{
		townRoot: townRoot,
		rigName:  rigName,
		logBuf:   &bytes.Buffer{},
		alerts:   &rigStatusAlerts{},
		clock:    newFixedClock(),
		show:     show,
	}
	f.daemon = &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(f.logBuf, "", 0),
		clock:  f.clock,
	}
	f.daemon.rigStatusAlert = f.alerts.alert
	f.daemon.rigStatusClear = f.alerts.clear
	f.daemon.rigBeadShowFn = func(_, id string) (*beads.Issue, error) {
		f.mu.Lock()
		f.shows++
		show := f.show
		f.mu.Unlock()
		if id != "tr-rig-testrig" {
			return nil, fmt.Errorf("unexpected rig bead %q", id)
		}
		return show(f.clock)
	}
	return f
}

// setShow swaps how an in-process fixture answers.
func (f *rigStatusFixture) setShow(show rigShow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.show = show
}

// showCount is the number of `bd show` calls the stub has served.
func (f *rigStatusFixture) showCount(t *testing.T) int {
	t.Helper()
	if f.clock != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.shows
	}
	data, err := os.ReadFile(f.countPath)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading stub counter: %v", err)
	}
	return strings.Count(string(data), "show\n")
}

// expire ages the memoized read for rigName past its TTL, so the next call
// performs a real read without the test sleeping out the window.
func (f *rigStatusFixture) expire(t *testing.T, rigName string) {
	t.Helper()
	cache := &f.daemon.rigOperational
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[rigName]
	if !ok {
		t.Fatalf("no memoized entry for rig %s", rigName)
	}
	entry.expiresAt = f.daemon.clk().Now().Add(-time.Second)
	cache.entries[rigName] = entry
}

// memoized returns the stored read for rigName.
func (f *rigStatusFixture) memoized(t *testing.T, rigName string) rigBeadEntry {
	t.Helper()
	cache := &f.daemon.rigOperational
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[rigName]
	if !ok {
		t.Fatalf("no memoized entry for rig %s", rigName)
	}
	return entry
}

// TestIsRigOperational_MemoizesDeterminationWithinTTL pins the fix for the cost
// half of gt-4nu3: one heartbeat evaluates a rig's docked/parked state from
// roughly ten call sites (patrol rig filters, witness and refinery auto-start,
// the convoy manager's isRigParked), and each of those used to pay its own bd
// subprocess. Within the TTL window only the first pays.
func TestIsRigOperational_MemoizesDeterminationWithinTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		show         rigShow
		wantOperable bool
		wantReason   string
	}{
		{
			name:         "operational rig",
			show:         rigShowOperational,
			wantOperable: true,
		},
		{
			name:         "docked rig",
			show:         rigShowDocked,
			wantOperable: false,
			wantReason:   "rig is docked (global)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRigStatusFakeFixture(t, tc.show)

			for i := 0; i < 3; i++ {
				operational, reason := f.daemon.isRigOperational(f.rigName)
				if operational != tc.wantOperable {
					t.Fatalf("call %d: operational = %v, want %v (reason %q)", i+1, operational, tc.wantOperable, reason)
				}
				if tc.wantReason != "" && reason != tc.wantReason {
					t.Fatalf("call %d: reason = %q, want %q", i+1, reason, tc.wantReason)
				}
			}

			if got := f.showCount(t); got != 1 {
				t.Errorf("stub bd served %d rig-bead reads across 3 calls, want 1 (the memoized path must not be re-consulted within the TTL)", got)
			}

			// Past the window the rig is read again, so a dock or park set
			// outside the daemon is still noticed on a later heartbeat.
			f.expire(t, f.rigName)
			if operational, _ := f.daemon.isRigOperational(f.rigName); operational != tc.wantOperable {
				t.Errorf("after TTL expiry: operational = %v, want %v", operational, tc.wantOperable)
			}
			if got := f.showCount(t); got != 2 {
				t.Errorf("stub bd served %d rig-bead reads, want 2 after the entry expired", got)
			}

			// An answered read is not a failure, so it must neither raise nor
			// clear anything: a rig parked or docked is a decision an operator
			// made, not a condition to wake the Mayor about, and the clear is a
			// `gt escalate clear` subprocess (the cost class this file exists to
			// remove) that belongs only to a failure that is over. The hooks run
			// on the rig's escalation queue, hence the drain before asserting.
			drainRigEscalations(f.daemon, f.rigName)
			if raised, _, cleared := f.alerts.snapshot(); len(raised) != 0 || len(cleared) != 0 {
				t.Errorf("escalations raised=%v cleared=%v, want neither for a rig whose status was read", raised, cleared)
			}
		})
	}
}

// TestIsRigOperational_MemoWindowStartsWhenTheReadAnswers pins the bug a
// pre-read clock hides: a read that consumes part of the subprocess budget must
// not hand back an entry that is already part of the way through its window. On
// the host this bead is about - where a read can spend the whole 60s budget -
// expiring from before the read leaves the entry dead on arrival, and every call
// site in the tick re-pays the budget the memo exists to save.
func TestIsRigOperational_MemoWindowStartsWhenTheReadAnswers(t *testing.T) {
	t.Parallel()
	f := newRigStatusFakeFixture(t, rigShowSlowAnswer)

	start := f.clock.Now()
	if operational, reason := f.daemon.isRigOperational(f.rigName); !operational {
		t.Fatalf("stub answers a status-less bead, got not operational: %q", reason)
	}

	// The read spends 50ms, well past this margin, before answering, so an expiry
	// measured from the call would land at start+TTL and fail this.
	entry := f.memoized(t, f.rigName)
	if floor := start.Add(rigOperationalCacheTTL + 20*time.Millisecond); !entry.expiresAt.After(floor) {
		t.Errorf("entry expires at %v (%s after the call began); the window must be measured from the answer, not from the request",
			entry.expiresAt, entry.expiresAt.Sub(start).Round(time.Millisecond))
	}
}

// TestIsRigOperational_ParkTakesEffectWithoutWaitingForTheMemo covers the layer
// split: `gt rig park` writes the registry record, which is cheap to read, so
// parking a rig must be visible on the next evaluation even though the identity
// bead read behind it is memoized for a minute. (The dock label is global and
// read over a subprocess, so that one is bounded by the memo instead.)
func TestIsRigOperational_ParkTakesEffectWithoutWaitingForTheMemo(t *testing.T) {
	t.Parallel()
	f := newRigStatusFakeFixture(t, rigShowOperational)

	if operational, reason := f.daemon.isRigOperational(f.rigName); !operational {
		t.Fatalf("rig starts operational, got %q", reason)
	}
	// The park below only proves something if a read is already memoized: this
	// pins that the memo exists and says "active" before it.
	if entry := f.memoized(t, f.rigName); entry.failed != "" || entry.verdict != rigBeadActive {
		t.Fatalf("expected a memoized active verdict before parking, got %+v", entry)
	}

	// Exactly what `gt rig park` does.
	if _, err := townconfig.Park(f.townRoot, f.rigName, config.RigParked{Since: f.clock.Now(), By: "test", Reason: "maintenance"}); err != nil {
		t.Fatalf("parking rig: %v", err)
	}

	operational, reason := f.daemon.isRigOperational(f.rigName)
	if operational {
		t.Error("a rig parked after the memo was filled must read as not operational")
	}
	if !strings.HasPrefix(reason, "parked since") || !strings.Contains(reason, "maintenance") {
		t.Errorf("reason = %q, want the registry record", reason)
	}
}

// TestIsRigOperational_UnreadableRegistryFailsClosed: a registry that does
// not load parks every rig, and no bead is read for it.
func TestIsRigOperational_UnreadableRegistryFailsClosed(t *testing.T) {
	t.Parallel()
	f := newRigStatusFakeFixture(t, rigShowOperational)
	if err := os.WriteFile(filepath.Join(f.townRoot, "mayor", "rigs.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	operational, reason := f.daemon.isRigOperational(f.rigName)
	if operational || !strings.Contains(reason, "treated as parked") {
		t.Errorf("isRigOperational = %v, %q; want not operational, fail closed", operational, reason)
	}
	if n := f.showCount(t); n != 0 {
		t.Errorf("bead reads = %d, want 0 for a rig that reads parked", n)
	}
}

// TestIsRigOperational_EscalationsForARigAreSerialized covers the ordering the
// raise and the clear would otherwise not have: both are subprocess work handed
// to their own goroutine, so a clear can close the alert of a rig that started
// failing again before it ran. The raised hook here blocks until the test lets
// it go, which is long enough for the recovery clear to have run out of order if
// nothing serialized them.
func TestIsRigOperational_EscalationsForARigAreSerialized(t *testing.T) {
	t.Parallel()
	f := newRigStatusFakeFixture(t, rigShowTimedOut)

	raiseStarted := make(chan struct{})
	releaseRaise := make(chan struct{})
	var mu sync.Mutex
	var order []string
	raiseDone := make(chan struct{})

	f.daemon.rigStatusAlert = func(key, source, message string) {
		mu.Lock()
		order = append(order, "raise:"+key)
		mu.Unlock()
		close(raiseStarted)
		<-releaseRaise
		close(raiseDone)
	}
	f.daemon.rigStatusClear = func(reason string, keys ...string) {
		mu.Lock()
		defer mu.Unlock()
		for _, key := range keys {
			order = append(order, "clear:"+key)
		}
	}

	// Failing read: the raise is queued, and it blocks inside the hook.
	if operational, _ := f.daemon.isRigOperational(f.rigName); operational {
		t.Fatal("a timed-out read must fail closed")
	}
	<-raiseStarted

	// The rig answers again while that raise is still in flight.
	f.setShow(rigShowOperational)
	f.expire(t, f.rigName)
	if operational, reason := f.daemon.isRigOperational(f.rigName); !operational {
		t.Fatalf("recovered rig reported not operational: %q", reason)
	}

	// The clear must be waiting behind the raise, not racing it.
	mu.Lock()
	queuedClear := len(order) > 1
	mu.Unlock()
	if queuedClear {
		t.Fatalf("the clear overtook the raise still in flight: order = %v", order)
	}

	close(releaseRaise)
	<-raiseDone
	waitForRigStatusAlert(t, f, "the clear to follow the raise", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 2
	})

	mu.Lock()
	defer mu.Unlock()
	key := rigStatusAlertKey(f.rigName)
	want := []string{"raise:" + key, "clear:" + key}
	if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("escalation order = %v, want %v", order, want)
	}
}

// TestIsRigOperational_TimeoutFailsClosedAndEscalates exercises the branch a
// CPU-starved host takes: the rig-bead read never returns, so bd is killed at
// its subprocess budget. The rig must still be reported not operational (the
// fail-closed direction that keeps a docked rig's agents from being started),
// the failure must be logged as a timeout, and the suppression must be
// escalated - once per failure episode, not once per call site.
func TestIsRigOperational_TimeoutFailsClosedAndEscalates(t *testing.T) {
	t.Parallel()
	f := newRigStatusFakeFixture(t, rigShowTimedOut)

	operational, reason := f.daemon.isRigOperational(f.rigName)
	if operational {
		t.Fatalf("a timed-out lookup must fail closed, got operational=true")
	}
	if !strings.Contains(reason, "cannot verify rig status") || !strings.Contains(reason, "lookup timed out") {
		t.Errorf("reason = %q, want it to name the timeout", reason)
	}

	logged := f.logBuf.String()
	if !strings.Contains(logged, "lookup timed out") {
		t.Errorf("log must name the timeout, got:\n%s", logged)
	}
	if !strings.Contains(logged, "assuming not operational") {
		t.Errorf("log must state the fail-closed assumption, got:\n%s", logged)
	}
	if strings.Contains(logged, "rig bead missing") {
		t.Errorf("a timeout must not be logged as a missing bead, got:\n%s", logged)
	}

	key := rigStatusAlertKey(f.rigName)
	waitForRigStatusAlert(t, f, "the timeout escalation", func() bool {
		raised, _, _ := f.alerts.snapshot()
		return len(raised) == 1
	})
	raised, messages, _ := f.alerts.snapshot()
	if raised[0] != key {
		t.Errorf("escalated fingerprint = %q, want %q", raised[0], key)
	}
	if !strings.Contains(messages[0], "lookup timed out") {
		t.Errorf("escalation message = %q, want it to name the timeout", messages[0])
	}

	// A second call is answered from the failure memo: the whole point is that
	// a starved host pays the subprocess budget once per failure window rather
	// than once per call site, and that one rig holds one open alert.
	operational, _ = f.daemon.isRigOperational(f.rigName)
	if operational {
		t.Error("second call must still fail closed")
	}
	if got := f.showCount(t); got != 1 {
		t.Errorf("stub bd served %d reads, want 1: a memoized failure must not re-pay the budget within its window", got)
	}
	// Even a fresh lookup for the same still-failing rig does not open a
	// second alert.
	f.expire(t, f.rigName)
	if operational, _ := f.daemon.isRigOperational(f.rigName); operational {
		t.Error("third call must still fail closed")
	}
	if got := f.showCount(t); got != 2 {
		t.Errorf("stub bd served %d reads, want 2 after the failure window expired", got)
	}
	if raised, _, _ := f.alerts.snapshot(); len(raised) != 1 {
		t.Errorf("escalations raised = %d, want 1 — one open alert per failing rig", len(raised))
	}

	// The alert must not outlive the condition: once the rig answers, the
	// escalation is cleared.
	f.setShow(rigShowOperational)
	f.expire(t, f.rigName)
	if operational, reason := f.daemon.isRigOperational(f.rigName); !operational {
		t.Fatalf("recovered rig reported not operational: %q", reason)
	}
	waitForRigStatusAlert(t, f, "the escalation clear", func() bool {
		_, _, cleared := f.alerts.snapshot()
		return len(cleared) == 1
	})
	if _, _, cleared := f.alerts.snapshot(); cleared[0] != key {
		t.Errorf("cleared fingerprint = %q, want %q", cleared[0], key)
	}
}

// TestIsRigOperational_MissingBeadIsDistinctFromTimeout covers the second half
// of criterion four of gt-4nu3: a missing identity bead and a starved-host
// timeout used to produce the same "(assuming not operational)" line, and they
// call for different responses (data repair vs. a saturated host).
func TestIsRigOperational_MissingBeadIsDistinctFromTimeout(t *testing.T) {
	t.Parallel()
	f := newRigStatusFakeFixture(t, rigShowMissing)

	operational, reason := f.daemon.isRigOperational(f.rigName)
	if operational {
		t.Fatal("a missing identity bead must fail closed")
	}
	if !strings.Contains(reason, "cannot verify rig status") || !strings.Contains(reason, "rig bead missing") {
		t.Errorf("reason = %q, want it to name the missing bead", reason)
	}

	logged := f.logBuf.String()
	if !strings.Contains(logged, "rig bead missing") {
		t.Errorf("log must name the missing bead, got:\n%s", logged)
	}
	if strings.Contains(logged, "lookup timed out") {
		t.Errorf("a missing bead must not be logged as a timeout, got:\n%s", logged)
	}

	waitForRigStatusAlert(t, f, "the missing-bead escalation", func() bool {
		raised, _, _ := f.alerts.snapshot()
		return len(raised) == 1
	})
	_, messages, _ := f.alerts.snapshot()
	if !strings.Contains(messages[0], "rig bead missing") {
		t.Errorf("escalation message = %q, want it to name the missing bead", messages[0])
	}
}

// TestIsRigOperational_ConcurrentCallers runs the memo from several goroutines
// at once, which is how the daemon reaches it (heartbeat goroutine, rigPool
// workers, the convoy manager's callback). Run under -race, this is what keeps
// the cache honest; the assertion after the race is that the memo it left behind
// is populated and usable rather than corrupted.
func TestIsRigOperational_ConcurrentCallers(t *testing.T) {
	t.Parallel()
	f := newRigStatusFakeFixture(t, rigShowOperational)

	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				operational, reason := f.daemon.isRigOperational(f.rigName)
				if !operational {
					t.Errorf("goroutine %d: operational = false (%q)", idx, reason)
					return
				}
				results[idx] = operational
			}
		}(i)
	}
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Errorf("goroutine %d never saw an operational rig", i)
		}
	}

	// Racing callers may each have paid for their own read, but they must have
	// left a usable memo behind: the next caller is served from it.
	settled := f.showCount(t)
	if operational, reason := f.daemon.isRigOperational(f.rigName); !operational {
		t.Fatalf("post-race call reported not operational: %q", reason)
	}
	if got := f.showCount(t); got != settled {
		t.Errorf("stub bd served %d reads after the race, want %d: the memo left by concurrent callers is not usable", got, settled)
	}
	if entry := f.memoized(t, f.rigName); entry.failed != "" {
		t.Errorf("memoized read after the race is a failure (%q), want the answered verdict", entry.failed)
	}
}
