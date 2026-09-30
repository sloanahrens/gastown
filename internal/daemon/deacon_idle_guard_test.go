package daemon

import (
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
)

// newDeaconHeartbeatDaemon builds a Daemon whose Deacon session is live in a
// fake tmux (its pane runs claude), with the stuck-Deacon restart's respawn
// wired to bring that session back. The restart's pause runs on clk.
func newDeaconHeartbeatDaemon(t *testing.T, townRoot string, stores map[string]beadsdk.Storage) (*Daemon, *fakeTmux, *clockwork.FakeClock) {
	t.Helper()
	clk := newFixedClock()
	tm := newFakeTmux(clk)
	name := session.DeaconSessionName()
	tm.addSession(name, "claude", clk.Now())
	d := newTestDaemonWithStores(t, townRoot, stores)
	d.tmux = tm
	d.clock = clk
	d.notifier = notifyfake.New()
	d.startDeaconFn = func() error {
		if has, _ := tm.HasSession(name); has {
			return deacon.ErrAlreadyRunning
		}
		tm.addSession(name, "claude", clk.Now())
		return nil
	}
	return d, tm, clk
}

// deaconHealthChecks counts the heartbeat nudges delivered to the Deacon.
func deaconHealthChecks(tm *fakeTmux) int {
	n := 0
	for _, sent := range tm.Sent(session.DeaconSessionName()) {
		if strings.HasPrefix(sent, "HEALTH_CHECK:") {
			n++
		}
	}
	return n
}

// seedDeaconSample writes the Deacon's previous liveness sample into its
// intent record, as an earlier heartbeat tick (possibly by an earlier daemon)
// would have: heartbeat cycle `cycle`, the pane as it is now, unchanged since
// changedAgo.
func seedDeaconSample(t *testing.T, d *Daemon, tm *fakeTmux, cycle int64, changedAgo time.Duration) {
	t.Helper()
	name := session.DeaconSessionName()
	pane, _ := tm.CapturePane(name, 200)
	created, _ := tm.GetSessionCreatedTime(name)
	now := d.clk().Now()
	if _, err := intent.Update(d.config.TownRoot, supervisor.IntentSeat(deaconSeat), func(r *intent.Record) error {
		r.Progress = &intent.Progress{
			SessionCreated: created,
			PaneHash:       tmux.PaneProgressSignature(pane, tmux.DefaultReadyPromptPrefix),
			HasHeartbeat:   true,
			HeartbeatCycle: cycle,
			SampledAt:      now.Add(-time.Minute),
			ChangedAt:      now.Add(-changedAgo),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

var activeWork = map[string]beadsdk.Storage{
	"hq": &searchStorage{results: map[string][]*beadsdk.Issue{"in_progress": {{ID: "sc-abc"}}}},
}

// TestCheckDeaconHeartbeat_ProgressTiers: quiet 5-20m with work in flight is
// nudged, stalled 20m is restarted through the supervisor, and any change of
// the heartbeat cycle is progress (gt-t3cw). The file's timestamp is fresh
// in every case: the poller refreshes it, so it dates nothing.
func TestCheckDeaconHeartbeat_ProgressTiers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		seedCycle  int64
		cycle      int64
		quiet      time.Duration
		wantLogs   []string
		wantNoLogs []string
		wantNudge  bool
	}{
		{
			name: "cycle static 6m - nudge", seedCycle: 42, cycle: 42, quiet: 6 * time.Minute,
			wantLogs: []string{"Deacon quiet for 6m", "nudging session"}, wantNoLogs: []string{"STUCK DEACON"}, wantNudge: true,
		},
		{
			name: "cycle static 21m - restart", seedCycle: 42, cycle: 42, quiet: 21 * time.Minute,
			wantLogs: []string{"STUCK DEACON: no progress for 21m", "Deacon restarted: no progress for 21m"}, wantNoLogs: []string{"nudging session"},
		},
		{
			name: "cycle advanced - healthy", seedCycle: 42, cycle: 43, quiet: 30 * time.Minute,
			wantNoLogs: []string{"quiet for", "nudging session", "STUCK DEACON"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeDeaconHeartbeatWithCycle(t, townRoot, 30*time.Second, tc.cycle)
			d, tm, _ := newDeaconHeartbeatDaemon(t, townRoot, activeWork)
			seedDeaconSample(t, d, tm, tc.seedCycle, tc.quiet)
			logBuf := &strings.Builder{}
			d.logger = log.New(logBuf, "", 0)

			d.checkDeaconHeartbeat()

			out := logBuf.String()
			for _, w := range tc.wantLogs {
				if !strings.Contains(out, w) {
					t.Errorf("log missing %q\nlog:\n%s", w, out)
				}
			}
			for _, w := range tc.wantNoLogs {
				if strings.Contains(out, w) {
					t.Errorf("log contains %q\nlog:\n%s", w, out)
				}
			}
			if got := deaconHealthChecks(tm) == 1; got != tc.wantNudge {
				t.Errorf("nudge delivered = %v, want %v", got, tc.wantNudge)
			}
		})
	}
}

// TestCheckDeaconHeartbeat_FirstSampleIsABaseline: with no previous sample
// there is nothing to compare, so nothing is acted on — the persisted
// version of the old two-tick debounce.
func TestCheckDeaconHeartbeat_FirstSampleIsABaseline(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDeaconHeartbeatWithCycle(t, townRoot, time.Hour, 42)
	d, tm, _ := newDeaconHeartbeatDaemon(t, townRoot, activeWork)
	logBuf := &strings.Builder{}
	d.logger = log.New(logBuf, "", 0)

	d.checkDeaconHeartbeat()

	if out := logBuf.String(); out != "" || deaconHealthChecks(tm) != 0 {
		t.Fatalf("a first sample was acted on\nlog:\n%s", out)
	}
	rec, _ := intent.Read(townRoot, supervisor.IntentSeat(deaconSeat))
	if rec.Progress == nil || rec.Progress.HeartbeatCycle != 42 {
		t.Fatalf("the baseline sample was not persisted: %+v", rec.Progress)
	}
}

// TestCheckDeaconHeartbeat_StallSurvivesDaemonRestarts is G1-04: a daemon
// restarted more often than the stall window used to re-baseline an
// in-memory tracker every time and never restart a wedged Deacon. The sample
// is on disk, so a fresh Daemon value 21 minutes later sees the stall.
func TestCheckDeaconHeartbeat_StallSurvivesDaemonRestarts(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDeaconHeartbeatWithCycle(t, townRoot, 30*time.Second, 42)

	first, tm, clk := newDeaconHeartbeatDaemon(t, townRoot, activeWork)
	first.logger = log.New(&strings.Builder{}, "", 0)
	first.checkDeaconHeartbeat() // baseline

	for i := 0; i < 7; i++ { // a new daemon every 3 minutes
		clk.Advance(3 * time.Minute)
		d := newTestDaemonWithStores(t, townRoot, activeWork)
		d.tmux, d.clock, d.notifier = tm, clk, notifyfake.New()
		d.startDeaconFn = first.startDeaconFn
		logBuf := &strings.Builder{}
		d.logger = log.New(logBuf, "", 0)
		d.checkDeaconHeartbeat()
		if i == 6 && !strings.Contains(logBuf.String(), "STUCK DEACON: no progress for 21m") {
			t.Fatalf("a Deacon wedged 21m across seven daemon restarts was not restarted\nlog:\n%s", logBuf)
		}
	}
}

// TestCheckDeaconHeartbeat_IdleGuard: the nudge tier is suppressed when no
// work is in flight (the Deacon wakes from its await-signal on its own), and
// fires conservatively when the work state cannot be read.
func TestCheckDeaconHeartbeat_IdleGuard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stores    map[string]beadsdk.Storage
		wantNudge bool
		wantSkip  bool
	}{
		{"no work", map[string]beadsdk.Storage{"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}}}, false, true},
		{"in_progress work", activeWork, true, false},
		{"hooked patrol wisp only", map[string]beadsdk.Storage{"hq": &searchStorage{results: map[string][]*beadsdk.Issue{"hooked": {{ID: "hq-wisp-34zi"}}}}}, false, true},
		{"store error", map[string]beadsdk.Storage{"hq": &searchStorage{err: fmt.Errorf("db offline")}}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeDeaconHeartbeatWithCycle(t, townRoot, 30*time.Second, 42)
			d, tm, _ := newDeaconHeartbeatDaemon(t, townRoot, tc.stores)
			seedDeaconSample(t, d, tm, 42, 10*time.Minute)
			logBuf := &strings.Builder{}
			d.logger = log.New(logBuf, "", 0)

			d.checkDeaconHeartbeat()

			if got := deaconHealthChecks(tm) == 1; got != tc.wantNudge {
				t.Errorf("nudge delivered = %v, want %v\nlog:\n%s", got, tc.wantNudge, logBuf)
			}
			if got := strings.Contains(logBuf.String(), "nudge skipped"); got != tc.wantSkip {
				t.Errorf("idle-guard log = %v, want %v\nlog:\n%s", got, tc.wantSkip, logBuf)
			}
		})
	}
}

// TestDeaconRestartBudget: the fourth Deacon restart in an hour is refused,
// the seat is frozen and one escalation goes to the operator. This replaces
// the deacon-only RestartTracker (G1-15).
func TestDeaconRestartBudget(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDeaconHeartbeatWithCycle(t, townRoot, 30*time.Second, 42)
	d, tm, clk := newDeaconHeartbeatDaemon(t, townRoot, activeWork)
	notes := notifyfake.New()
	d.notifier = notes
	logBuf := &strings.Builder{}
	d.logger = log.New(logBuf, "", 0)

	for i := 0; i < 4; i++ {
		seedDeaconSample(t, d, tm, 42, 21*time.Minute)
		d.checkDeaconHeartbeat()
		clk.Advance(5 * time.Minute)
	}

	if n := strings.Count(logBuf.String(), "Deacon restarted:"); n != 3 {
		t.Fatalf("restarts = %d, want 3\nlog:\n%s", n, logBuf)
	}
	if !strings.Contains(logBuf.String(), "restart budget exhausted") {
		t.Fatalf("the fourth restart was not refused on budget\nlog:\n%s", logBuf)
	}
	if n := len(notes.Escalations()); n != 1 {
		t.Fatalf("escalations = %d, want 1", n)
	}
	rec, _ := intent.Read(townRoot, supervisor.IntentSeat(deaconSeat))
	if !rec.Frozen {
		t.Fatalf("deacon seat not frozen after an exhausted budget: %+v", rec)
	}
}

// TestEnsureDeaconRunning_UnknownIsNotDead: a failed tmux query starts
// nothing (G1-09).
func TestEnsureDeaconRunning_UnknownIsNotDead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d, tm, _ := newDeaconHeartbeatDaemon(t, townRoot, nil)
	tm.mu.Lock()
	tm.hasErr = fmt.Errorf("tmux: server timed out")
	tm.mu.Unlock()
	started := false
	d.startDeaconFn = func() error { started = true; return nil }
	logBuf := &strings.Builder{}
	d.logger = log.New(logBuf, "", 0)

	d.ensureDeaconRunning()

	if started || !strings.Contains(logBuf.String(), "liveness unknown") {
		t.Fatalf("started=%v on an unknown liveness answer\nlog:\n%s", started, logBuf)
	}
}

// writeDeaconHeartbeatWithCycle writes a heartbeat with a controlled age and
// cycle.
func writeDeaconHeartbeatWithCycle(t *testing.T, townRoot string, age time.Duration, cycle int64) {
	t.Helper()
	if err := deacon.WriteHeartbeat(townRoot, &deacon.Heartbeat{Timestamp: time.Now().Add(-age), Cycle: cycle}); err != nil {
		t.Fatalf("writeDeaconHeartbeatWithCycle: %v", err)
	}
}
