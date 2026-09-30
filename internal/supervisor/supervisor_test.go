package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
)

// fakeTmux records kills over a set of live sessions.
type fakeTmux struct {
	mu       sync.Mutex
	sessions map[string]bool
	killed   []string
}

func newFakeTmux(sessions ...string) *fakeTmux {
	f := &fakeTmux{sessions: map[string]bool{}}
	for _, s := range sessions {
		f.sessions[s] = true
	}
	return f
}

func (f *fakeTmux) HasSession(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[name], nil
}

func (f *fakeTmux) KillSessionWithProcesses(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, name)
	delete(f.sessions, name)
	return nil
}

func (f *fakeTmux) kills() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.killed...)
}

type harness struct {
	town      string
	tmux      *fakeTmux
	now       time.Time
	restarts  []string
	escalated []string
	mirrored  []intent.Record
	mirrorErr error
}

var flint = SeatFor("gastown", "polecat", "flint")

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{
		town: t.TempDir(),
		tmux: newFakeTmux(flint.SessionName()),
		now:  time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC),
	}
}

func (h *harness) sup() *Supervisor {
	return New(Options{
		TownRoot: h.town,
		Tmux:     h.tmux,
		Restart: func(seat Seat) error {
			h.restarts = append(h.restarts, seat.SessionName())
			return nil
		},
		Mirror: func(_ Seat, rec intent.Record) error {
			h.mirrored = append(h.mirrored, rec)
			return h.mirrorErr
		},
		Escalate: func(_ Seat, line string) { h.escalated = append(h.escalated, line) },
		Logf:     func(string, ...any) {},
		Now:      func() time.Time { return h.now },
	})
}

// actions returns the action log lines.
func (h *harness) actions(t *testing.T) []ActionLine {
	t.Helper()
	f, err := os.Open(ActionLogPath(h.town))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer f.Close()
	var out []ActionLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l ActionLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("action log line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

func (h *harness) pause(t *testing.T) {
	t.Helper()
	if _, err := intent.Update(h.town, IntentSeat(flint), func(r *intent.Record) error {
		r.Desired = intent.DesiredPark
		r.Reason = "operator inspecting"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPausedSeatRefusesKillAndRestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pause(t)
	s := h.sup()

	if err := s.Kill(flint, "idle", "daemon/idle-reaper"); !errors.Is(err, ErrRefused) || !errors.Is(err, ErrPaused) {
		t.Fatalf("Kill on a paused seat = %v, want ErrPaused", err)
	}
	if err := s.Restart(flint, "dead agent", "witness"); !errors.Is(err, ErrPaused) {
		t.Fatalf("Restart on a paused seat = %v, want ErrPaused", err)
	}
	if len(h.tmux.kills()) != 0 || len(h.restarts) != 0 {
		t.Fatalf("a paused seat was touched: kills=%v restarts=%v", h.tmux.kills(), h.restarts)
	}
	lines := h.actions(t)
	if len(lines) != 2 || lines[0].Actor != "daemon/idle-reaper" || lines[0].Outcome != "refused" || !strings.Contains(lines[0].Detail, "operator inspecting") {
		t.Fatalf("action log = %+v, want two refusals naming actor and hold", lines)
	}
}

func TestEstopRefusesKillAndRestart(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"town", "rig"} {
		h := newHarness(t)
		if which == "town" {
			_ = estop.Activate(h.town, estop.TriggerManual, "x")
		} else {
			_ = estop.ActivateRig(h.town, "gastown", estop.TriggerManual, "x")
		}
		s := h.sup()
		if err := s.Kill(flint, "idle", "daemon"); !errors.Is(err, ErrEstop) {
			t.Errorf("%s estop: Kill = %v, want ErrEstop", which, err)
		}
		if err := s.Restart(flint, "dead", "daemon"); !errors.Is(err, ErrEstop) {
			t.Errorf("%s estop: Restart = %v, want ErrEstop", which, err)
		}
		if which == "town" {
			if err := s.KillStray("gt-witness", "ghost", "daemon"); !errors.Is(err, ErrEstop) {
				t.Errorf("town estop: KillStray = %v, want ErrEstop", err)
			}
		}
		if len(h.tmux.kills()) != 0 || len(h.restarts) != 0 {
			t.Errorf("%s estop: kills=%v restarts=%v, want none", which, h.tmux.kills(), h.restarts)
		}
	}
}

func TestShutdownRefusesRestartNotKill(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	lockPath := filepath.Join(h.town, "daemon", "shutdown.lock")
	_ = os.MkdirAll(filepath.Dir(lockPath), 0o755)
	fl := flock.New(lockPath)
	if ok, err := fl.TryLock(); !ok || err != nil {
		t.Fatalf("taking the shutdown lock: %v %v", ok, err)
	}
	defer func() { _ = fl.Unlock() }()
	if !ShutdownInProgress(h.town) {
		t.Fatal("ShutdownInProgress = false while the lock is held")
	}
	s := h.sup()
	if err := s.Restart(flint, "dead", "daemon"); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Restart during shutdown = %v, want ErrShutdown", err)
	}
	if err := s.Kill(flint, "shutdown", "gt down"); err != nil {
		t.Fatalf("Kill during shutdown = %v, want allowed", err)
	}
}

func TestFourthRestartInAnHourFreezesTheSeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		if err := h.sup().Restart(flint, "agent dead", "witness"); err != nil {
			t.Fatalf("restart %d: %v", i+1, err)
		}
		h.now = h.now.Add(10 * time.Minute)
	}
	// A new Supervisor each time: the budget lives in the file, not in memory.
	err := h.sup().Restart(flint, "agent dead", "witness")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("fourth restart = %v, want ErrBudgetExhausted", err)
	}
	if len(h.restarts) != 3 {
		t.Fatalf("restarts executed = %d, want 3", len(h.restarts))
	}
	rec, _ := intent.Read(h.town, IntentSeat(flint))
	if !rec.Frozen || !rec.Held() || rec.PausedBy != "supervisor" || !strings.Contains(rec.Reason, "budget") {
		t.Fatalf("after exhaustion: %+v, want frozen, held, paused by supervisor with a budget reason", rec)
	}
	if len(h.escalated) != 1 || !strings.Contains(h.escalated[0], "gastown") {
		t.Fatalf("escalations = %v, want exactly one line naming the seat", h.escalated)
	}
	lines := h.actions(t)
	last := lines[len(lines)-1]
	if last.Outcome != "refused" || last.Actor != "witness" || last.Verb != "restart" {
		t.Fatalf("last action = %+v, want a refused restart by witness", last)
	}

	// Frozen now: the next attempt is refused as held, with no second escalation.
	if err := h.sup().Restart(flint, "agent dead", "witness"); !errors.Is(err, ErrFrozen) {
		t.Fatalf("restart of a frozen seat = %v, want ErrFrozen", err)
	}
	if len(h.escalated) != 1 {
		t.Fatalf("escalations = %d, want still 1", len(h.escalated))
	}
}

func TestRestartsOlderThanTheWindowDoNotCount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := 0; i < 6; i++ {
		if err := h.sup().Restart(flint, "dead", "daemon"); err != nil {
			t.Fatalf("restart %d at %v: %v", i+1, h.now, err)
		}
		h.now = h.now.Add(25 * time.Minute) // at most 3 inside any hour
	}
	rec, _ := intent.Read(h.town, IntentSeat(flint))
	if len(rec.Restarts) > 3 {
		t.Fatalf("stored restarts = %d, want pruned to the window", len(rec.Restarts))
	}
}

func TestUnreadableIntentRefuses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	path := IntentSeat(flint).Path(h.town)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("{broken"), 0o644)
	if err := h.sup().Kill(flint, "idle", "daemon"); !errors.Is(err, ErrIntentUnreadable) {
		t.Fatalf("Kill with a broken record = %v, want ErrIntentUnreadable", err)
	}
	if err := h.sup().Restart(flint, "dead", "daemon"); !errors.Is(err, ErrIntentUnreadable) {
		t.Fatalf("Restart with a broken record = %v, want ErrIntentUnreadable", err)
	}
	if len(h.tmux.kills()) != 0 || len(h.restarts) != 0 {
		t.Fatal("a seat with a broken record was touched")
	}
}

func TestKillRecordsStopAndMirrorFailureDoesNotUndoIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.mirrorErr = errors.New("dolt: connection refused")
	if err := h.sup().Kill(flint, "idle 20m", "daemon/idle-reaper"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if got := h.tmux.kills(); len(got) != 1 || got[0] != flint.SessionName() {
		t.Fatalf("kills = %v", got)
	}
	rec, _ := intent.Read(h.town, IntentSeat(flint))
	if rec.EffectiveDesired() != intent.DesiredStop || rec.LastAction == nil || rec.LastAction.Actor != "daemon/idle-reaper" {
		t.Fatalf("record after Kill = %+v", rec)
	}
	if len(h.mirrored) != 1 {
		t.Fatalf("mirror calls = %d, want 1", len(h.mirrored))
	}
	// A second Kill of a session that is already gone still succeeds.
	if err := h.sup().Kill(flint, "idle", "daemon"); err != nil {
		t.Fatalf("Kill of a missing session: %v", err)
	}
}

func TestRestartWritesANewIncarnation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.sup().Restart(flint, "dead", "daemon"); err != nil {
		t.Fatal(err)
	}
	first, _ := intent.Read(h.town, IntentSeat(flint))
	h.now = h.now.Add(time.Minute)
	if err := h.sup().Restart(flint, "dead", "daemon"); err != nil {
		t.Fatal(err)
	}
	second, _ := intent.Read(h.town, IntentSeat(flint))
	if first.IncarnationID == "" || first.IncarnationID == second.IncarnationID || second.EffectiveDesired() != intent.DesiredRun {
		t.Fatalf("incarnations %q then %q, desired %q", first.IncarnationID, second.IncarnationID, second.Desired)
	}
}

func TestRestartFailureIsReportedAndCounted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	s := New(Options{TownRoot: h.town, Tmux: h.tmux, Now: func() time.Time { return h.now },
		Restart: func(Seat) error { return errors.New("claude exited: 401") }})
	if err := s.Restart(flint, "dead", "daemon"); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("failed restart = %v, want the executor error", err)
	}
	rec, _ := intent.Read(h.town, IntentSeat(flint))
	if len(rec.Restarts) != 1 || rec.LastAction.Outcome != "failed" {
		t.Fatalf("record = %+v, want the attempt counted and marked failed", rec)
	}
}

func TestNoStarterIsAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	s := New(Options{TownRoot: h.town, Tmux: h.tmux})
	if err := s.Restart(flint, "dead", "daemon"); !errors.Is(err, ErrNoStarter) {
		t.Fatalf("Restart without a starter = %v, want ErrNoStarter", err)
	}
}

// TestDecidesWithDoltDown: with a bd on PATH that records every call and
// fails, Kill and Restart still decide and act, and never invoke it.
func TestDecidesWithDoltDown(t *testing.T) {
	h := newHarness(t)
	bin := t.TempDir()
	called := filepath.Join(bin, "called")
	script := "#!/bin/sh\necho \"$@\" >> " + called + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	s := New(Options{TownRoot: h.town, Tmux: h.tmux, Now: func() time.Time { return h.now },
		Restart: func(Seat) error { return nil }})
	if err := s.Restart(flint, "dead", "daemon"); err != nil {
		t.Fatalf("Restart with Dolt down: %v", err)
	}
	if err := s.Kill(flint, "idle", "daemon"); err != nil {
		t.Fatalf("Kill with Dolt down: %v", err)
	}
	if _, err := os.Stat(called); err == nil {
		data, _ := os.ReadFile(called)
		t.Fatalf("bd was invoked: %s", data)
	}
}

func TestClearHoldFreesAFrozenSeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := 0; i < 4; i++ {
		_ = h.sup().Restart(flint, "dead", "daemon")
	}
	if err := ClearHold(h.town, flint, "gt daemon clear-backoff"); err != nil {
		t.Fatal(err)
	}
	rec, _ := intent.Read(h.town, IntentSeat(flint))
	if rec.Held() || len(rec.Restarts) != 0 {
		t.Fatalf("after ClearHold: %+v, want free with an empty budget", rec)
	}
	if err := h.sup().Restart(flint, "dead", "daemon"); err != nil {
		t.Fatalf("restart after ClearHold: %v", err)
	}
}

func TestKillStrayKillsAndLogs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.tmux.sessions["gt-witness"] = true
	if err := h.sup().KillStray("gt-witness", "default-prefix ghost", "daemon/ghosts"); err != nil {
		t.Fatal(err)
	}
	if got := h.tmux.kills(); len(got) != 1 || got[0] != "gt-witness" {
		t.Fatalf("kills = %v", got)
	}
	lines := h.actions(t)
	if len(lines) != 1 || lines[0].Verb != "kill-stray" || lines[0].Session != "gt-witness" || lines[0].Outcome != "done" {
		t.Fatalf("action log = %+v", lines)
	}
}

func TestSeatForSession(t *testing.T) {
	t.Parallel()
	seat, err := SeatForSession("hq-deacon")
	if err != nil || seat.Role != "deacon" || seat.SessionName() != "hq-deacon" {
		t.Fatalf("SeatForSession(hq-deacon) = %+v, %v", seat, err)
	}
	if IntentSeat(SeatFor("", "deacon", "boot")).Path("/t") != "/t/.runtime/agents/deacon.boot.json" {
		t.Fatalf("boot seat path = %s", IntentSeat(SeatFor("", "deacon", "boot")).Path("/t"))
	}
}
