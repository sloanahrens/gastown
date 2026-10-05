package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/session"
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

// KillAll is the operator's override: it kills through a town e-stop, a rig
// e-stop and a parked seat, logs verb kill-all with the actor and leaves the
// park in place (gt-4k3fj.4).
func TestKillAllBypassesEstopAndHold(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_ = estop.Activate(h.town, estop.TriggerManual, "x")
	_ = estop.ActivateRig(h.town, "gastown", estop.TriggerManual, "x")
	h.pause(t)
	if err := h.sup().KillAll(flint, "operator kill-all", "gt kill-all/overseer"); err != nil {
		t.Fatalf("KillAll = %v, want the kill", err)
	}
	if got := h.tmux.kills(); len(got) != 1 || got[0] != flint.SessionName() {
		t.Fatalf("kills = %v, want [%s]", got, flint.SessionName())
	}
	rec, err := intent.Read(h.town, IntentSeat(flint))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Held() {
		t.Errorf("record desired=%q: kill-all dropped the seat's park", rec.Desired)
	}
	lines := h.actions(t)
	if len(lines) != 1 || lines[0].Verb != "kill-all" || lines[0].Actor != "gt kill-all/overseer" || lines[0].Outcome != "done" {
		t.Fatalf("action log = %+v, want one done kill-all line naming the actor", lines)
	}
	// Plain Kill still refuses under the same e-stop.
	if err := h.sup().Kill(flint, "idle", "daemon"); !errors.Is(err, ErrRefused) {
		t.Errorf("Kill after KillAll = %v, want a refusal", err)
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

// gt-obbx2: gt done marks the seat submitted. A restart would raise a session
// on finished work, so it is refused and spends no budget; Kill still works so
// cleanup after the landing is unaffected; new work ends the state.
func TestSubmittedSeatRefusesRestartNotKill(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := intent.MarkSubmitted(h.town, IntentSeat(flint), "gt-abc", "gt done", h.now); err != nil {
		t.Fatal(err)
	}
	s := h.sup()

	err := s.Restart(flint, "dead agent", "witness")
	if !errors.Is(err, ErrRefused) || !errors.Is(err, ErrSubmitted) {
		t.Fatalf("Restart on a submitted seat = %v, want ErrSubmitted", err)
	}
	if !strings.Contains(err.Error(), "gt-abc") {
		t.Errorf("refusal %q does not name the submitted bead", err)
	}
	if len(h.restarts) != 0 {
		t.Fatalf("a submitted seat was restarted: %v", h.restarts)
	}
	if rec, _ := intent.Read(h.town, IntentSeat(flint)); len(rec.Restarts) != 0 || !rec.Submitted() {
		t.Fatalf("record = %+v, want submitted with no restart charged", rec)
	}

	if err := s.Kill(flint, "cleanup after landing", "witness"); err != nil {
		t.Fatalf("Kill on a submitted seat = %v, want allowed", err)
	}

	// Kill records desired=stop, which is what an idle seat looks like: the
	// next dispatch restarts it normally.
	if err := s.Restart(flint, "dispatch", "gt sling"); err != nil {
		t.Fatalf("Restart after the seat stopped = %v, want allowed", err)
	}
}

func TestClearSubmittedLetsARestartThrough(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seat := IntentSeat(flint)
	if err := intent.MarkSubmitted(h.town, seat, "gt-abc", "gt done", h.now); err != nil {
		t.Fatal(err)
	}
	if err := intent.ClearSubmitted(h.town, seat, "gt sling", h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.sup().Restart(flint, "dead agent", "witness"); err != nil {
		t.Fatalf("Restart after new work cleared the submitted state = %v", err)
	}
	if len(h.restarts) != 1 {
		t.Fatalf("restarts = %v, want one", h.restarts)
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
	seat, err := SeatForSession(nil, "hq-overseer")
	if err != nil || seat.Role != "overseer" || seat.SessionName() != "hq-overseer" {
		t.Fatalf("SeatForSession(hq-overseer) = %+v, %v", seat, err)
	}
	// The mayor is retired with its role: a leftover hq-mayor name is not a
	// seat, so it is refused rather than silently kept alive (gt-rwp7z).
	if _, err := SeatForSession(nil, "hq-mayor"); err == nil {
		t.Fatal("SeatForSession(hq-mayor) = nil error, want the retired session refused")
	}
	reg := session.NewPrefixRegistry()
	reg.Register("ga", "gastown")
	seat, err = SeatForSession(reg, "ga-crew-max")
	if err != nil || seat.Rig != "gastown" || seat.SessionName() != "ga-crew-max" {
		t.Fatalf("SeatForSession(ga-crew-max) = %+v, %v", seat, err)
	}
	if IntentSeat(SeatFor("", "crew", "max")).Path("/t") != "/t/.runtime/agents/crew.max.json" {
		t.Fatalf("named seat path = %s", IntentSeat(SeatFor("", "crew", "max")).Path("/t"))
	}
}

// TestKillStrayHonorsRigEstop: a stray whose prefix the registry maps to a
// rig is refused while that rig is e-stopped.
func TestKillStrayHonorsRigEstop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_ = estop.ActivateRig(h.town, "gastown", estop.TriggerManual, "x")
	reg := session.NewPrefixRegistry()
	reg.Register("ga", "gastown")
	s := New(Options{TownRoot: h.town, Tmux: h.tmux, Prefixes: reg, Now: func() time.Time { return h.now }})
	if err := s.KillStray("ga-ghost", "ghost", "daemon"); !errors.Is(err, ErrEstop) {
		t.Fatalf("KillStray = %v, want ErrEstop", err)
	}
	if len(h.tmux.kills()) != 0 {
		t.Fatalf("kills = %v, want none", h.tmux.kills())
	}
}

// TestDeclinedRestartIsNotCharged: an executor that started nothing (a fork
// rig, a safety stop, a session someone else already raised) wraps
// ErrDeclined, and the attempt does not spend the seat's budget.
func TestDeclinedRestartIsNotCharged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	s := New(Options{TownRoot: h.town, Tmux: h.tmux, Now: func() time.Time { return h.now },
		Restart: func(Seat) error { return fmt.Errorf("%w: refinery is fork-backed", ErrDeclined) }})
	for i := 0; i < 5; i++ {
		if err := s.Restart(flint, "dead", "daemon"); !errors.Is(err, ErrDeclined) || errors.Is(err, ErrRefused) {
			t.Fatalf("attempt %d = %v, want ErrDeclined", i+1, err)
		}
	}
	rec, _ := intent.Read(h.town, IntentSeat(flint))
	if len(rec.Restarts) != 0 || rec.Held() || rec.LastAction.Outcome != "declined" {
		t.Fatalf("record after declined restarts = %+v, want no budget spent", rec)
	}
}

// A caller that keeps asking for a refused action (the daemon, every
// heartbeat, for a parked dead witness) writes one action-log line per
// window, not one per tick. The logger still sees every refusal.
func TestRepeatedRefusalIsLoggedOncePerWindow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pause(t)
	for i := 0; i < 5; i++ {
		_ = h.sup().Restart(flint, "witness dead", "daemon/ensure-witness")
		h.now = h.now.Add(3 * time.Minute)
	}
	if n := len(h.actions(t)); n != 1 {
		t.Fatalf("action lines after 5 identical refusals = %d, want 1", n)
	}
	h.now = h.now.Add(time.Hour)
	_ = h.sup().Restart(flint, "witness dead", "daemon/ensure-witness")
	if n := len(h.actions(t)); n != 2 {
		t.Fatalf("action lines after the window = %d, want 2", n)
	}
	// A different actor is a different request and is logged.
	_ = h.sup().Restart(flint, "witness dead", "gt doctor fix")
	if n := len(h.actions(t)); n != 3 {
		t.Fatalf("action lines after another actor asked = %d, want 3", n)
	}
}

// Kill re-checks the hold under the seat's lock, the same lock a pause takes,
// so a pause cannot land between the check and the kill.
func TestKillChecksTheHoldUnderTheSeatLock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var heldDuringKill bool
	lockPath := filepath.Join(h.town, ".runtime", "locks", "intent", "gastown", "polecat.flint.lock")
	k := killerFunc(func(name string) error {
		// The seat lock a pause would take is held while the session dies.
		fl := flock.New(lockPath)
		got, err := fl.TryLock()
		if got {
			_ = fl.Unlock()
		}
		heldDuringKill = err == nil && !got
		return nil
	})
	s := New(Options{TownRoot: h.town, Tmux: k, Now: func() time.Time { return h.now }})
	if err := s.Kill(flint, "idle", "daemon"); err != nil {
		t.Fatal(err)
	}
	if !heldDuringKill {
		t.Fatal("a pause completed while Kill was killing: the hold check is not under the seat lock")
	}
}

type killerFunc func(name string) error

func (f killerFunc) KillSessionWithProcesses(name string) error { return f(name) }

// TestDecidesWithoutTheStore: every guard is a file read, so Kill and Restart
// decide and act when Dolt is down. The package runs no process and imports
// nothing that reaches the store; the agent-bead mirror is a callback its
// host supplies, and its failure never undoes an action (see
// TestKillRecordsStopAndMirrorFailureDoesNotUndoIt).
func TestDecidesWithoutTheStore(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "supervisor.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "os/exec" || strings.Contains(path, "internal/beads") || strings.Contains(path, "internal/doltserver") {
			t.Errorf("supervisor.go imports %s", path)
		}
	}
}

// Stop and Cleanup are the operator's stop verbs and gt's own rollback
// kill (gt-4k3fj.4.1): neither an e-stop nor a park refuses them, and each
// is logged under its verb with the actor and reason.
func TestStopAndCleanupBypassEstopAndHold(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"stop", "cleanup"} {
		h := newHarness(t)
		_ = estop.Activate(h.town, estop.TriggerManual, "x")
		h.pause(t)
		s := h.sup()
		var err error
		if verb == "stop" {
			err = s.Stop(flint, "gt down", "gt down/overseer")
		} else {
			err = s.Cleanup(flint, "spawn rollback", "gt sling/overseer")
		}
		if err != nil {
			t.Fatalf("%s = %v, want the kill", verb, err)
		}
		if got := h.tmux.kills(); len(got) != 1 || got[0] != flint.SessionName() {
			t.Fatalf("%s kills = %v", verb, got)
		}
		lines := h.actions(t)
		if len(lines) != 1 || lines[0].Verb != verb || lines[0].Outcome != "done" || lines[0].Actor == "" || lines[0].Reason == "" {
			t.Fatalf("%s action log = %+v, want one done line with actor and reason", verb, lines)
		}
	}
}

// StopSession kills a name that names no seat as a logged stray, through
// an e-stop.
func TestStopSessionKillsStrayThroughEstop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_ = estop.Activate(h.town, estop.TriggerManual, "x")
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	s := New(Options{TownRoot: h.town, Tmux: h.tmux, Prefixes: reg, Logf: func(string, ...any) {}, Now: func() time.Time { return h.now }})
	if err := s.StopSession("zz-not-a-seat-name-at-all", "rig remove --force", "gt rig remove/overseer"); err != nil {
		t.Fatal(err)
	}
	if err := s.StopSession(flint.SessionName(), "rig remove --force", "gt rig remove/overseer"); err != nil {
		t.Fatal(err)
	}
	lines := h.actions(t)
	if len(lines) != 2 || lines[0].Verb != "stop-stray" || lines[1].Verb != "stop" || lines[1].Seat == "" {
		t.Fatalf("action log = %+v, want stop-stray then stop on the seat", lines)
	}
}

// Respawn is refused by an e-stop, a park and a gt down in progress, and
// never runs its executor then.
func TestRespawnRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		setup func(t *testing.T, h *harness)
		want  error
	}{
		"town estop": {func(_ *testing.T, h *harness) { _ = estop.Activate(h.town, estop.TriggerManual, "x") }, ErrEstop},
		"rig estop": {func(_ *testing.T, h *harness) {
			_ = estop.ActivateRig(h.town, "gastown", estop.TriggerManual, "x")
		}, ErrEstop},
		"park": {func(t *testing.T, h *harness) { h.pause(t) }, ErrPaused},
		"shutdown": {func(t *testing.T, h *harness) {
			lockPath := filepath.Join(h.town, "daemon", "shutdown.lock")
			_ = os.MkdirAll(filepath.Dir(lockPath), 0o755)
			fl := flock.New(lockPath)
			if ok, err := fl.TryLock(); !ok || err != nil {
				t.Fatalf("taking the shutdown lock: %v %v", ok, err)
			}
			t.Cleanup(func() { _ = fl.Unlock() })
		}, ErrShutdown},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			tc.setup(t, h)
			ran := false
			err := h.sup().Respawn(flint, "crew restart", "gt crew restart/overseer", func() error { ran = true; return nil })
			if !errors.Is(err, ErrRefused) || !errors.Is(err, tc.want) {
				t.Fatalf("Respawn = %v, want %v", err, tc.want)
			}
			if ran {
				t.Fatal("Respawn ran its executor after refusing")
			}
		})
	}
}

// Respawn records the new incarnation and a started line before it runs
// (a self-handoff never returns), spends no budget, and logs a failed run.
func TestRespawnRecordsBeforeRunAndSpendsNoBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	s := h.sup()
	for i := 0; i < DefaultBudget+2; i++ {
		err := s.Respawn(flint, "handoff", "gt handoff/gastown/crew/max", func() error {
			if lines := h.actions(t); len(lines) == 0 || lines[len(lines)-1].Outcome != "started" {
				t.Errorf("no started line before run: %+v", lines)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("respawn %d = %v", i, err)
		}
	}
	rec, err := intent.Read(h.town, IntentSeat(flint))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Desired != intent.DesiredRun || rec.IncarnationID == "" || len(rec.Restarts) != 0 || rec.Frozen {
		t.Fatalf("record = %+v, want desired=run, an incarnation and no budget spent", rec)
	}
	if err := s.Respawn(flint, "handoff", "gt handoff", func() error { return errors.New("pane gone") }); err == nil {
		t.Fatal("Respawn with a failing run = nil")
	}
	lines := h.actions(t)
	if last := lines[len(lines)-1]; last.Verb != "respawn" || last.Outcome != "failed" || last.Detail != "pane gone" {
		t.Fatalf("last action = %+v, want a failed respawn", last)
	}
}
