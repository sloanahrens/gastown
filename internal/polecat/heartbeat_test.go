package polecat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/rig"
)

func TestTouchAndReadSessionHeartbeat(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// No heartbeat initially
	hb := ReadSessionHeartbeat(townRoot, "gt-test-session")
	if hb != nil {
		t.Fatal("expected nil heartbeat before touch")
	}

	// Touch heartbeat
	TouchSessionHeartbeat(townRoot, "gt-test-session")

	// Read it back
	hb = ReadSessionHeartbeat(townRoot, "gt-test-session")
	if hb == nil {
		t.Fatal("expected non-nil heartbeat after touch")
	}

	if time.Since(hb.Timestamp) > 5*time.Second {
		t.Errorf("heartbeat timestamp too old: %v", hb.Timestamp)
	}

	// v2: TouchSessionHeartbeat writes state="working" by default (gt-3vr5)
	if hb.State != HeartbeatWorking {
		t.Errorf("heartbeat state = %q, want %q", hb.State, HeartbeatWorking)
	}
}

func TestTouchSessionHeartbeatWithState(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	TouchSessionHeartbeatWithState(townRoot, "gt-test-state", HeartbeatExiting, "gt done", "gt-abc123")

	hb := ReadSessionHeartbeat(townRoot, "gt-test-state")
	if hb == nil {
		t.Fatal("expected non-nil heartbeat after touch with state")
	}

	if hb.State != HeartbeatExiting {
		t.Errorf("state = %q, want %q", hb.State, HeartbeatExiting)
	}
	if hb.Context != "gt done" {
		t.Errorf("context = %q, want %q", hb.Context, "gt done")
	}
	if hb.Bead != "gt-abc123" {
		t.Errorf("bead = %q, want %q", hb.Bead, "gt-abc123")
	}
}

func TestSessionHeartbeat_EffectiveState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state HeartbeatState
		want  HeartbeatState
	}{
		{"empty (v1 compat)", "", HeartbeatWorking},
		{"working", HeartbeatWorking, HeartbeatWorking},
		{"idle", HeartbeatIdle, HeartbeatIdle},
		{"exiting", HeartbeatExiting, HeartbeatExiting},
		{"stuck", HeartbeatStuck, HeartbeatStuck},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hb := &SessionHeartbeat{State: tt.state}
			if got := hb.EffectiveState(); got != tt.want {
				t.Errorf("EffectiveState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionHeartbeat_IsV2(t *testing.T) {
	t.Parallel()
	// v1 heartbeat (no state)
	v1 := &SessionHeartbeat{Timestamp: time.Now()}
	if v1.IsV2() {
		t.Error("expected IsV2()=false for v1 heartbeat")
	}

	// v2 heartbeat (has state)
	v2 := &SessionHeartbeat{Timestamp: time.Now(), State: HeartbeatWorking}
	if !v2.IsV2() {
		t.Error("expected IsV2()=true for v2 heartbeat")
	}
}

func TestIsSessionHeartbeatStale_NoFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	stale, exists := IsSessionHeartbeatStale(townRoot, "nonexistent")
	if exists {
		t.Error("expected exists=false for missing heartbeat")
	}
	if stale {
		t.Error("expected stale=false for missing heartbeat")
	}
}

func TestIsSessionHeartbeatStale_Fresh(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	TouchSessionHeartbeat(townRoot, "gt-test-fresh")

	stale, exists := IsSessionHeartbeatStale(townRoot, "gt-test-fresh")
	if !exists {
		t.Error("expected exists=true for fresh heartbeat")
	}
	if stale {
		t.Error("expected stale=false for fresh heartbeat")
	}
}

func TestIsSessionHeartbeatStale_Old(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Write a heartbeat with an old timestamp
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	oldTime := time.Now().Add(-10 * time.Minute).UTC()
	data := []byte(`{"timestamp":"` + oldTime.Format(time.RFC3339Nano) + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "gt-test-stale.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	stale, exists := IsSessionHeartbeatStale(townRoot, "gt-test-stale")
	if !exists {
		t.Error("expected exists=true for old heartbeat")
	}
	if !stale {
		t.Error("expected stale=true for 10-minute-old heartbeat")
	}
}

func TestRemoveSessionHeartbeat(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	TouchSessionHeartbeat(townRoot, "gt-test-remove")

	// Verify it exists
	hb := ReadSessionHeartbeat(townRoot, "gt-test-remove")
	if hb == nil {
		t.Fatal("expected heartbeat to exist before removal")
	}

	// Remove it
	RemoveSessionHeartbeat(townRoot, "gt-test-remove")

	// Verify it's gone
	hb = ReadSessionHeartbeat(townRoot, "gt-test-remove")
	if hb != nil {
		t.Error("expected nil heartbeat after removal")
	}
}

func TestRemoveSessionHeartbeat_NoopOnMissing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// Should not panic or error on missing file
	RemoveSessionHeartbeat(townRoot, "nonexistent")
}

func TestIsSessionProcessDead_HeartbeatFresh(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	sessionName := "gt-test-hb-alive"

	// Touch a fresh heartbeat — isSessionProcessDead should return false
	TouchSessionHeartbeat(townRoot, sessionName)

	dead := isSessionProcessDead(nil, sessionName, townRoot)
	if dead {
		t.Error("expected alive (dead=false) for session with fresh heartbeat")
	}
}

func writeStaleSessionHeartbeat(t *testing.T, townRoot, sessionName string) {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(SessionHeartbeat{
		Timestamp: time.Now().Add(-10 * time.Minute).UTC(),
		State:     HeartbeatWorking,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestIsSessionProcessDead_HeartbeatStaleUsesAgentLiveness(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	tests := []struct {
		name     string
		alive    bool
		aliveErr error
		wantDead bool
	}{
		{name: "live_agent", alive: true, wantDead: false},
		{name: "not_live_agent", alive: false, wantDead: true},
		{name: "query_error", alive: false, aliveErr: errors.New("tmux query failed"), wantDead: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionName := "gt-test-hb-stale-" + tt.name
			writeStaleSessionHeartbeat(t, townRoot, sessionName)

			tm := newFakeProbe()
			tm.setAlive(sessionName, tt.alive)
			tm.aliveErr = tt.aliveErr

			dead := isSessionProcessDead(tm, sessionName, townRoot)
			if dead != tt.wantDead {
				t.Fatalf("isSessionProcessDead() = %v, want %v", dead, tt.wantDead)
			}
			if got := tm.probedSessions(); len(got) != 1 || got[0] != sessionName {
				t.Fatalf("liveness probed %v, want exactly [%s]", got, sessionName)
			}
		})
	}
}

func TestIsSessionProcessDead_HeartbeatStaleWithoutTmuxFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	sessionName := "gt-test-hb-stale-no-tmux"
	writeStaleSessionHeartbeat(t, townRoot, sessionName)

	dead := isSessionProcessDead(nil, sessionName, townRoot)
	if dead {
		t.Error("expected dead=false for stale heartbeat without tmux liveness evidence")
	}
}

// TestPolecatSessionState pins the stale verdict that decides whether a
// polecat whose session still exists is reclaimed as dead (gt-22hdp.54): the
// heartbeat is read from the town root (the rig's parent), and only a stale
// heartbeat plus a confirmed-dead agent is stale.
func TestPolecatSessionState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		noTmux      bool
		noSession   bool
		heartbeat   string // "fresh", "stale"
		alive       bool
		aliveErr    error
		wantRunning bool
		wantStale   bool
	}{
		{name: "nil_tmux", noTmux: true, heartbeat: "stale", wantRunning: false, wantStale: false},
		{name: "no_session", noSession: true, heartbeat: "stale", wantRunning: false, wantStale: false},
		{name: "fresh_heartbeat", heartbeat: "fresh", wantRunning: true, wantStale: false},
		{name: "stale_heartbeat_agent_dead", heartbeat: "stale", alive: false, wantRunning: true, wantStale: true},
		{name: "stale_heartbeat_agent_alive", heartbeat: "stale", alive: true, wantRunning: true, wantStale: false},
		{name: "stale_heartbeat_probe_error", heartbeat: "stale", aliveErr: errors.New("tmux query failed"), wantRunning: true, wantStale: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			r := &rig.Rig{Name: "testrig", Path: filepath.Join(townRoot, "testrig")}

			tm := newFakeProbe()
			var probe sessionProbe = tm
			if tt.noTmux {
				probe = nil
			}
			mgr := newTestManager(r, nil, probe, nil)
			sessionName := mgr.sessionName("toast")

			if !tt.noSession {
				if err := tm.NewSessionWithCommandAndEnv(sessionName, townRoot, "sleep 300", nil); err != nil {
					t.Fatalf("create session: %v", err)
				}
			}
			switch tt.heartbeat {
			case "fresh":
				TouchSessionHeartbeat(townRoot, sessionName)
			case "stale":
				writeStaleSessionHeartbeat(t, townRoot, sessionName)
			}
			tm.setAlive(sessionName, tt.alive)
			tm.aliveErr = tt.aliveErr

			running, stale := mgr.polecatSessionState("toast")
			if running != tt.wantRunning || stale != tt.wantStale {
				t.Fatalf("polecatSessionState() = (running=%v, stale=%v), want (%v, %v)", running, stale, tt.wantRunning, tt.wantStale)
			}
		})
	}
}

func TestIsSessionProcessDead_EmptyTownRoot(t *testing.T) {
	t.Parallel()
	// With empty townRoot, heartbeat check is skipped entirely.
	// This tests backward compatibility when townRoot isn't available.
	// We can't test the full PID fallback without a real tmux session,
	// but we verify no panic with empty townRoot.
	sessionName := "gt-test-no-townroot"

	// Empty townRoot skips heartbeat, falls through to PID check.
	// Can't test PID path without tmux, but verify heartbeat path is skipped.
	stale, exists := IsSessionHeartbeatStale("", sessionName)
	if exists {
		t.Error("expected exists=false with empty townRoot")
	}
	if stale {
		t.Error("expected stale=false with empty townRoot")
	}
}

func TestReadSessionHeartbeat_V1BackwardsCompat(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Write a v1 heartbeat (timestamp only, no state field)
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	ts := time.Now().UTC()
	data := []byte(`{"timestamp":"` + ts.Format(time.RFC3339Nano) + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "gt-test-v1.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	hb := ReadSessionHeartbeat(townRoot, "gt-test-v1")
	if hb == nil {
		t.Fatal("expected non-nil heartbeat for v1 format")
	}

	// State should be empty (v1)
	if hb.State != "" {
		t.Errorf("v1 heartbeat state = %q, want empty", hb.State)
	}

	// IsV2 should return false
	if hb.IsV2() {
		t.Error("expected IsV2()=false for v1 heartbeat")
	}

	// EffectiveState should default to working
	if hb.EffectiveState() != HeartbeatWorking {
		t.Errorf("v1 EffectiveState() = %q, want %q", hb.EffectiveState(), HeartbeatWorking)
	}
}

// writeHeartbeat writes an arbitrary heartbeat straight to disk, bypassing
// TouchSessionHeartbeatWithState so tests can seed a stale timestamp.
func writeHeartbeat(t *testing.T, townRoot, sessionName string, hb SessionHeartbeat) {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestStartHeartbeatKeepAlive_RenewsExitingUntilStopped is the gt-azmw
// regression test: gt done writes state="exiting" once and can then sit inside
// a silent bounded gate for 30 minutes, so the heartbeat has to keep being
// renewed for as long as that stage runs — otherwise every consumer (the
// witness's 3m stale threshold, the daemon idle-reaper's 15m) sees a dead agent
// and kills a healthy polecat mid-submit.
//
// The assertions are count/content, not mere absence: the renewal must
// actually happen, must keep happening on every tick, and must stop happening
// on stop. Dropping the ticker (keeping only the first write) fails the second;
// dropping the stop path fails the third.
func TestStartHeartbeatKeepAlive_RenewsExitingUntilStopped(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	sessionName := "myr-mycat"
	const interval = 30 * time.Second
	clk := clockwork.NewFakeClockAt(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))

	// Seed the heartbeat exactly as gt done leaves it after its long gate
	// start: state=exiting, already past every consumer's stale threshold.
	seeded := clk.Now().Add(-19 * time.Minute)
	writeHeartbeat(t, townRoot, sessionName, SessionHeartbeat{
		Timestamp: seeded,
		State:     HeartbeatExiting,
		Context:   "gt done",
		Bead:      "gt-azmw",
	})

	stop := startHeartbeatKeepAlive(clk, townRoot, sessionName, "gt done", "gt-azmw", interval)
	defer stop()

	// 1. The first renewal is synchronous, so the stage starts fresh.
	first := ReadSessionHeartbeat(townRoot, sessionName)
	if first == nil {
		t.Fatal("expected a heartbeat after starting the keep-alive")
	}
	if !first.Timestamp.Equal(clk.Now()) {
		t.Fatalf("keep-alive did not renew the seeded heartbeat: got %v, want %v (seeded %v)", first.Timestamp, clk.Now(), seeded)
	}
	// Content, not just freshness: a renewal that downgrades the state (or
	// drops the bead) would let the witness's exiting-check miss it and fall
	// through to the done-intent restart this fix exists to prevent.
	if first.State != HeartbeatExiting || first.Context != "gt done" || first.Bead != "gt-azmw" {
		t.Fatalf("renewal changed the heartbeat's meaning: state=%q context=%q bead=%q", first.State, first.Context, first.Bead)
	}

	// 2. The stage outlives one write: every tick advances it. Stop is a
	// barrier, so once it returns the last tick's write has landed.
	for i := 1; i <= 3; i++ {
		if err := clk.BlockUntilContext(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
		clk.Advance(interval)
	}
	if err := clk.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	stop()
	stop() // idempotent — a second call must not panic or re-arm anything
	frozen := ReadSessionHeartbeat(townRoot, sessionName)
	if frozen == nil {
		t.Fatal("expected the heartbeat to survive stop")
	}
	if want := first.Timestamp.Add(3 * interval); !frozen.Timestamp.Equal(want) {
		t.Fatalf("after three ticks the heartbeat reads %v, want %v: the keep-alive must renew on every tick", frozen.Timestamp, want)
	}

	// 3. Stop ends the renewal: ticks after it write nothing.
	clk.Advance(10 * interval)
	after := ReadSessionHeartbeat(townRoot, sessionName)
	if after == nil || !after.Timestamp.Equal(frozen.Timestamp) {
		t.Fatalf("keep-alive kept renewing after stop: %v -> %v", frozen.Timestamp, after)
	}
}

// TestKeepRenewing_StopIsABarrier pins the invariant the test above depends
// on: once stop returns, renew can never run again. That is not free — closing
// the done channel only *signals* the goroutine, and a tick already taken out
// of the ticker but still inside the write (mkdir, marshal, write) runs to
// completion regardless, landing a renewal after stop returned (the "kept
// renewing after stop" failure in gt-nyh8, which only surfaced under host
// load because that is what makes the write straddle the read).
//
// Here the write is held open deliberately: a tick starts renew, renew blocks,
// and stop is called while it is in flight. The barrier holds only if stop
// returns after renew does; a signal-only stop returns while renew is still
// blocked.
func TestKeepRenewing_StopIsABarrier(t *testing.T) {
	t.Parallel()
	const interval = time.Second

	for i := 0; i < 50; i++ {
		clk := clockwork.NewFakeClockAt(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
		var mu sync.Mutex
		var events []string
		record := func(e string) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		}
		calls := 0
		inFlight := make(chan struct{})
		release := make(chan struct{})
		stop := keepRenewing(clk, interval, func() {
			calls++
			if calls == 1 {
				return // the synchronous first renewal
			}
			close(inFlight)
			<-release
			record("renewed")
		})

		if err := clk.BlockUntilContext(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
		clk.Advance(interval)
		<-inFlight

		stopped := make(chan struct{})
		go func() {
			stop()
			record("stopped")
			close(stopped)
		}()
		// Give stop the chance to run first: a signal-only stop returns here
		// without waiting on the held write.
		for j := 0; j < 100; j++ {
			runtime.Gosched()
		}
		close(release)
		<-stopped

		mu.Lock()
		got := strings.Join(events, ",")
		mu.Unlock()
		if got != "renewed,stopped" {
			t.Fatalf("iteration %d: events = %s; stop returned with a renewal still in flight", i, got)
		}
	}
}

// TestStartHeartbeatKeepAlive_NoIdentityIsNoop covers the callers that have no
// session to renew (crew/dog sessions, or a run with GT_SESSION unset): the
// helper must return a usable stop and must not invent a heartbeat file.
func TestStartHeartbeatKeepAlive_NoIdentityIsNoop(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	for _, tc := range []struct{ name, townRoot, session string }{
		{"no session", townRoot, ""},
		{"no town root", "", "myr-mycat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := StartExitingHeartbeatKeepAlive(tc.townRoot, tc.session, "gt done", "gt-azmw")
			stop()
			if tc.session != "" {
				if hb := ReadSessionHeartbeat(tc.townRoot, tc.session); hb != nil {
					t.Fatalf("expected no heartbeat to be written by a no-op keep-alive, got %+v", hb)
				}
			}
		})
	}
}

func TestReadSessionHeartbeat_V2AllStates(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	states := []HeartbeatState{HeartbeatWorking, HeartbeatIdle, HeartbeatExiting, HeartbeatStuck}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			session := "gt-test-v2-" + string(state)
			hb := SessionHeartbeat{
				Timestamp: time.Now().UTC(),
				State:     state,
				Context:   "test context",
				Bead:      "gt-test-bead",
			}
			data, err := json.Marshal(hb)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, session+".json"), data, 0644); err != nil {
				t.Fatal(err)
			}

			read := ReadSessionHeartbeat(townRoot, session)
			if read == nil {
				t.Fatal("expected non-nil heartbeat")
			}
			if read.State != state {
				t.Errorf("state = %q, want %q", read.State, state)
			}
			if !read.IsV2() {
				t.Error("expected IsV2()=true")
			}
			if read.EffectiveState() != state {
				t.Errorf("EffectiveState() = %q, want %q", read.EffectiveState(), state)
			}
			if read.Context != "test context" {
				t.Errorf("context = %q, want %q", read.Context, "test context")
			}
			if read.Bead != "gt-test-bead" {
				t.Errorf("bead = %q, want %q", read.Bead, "gt-test-bead")
			}
		})
	}
}

// TestTouchSessionHeartbeat_ReaderNeverSeesTornWrite pins the write as atomic
// (gt-sle0). ReadSessionHeartbeat returns nil on a parse error, and the
// witness reads a nil heartbeat as "no agent-reported state" and falls through
// to the legacy done-intent timeout, so a torn read restarts a polecat that is
// mid gt done. A truncate-then-write lets a reader see an empty or partial
// file; the payload is large enough that the write window is milliseconds
// rather than microseconds, so the reader hits it on nearly every run.
func TestTouchSessionHeartbeat_ReaderNeverSeesTornWrite(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const session = "gt-test-torn"
	bigContext := strings.Repeat("x", 4<<20)

	TouchSessionHeartbeatWithState(townRoot, session, HeartbeatExiting, bigContext, "gt-abc")
	if ReadSessionHeartbeat(townRoot, session) == nil {
		t.Fatal("seed heartbeat unreadable")
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				TouchSessionHeartbeatWithState(townRoot, session, HeartbeatExiting, bigContext, "gt-abc")
			}
		}
	}()

	torn := 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ReadSessionHeartbeat(townRoot, session) == nil {
			torn++
		}
	}
	close(stop)
	wg.Wait()

	if torn > 0 {
		t.Errorf("reader saw %d unreadable heartbeats while the file was being rewritten; the write must be temp-file + rename", torn)
	}

	entries, err := os.ReadDir(heartbeatsDir(townRoot))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("heartbeats dir has %d entries after writes, want just the heartbeat (no leftover temp files)", len(entries))
	}
}
