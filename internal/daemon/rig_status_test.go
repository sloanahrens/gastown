package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/wisp"
)

// rigStatusBDStub is a POSIX shell stub for the bd the rig-status lookup shells
// out to: the real lookup reads the rig's identity bead with `bd show`, and the
// whole point of gt-4nu3 is what happens when that subprocess is slow or fails.
// Each `show` served is appended to countPath, so a test can prove the memoized
// path is not re-consulted.
//
// The capability probe (`bd --allow-stale version`) is answered without
// counting and without honoring showScript: it runs before every real call, and
// a probe that slept out showScript's timeout would make the daemon read "bd
// does not support --allow-stale" instead of exercising the branch under test.
func writeRigStatusBDStub(t *testing.T, binDir, countPath, showScript string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("rig-status stubs are POSIX shell scripts")
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "version" ] || [ "$2" = "version" ]; then
  echo "bd 9.9.9"
  exit 0
fi
if [ "$1" = "show" ] || [ "$2" = "show" ]; then
  echo show >> %s
fi
%s
`, countPath, showScript)

	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing bd stub: %v", err)
	}
	return path
}

// Stub behaviors for the branches the lookup can take.
const (
	// rigStatusStubOperational answers with a rig identity bead that carries no
	// status label: an operational rig, the common case.
	rigStatusStubOperational = `echo '[{"id":"tr-rig-testrig","labels":[]}]'`
	// rigStatusStubDocked answers with the global docked label.
	rigStatusStubDocked = `echo '[{"id":"tr-rig-testrig","labels":["status:docked"]}]'`
	// rigStatusStubMissing answers the way bd does when the bead is not there.
	rigStatusStubMissing = `echo 'Error: no issue found: tr-rig-testrig' >&2
exit 1`
	// rigStatusStubSlow blocks past any budget the test sets, so the subprocess
	// is killed at its deadline. It execs sleep so the killed process is the
	// one holding stdout - a shell that merely waits on a child would survive
	// the kill and hold the pipe open.
	rigStatusStubSlow = `exec sleep 60`
	// rigStatusStubSlowAnswer takes long enough to answer that the difference
	// between a window measured before the read and one measured after it is
	// unambiguous, while staying far under the budget.
	rigStatusStubSlowAnswer = `sleep 0.05
echo '[{"id":"tr-rig-testrig","labels":[]}]'`
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

// waitForRigStatusAlert polls until cond holds, for the escalation hooks that
// run in their own goroutine. It is waitFor (plugin_script_test.go) with a
// message, so a failure says which escalation never arrived.
func waitForRigStatusAlert(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// rigStatusFixture is a town with one rig whose identity-bead reads are served
// by a stub bd.
type rigStatusFixture struct {
	daemon    *Daemon
	townRoot  string
	rigName   string
	binDir    string
	countPath string
	logBuf    *bytes.Buffer
	alerts    *rigStatusAlerts
}

func newRigStatusFixture(t *testing.T, showScript string) *rigStatusFixture {
	t.Helper()

	townRoot := t.TempDir()
	const rigName = "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	// The prefix is what turns the rig name into its identity bead ID
	// (tr-rig-testrig), which is the read this bead is about.
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"beads":{"prefix":"tr"}}`), 0o644); err != nil {
		t.Fatalf("writing rig config.json: %v", err)
	}

	binDir := t.TempDir()
	countPath := filepath.Join(t.TempDir(), "bd-shows")
	writeRigStatusBDStub(t, binDir, countPath, showScript)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logBuf := &bytes.Buffer{}
	alerts := &rigStatusAlerts{}
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(logBuf, "", 0),
	}
	// Stand in for the daemon's escalateAlert/clearAlerts, which shell out to
	// `gt escalate`: the escalation must be observable, not a real town write.
	d.rigStatusAlert = alerts.alert
	d.rigStatusClear = alerts.clear

	return &rigStatusFixture{
		daemon:    d,
		townRoot:  townRoot,
		rigName:   rigName,
		binDir:    binDir,
		countPath: countPath,
		logBuf:    logBuf,
		alerts:    alerts,
	}
}

// setShowScript swaps what the stub bd answers, so a test can take the same rig
// from failing lookups to answering ones.
func (f *rigStatusFixture) setShowScript(t *testing.T, showScript string) {
	t.Helper()
	writeRigStatusBDStub(t, f.binDir, f.countPath, showScript)
}

// showCount is the number of `bd show` calls the stub has served.
func (f *rigStatusFixture) showCount(t *testing.T) int {
	t.Helper()
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
	entry.expiresAt = time.Now().Add(-time.Second)
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
	tests := []struct {
		name         string
		showScript   string
		wantOperable bool
		wantReason   string
	}{
		{
			name:         "operational rig",
			showScript:   rigStatusStubOperational,
			wantOperable: true,
		},
		{
			name:         "docked rig",
			showScript:   rigStatusStubDocked,
			wantOperable: false,
			wantReason:   "rig is docked (global)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRigStatusFixture(t, tc.showScript)

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
			// on their own goroutine, hence the settle before asserting.
			time.Sleep(50 * time.Millisecond)
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
	f := newRigStatusFixture(t, rigStatusStubSlowAnswer)

	start := time.Now()
	if operational, reason := f.daemon.isRigOperational(f.rigName); !operational {
		t.Fatalf("stub answers a status-less bead, got not operational: %q", reason)
	}

	// The stub sleeps well past this margin before answering, so an expiry
	// measured from the call would land at start+TTL and fail this.
	entry := f.memoized(t, f.rigName)
	if floor := start.Add(rigOperationalCacheTTL + 20*time.Millisecond); !entry.expiresAt.After(floor) {
		t.Errorf("entry expires at %v (%s after the call began); the window must be measured from the answer, not from the request",
			entry.expiresAt, entry.expiresAt.Sub(start).Round(time.Millisecond))
	}
}

// TestIsRigOperational_ParkTakesEffectWithoutWaitingForTheMemo covers the layer
// split: `gt rig park` writes the local wisp layer, which is cheap to read, so
// parking a rig must be visible on the next evaluation even though the identity
// bead read behind it is memoized for a minute. (The dock label is global and
// read over a subprocess, so that one is bounded by the memo instead.)
func TestIsRigOperational_ParkTakesEffectWithoutWaitingForTheMemo(t *testing.T) {
	f := newRigStatusFixture(t, rigStatusStubOperational)

	if operational, reason := f.daemon.isRigOperational(f.rigName); !operational {
		t.Fatalf("rig starts operational, got %q", reason)
	}
	// The park below only proves something if a read is already memoized: this
	// pins that the memo exists and says "active" before it.
	if entry := f.memoized(t, f.rigName); entry.failed != "" || entry.verdict != rigBeadActive {
		t.Fatalf("expected a memoized active verdict before parking, got %+v", entry)
	}

	// Exactly what `gt rig park` does.
	if err := wisp.NewConfig(f.townRoot, f.rigName).Set("status", "parked"); err != nil {
		t.Fatalf("parking rig: %v", err)
	}

	operational, reason := f.daemon.isRigOperational(f.rigName)
	if operational {
		t.Error("a rig parked after the memo was filled must read as not operational")
	}
	if reason != "rig is parked" {
		t.Errorf("reason = %q, want the wisp layer's %q", reason, "rig is parked")
	}
}

// TestIsRigOperational_EscalationsForARigAreSerialized covers the ordering the
// raise and the clear would otherwise not have: both are subprocess work handed
// to their own goroutine, so a clear can close the alert of a rig that started
// failing again before it ran. The raised hook here blocks until the test lets
// it go, which is long enough for the recovery clear to have run out of order if
// nothing serialized them.
func TestIsRigOperational_EscalationsForARigAreSerialized(t *testing.T) {
	// Shrink the bd subprocess budget: the blocked raise has to be standing
	// while the rig recovers, and the failure it comes from must not cost 60s.
	t.Setenv("GT_BD_TIMEOUT_SEC", "1")
	f := newRigStatusFixture(t, rigStatusStubSlow)

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
	f.setShowScript(t, rigStatusStubOperational)
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
	waitForRigStatusAlert(t, "the clear to follow the raise", func() bool {
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
	// Shrink the bd subprocess budget so the test does not wait out the
	// production 60s; the branch under test is the same one.
	t.Setenv("GT_BD_TIMEOUT_SEC", "1")
	f := newRigStatusFixture(t, rigStatusStubSlow)

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
	waitForRigStatusAlert(t, "the timeout escalation", func() bool {
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
	f.setShowScript(t, rigStatusStubOperational)
	f.expire(t, f.rigName)
	if operational, reason := f.daemon.isRigOperational(f.rigName); !operational {
		t.Fatalf("recovered rig reported not operational: %q", reason)
	}
	waitForRigStatusAlert(t, "the escalation clear", func() bool {
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
	f := newRigStatusFixture(t, rigStatusStubMissing)

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

	waitForRigStatusAlert(t, "the missing-bead escalation", func() bool {
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
	f := newRigStatusFixture(t, rigStatusStubOperational)

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
