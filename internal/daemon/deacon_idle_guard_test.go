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
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/session"
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

// TestCheckDeaconHeartbeat_IdleGuard verifies that the nudge is suppressed when
// the Deacon heartbeat is stale but no active work is in flight (idle guard).
func TestCheckDeaconHeartbeat_IdleGuard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		heartbeatAge     time.Duration
		stores           map[string]beadsdk.Storage
		wantNudgeLog     bool
		wantIdleGuardLog bool
		desc             string
	}{
		{
			name:         "idle: stale heartbeat, no work — nudge suppressed",
			heartbeatAge: 10 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			wantNudgeLog:     false,
			wantIdleGuardLog: true,
			desc:             "Idle guard must suppress nudge when no work is in flight",
		},
		{
			name:         "active work: stale heartbeat, in_progress bead — nudge sent",
			heartbeatAge: 10 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
					"in_progress": {{ID: "sc-abc"}},
				}},
			},
			wantNudgeLog:     true,
			wantIdleGuardLog: false,
			desc:             "Nudge must fire when in_progress work exists",
		},
		{
			name:         "hooked only: stale heartbeat, patrol wisp — nudge suppressed",
			heartbeatAge: 10 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
					"hooked": {{ID: "hq-wisp-34zi"}},
				}},
			},
			wantNudgeLog:     false,
			wantIdleGuardLog: true,
			desc:             "Patrol wisps in hooked state do not count as active work; nudge must be suppressed",
		},
		{
			name:         "store error: stale heartbeat, store fails — nudge sent conservatively",
			heartbeatAge: 10 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{err: fmt.Errorf("db offline")},
			},
			wantNudgeLog:     true,
			wantIdleGuardLog: false,
			desc:             "Nudge must fire conservatively when work state is unknown",
		},
		{
			name:         "very stale: heartbeat >= 20 min — escalation path, no nudge",
			heartbeatAge: 21 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			wantNudgeLog:     false,
			wantIdleGuardLog: false,
			desc:             "Very stale heartbeat takes escalation path, not nudge path; idle guard not reached",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeDeaconHeartbeat(t, townRoot, tc.heartbeatAge)

			d, tm, clk := newDeaconHeartbeatDaemon(t, townRoot, tc.stores)

			logBuf := &strings.Builder{}
			d.logger = log.New(logBuf, "", 0)

			runOnClock(t, clk, time.Second, d.checkDeaconHeartbeat)

			if got, want := deaconHealthChecks(tm) == 1, tc.wantNudgeLog; got != want {
				t.Errorf("%s\nnudge delivered=%v, want=%v (sent: %q)", tc.desc, got, want, tm.Sent(session.DeaconSessionName()))
			}

			logOutput := logBuf.String()

			hasIdleGuardLog := strings.Contains(logOutput, "nudge skipped")
			if hasIdleGuardLog != tc.wantIdleGuardLog {
				t.Errorf("%s\nidle guard log present=%v, want=%v\nlog:\n%s",
					tc.desc, hasIdleGuardLog, tc.wantIdleGuardLog, logOutput)
			}

			hasNudgeLog := strings.Contains(logOutput, "nudging session")
			if hasNudgeLog != tc.wantNudgeLog {
				t.Errorf("%s\nnudge log present=%v, want=%v\nlog:\n%s",
					tc.desc, hasNudgeLog, tc.wantNudgeLog, logOutput)
			}
		})
	}
}
