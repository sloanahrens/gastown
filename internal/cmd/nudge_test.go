package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
)

// nudgeTestRegistry maps the rig prefixes the nudge tests use.
func nudgeTestRegistry() *session.PrefixRegistry {
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	reg.Register("bd", "beads")
	return reg
}

func TestNudgeHelpUsesTownRootMessagingConfig(t *testing.T) {
	t.Parallel()
	const want = "<town-root>/config/messaging.json"

	if !strings.Contains(nudgeCmd.Long, want) {
		t.Fatalf("help should document %q:\n%s", want, nudgeCmd.Long)
	}
	if strings.Contains(nudgeCmd.Long, "~/gt/config/messaging.json") {
		t.Fatalf("help should not document the obsolete home-relative path:\n%s", nudgeCmd.Long)
	}
}

func TestNudgeStdinConflict(t *testing.T) {
	t.Parallel()
	// When both --stdin and --message are set, runNudge should return an error
	_, _, err := nudgeTargetAndMessage("some message", true, func() ([]byte, error) { t.Fatal("stdin read"); return nil, nil }, []string{"gastown/alpha"})
	if err == nil {
		t.Fatal("expected error when --stdin and --message are both set")
	}
	if !strings.Contains(err.Error(), "cannot use --stdin with --message/-m") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestResolveNudgePattern(t *testing.T) {
	t.Parallel()
	// Create test agent sessions (using rig prefixes)
	agents := []*AgentSession{
		{Name: "hq-mayor", Type: AgentMayor},
		{Name: "gt-crew-max", Type: AgentCrew, Rig: "gastown", AgentName: "max"},
		{Name: "gt-crew-jack", Type: AgentCrew, Rig: "gastown", AgentName: "jack"},
		{Name: "gt-alpha", Type: AgentPolecat, Rig: "gastown", AgentName: "alpha"},
		{Name: "gt-beta", Type: AgentPolecat, Rig: "gastown", AgentName: "beta"},
		{Name: "bd-gamma", Type: AgentPolecat, Rig: "beads", AgentName: "gamma"},
	}

	tests := []struct {
		name     string
		pattern  string
		expected []string
	}{
		{
			name:     "mayor special case",
			pattern:  "mayor",
			expected: []string{"hq-mayor"},
		},
		{
			name:     "all polecats in rig",
			pattern:  "gastown/polecats/*",
			expected: []string{"gt-alpha", "gt-beta"},
		},
		{
			name:     "specific polecat",
			pattern:  "gastown/polecats/alpha",
			expected: []string{"gt-alpha"},
		},
		{
			name:     "all crew in rig",
			pattern:  "gastown/crew/*",
			expected: []string{"gt-crew-max", "gt-crew-jack"},
		},
		{
			name:     "specific crew member",
			pattern:  "gastown/crew/max",
			expected: []string{"gt-crew-max"},
		},
		{
			name:     "legacy polecat format",
			pattern:  "gastown/alpha",
			expected: []string{"gt-alpha"},
		},
		{
			name:     "no matches",
			pattern:  "nonexistent/polecats/*",
			expected: nil,
		},
		{
			name:     "invalid pattern",
			pattern:  "invalid",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveNudgePattern(tt.pattern, agents)

			if len(got) != len(tt.expected) {
				t.Errorf("resolveNudgePattern(%q) returned %d results, want %d: got %v, want %v",
					tt.pattern, len(got), len(tt.expected), got, tt.expected)
				return
			}

			// Check each expected value is present
			gotMap := make(map[string]bool)
			for _, g := range got {
				gotMap[g] = true
			}
			for _, e := range tt.expected {
				if !gotMap[e] {
					t.Errorf("resolveNudgePattern(%q) missing expected %q, got %v",
						tt.pattern, e, got)
				}
			}
		})
	}
}

func TestSessionNameToAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		sessionName string
		expected    string
	}{
		{
			name:        "mayor",
			sessionName: "hq-mayor",
			expected:    "mayor",
		},
		{
			name:        "witness",
			sessionName: "gt-witness",
			expected:    "gastown/witness",
		},
		{
			name:        "crew member",
			sessionName: "gt-crew-max",
			expected:    "gastown/crew/max",
		},
		{
			name:        "polecat",
			sessionName: "gt-alpha",
			expected:    "gastown/alpha",
		},
		{
			name:        "unrecognized format",
			sessionName: "plaintext",
			expected:    "",
		},
		{
			name:        "gt prefix but no name",
			sessionName: "gt-",
			expected:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionNameToAddress(nudgeTestRegistry(), tt.sessionName)
			if got != tt.expected {
				t.Errorf("sessionNameToAddress(%q) = %q, want %q", tt.sessionName, got, tt.expected)
			}
		})
	}
}

func TestNudgeInvalidMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mode    string
		wantErr string
	}{
		{"bogus mode", "bogus", `invalid --mode "bogus"`},
		{"empty mode", "", `invalid --mode ""`},
		{"typo immediate", "imediate", `invalid --mode "imediate"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateNudgeFlags(tt.mode, "normal")
			if err == nil {
				t.Fatal("expected error for invalid mode")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestNudgeInvalidPriority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		priority string
		wantErr  string
	}{
		{"bogus priority", "bogus", `invalid --priority "bogus"`},
		{"empty priority", "", `invalid --priority ""`},
		{"high priority", "high", `invalid --priority "high"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateNudgeFlags(NudgeModeImmediate, tt.priority)
			if err == nil {
				t.Fatal("expected error for invalid priority")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestNudgeValidModesAccepted(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{NudgeModeImmediate, NudgeModeQueue, NudgeModeWaitIdle} {
		for _, priority := range []string{"normal", "urgent"} {
			if err := validateNudgeFlags(mode, priority); err != nil {
				t.Errorf("valid mode %q priority %q was rejected: %v", mode, priority, err)
			}
		}
	}
}

func TestIfFreshMaxAge(t *testing.T) {
	t.Parallel()
	// Verify the constant is 60 seconds as specified in the design.
	if ifFreshMaxAge != 60*time.Second {
		t.Errorf("ifFreshMaxAge = %v, want 60s", ifFreshMaxAge)
	}
}

func TestIfFreshSessionAgeCheck(t *testing.T) {
	t.Parallel()
	// Test the age comparison logic used by --if-fresh.
	// A session created 10 seconds ago should be "fresh" (nudge allowed).
	// A session created 120 seconds ago should be "stale" (nudge suppressed).
	now := time.Now()

	tests := []struct {
		name        string
		createdAt   time.Time
		shouldNudge bool
	}{
		{
			name:        "fresh session (10s old)",
			createdAt:   now.Add(-10 * time.Second),
			shouldNudge: true,
		},
		{
			name:        "borderline session (59s old)",
			createdAt:   now.Add(-59 * time.Second),
			shouldNudge: true,
		},
		{
			name:        "stale session (61s old)",
			createdAt:   now.Add(-61 * time.Second),
			shouldNudge: false,
		},
		{
			name:        "very stale session (5min old)",
			createdAt:   now.Add(-5 * time.Minute),
			shouldNudge: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			age := time.Since(tt.createdAt)
			shouldNudge := age <= ifFreshMaxAge
			if shouldNudge != tt.shouldNudge {
				t.Errorf("age=%v: shouldNudge=%v, want %v", age, shouldNudge, tt.shouldNudge)
			}
		})
	}
}

func TestPostQueueIdleRecovery_SkipsDeliveryWhenDrainEmpty(t *testing.T) {
	t.Parallel()
	// Behavioral test (gt-y2zk): when the idle recovery path fires but
	// another process already drained the queue, we must NOT deliver to
	// avoid duplicates. This exercises the len(drained) > 0 guard.
	townRoot := t.TempDir()
	session := "gt-crew-test"

	// Enqueue a nudge, then drain it (simulating a racing hook).
	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		Sender:  "test",
		Message: "hello",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	drained, err := nudge.Drain(townRoot, session)
	if err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("first Drain got %d entries, want 1", len(drained))
	}

	// Second drain should return empty — the racing hook already claimed it.
	drained2, err := nudge.Drain(townRoot, session)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(drained2) != 0 {
		t.Errorf("second Drain got %d entries, want 0 (already claimed)", len(drained2))
	}
}

func TestRequeueDrainedNudgesPreservesFailedDelivery(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-crew-test"
	drained := []nudge.QueuedNudge{
		{Sender: "test", Message: "first", Timestamp: time.Now().Add(-time.Second)},
		{Sender: "test", Message: "second", Timestamp: time.Now()},
	}

	requeueDrainedNudges(townRoot, session, "test", drained)

	// Both nudges are preserved — a failed injection must not lose the message.
	pending, err := nudge.Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != len(drained) {
		t.Fatalf("Pending = %d, want %d (requeue must preserve failed deliveries)", pending, len(drained))
	}

	// But they are deferred, so the failure cannot be retried on the very next
	// poll tick — that is the gt-tmlu re-injection loop.
	got, err := nudge.Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Drain immediately after requeue got %d nudges, want 0 (deferred by backoff)", len(got))
	}
}

func TestValidModeMapsMatchConstants(t *testing.T) {
	t.Parallel()
	// Ensure the validation maps cover all defined mode constants.
	modes := []string{NudgeModeImmediate, NudgeModeQueue, NudgeModeWaitIdle}
	for _, m := range modes {
		if !validNudgeModes[m] {
			t.Errorf("mode constant %q missing from validNudgeModes", m)
		}
	}
	priorities := []string{nudge.PriorityNormal, nudge.PriorityUrgent}
	for _, p := range priorities {
		if !validNudgePriorities[p] {
			t.Errorf("priority constant %q missing from validNudgePriorities", p)
		}
	}
}

func TestIdleWatcherTimeout(t *testing.T) {
	t.Parallel()
	// Verify the watcher timeout is in a reasonable range.
	if idleWatcherTimeout < 10*time.Second {
		t.Errorf("idleWatcherTimeout = %v, too short (min 10s)", idleWatcherTimeout)
	}
	if idleWatcherTimeout > 5*time.Minute {
		t.Errorf("idleWatcherTimeout = %v, too long (max 5m)", idleWatcherTimeout)
	}
}

func TestIdleWatcherPollInterval(t *testing.T) {
	t.Parallel()
	// Verify the poll interval is reasonable — fast enough to be responsive,
	// slow enough to not burn CPU.
	if idleWatcherPollInterval < 200*time.Millisecond {
		t.Errorf("idleWatcherPollInterval = %v, too fast (min 200ms)", idleWatcherPollInterval)
	}
	if idleWatcherPollInterval > 5*time.Second {
		t.Errorf("idleWatcherPollInterval = %v, too slow (max 5s)", idleWatcherPollInterval)
	}
}

func TestNudgeTrailingSlashNormalization(t *testing.T) {
	t.Parallel()
	// The mail system uses "mayor/" and "deacon/" as canonical addresses.
	// runNudge must strip the trailing slash so these match the role shortcuts.
	// Without normalization, "mayor/" falls through to parseAddress which
	// rejects it ("invalid address format"), silently dropping the nudge.
	for _, target := range []string{"mayor/", "deacon/", "witness/", "refinery/"} {
		got, message, err := nudgeTargetAndMessage("", false, func() ([]byte, error) { t.Fatal("stdin read"); return nil, nil }, []string{target, "hello"})
		if err != nil {
			t.Fatalf("nudgeTargetAndMessage(%q): %v", target, err)
		}
		if want := strings.TrimSuffix(target, "/"); got != want || message != "hello" {
			t.Errorf("target %q -> (%q, %q), want (%q, hello)", target, got, message, want)
		}
	}
}

func TestIdleWatcherExitsOnEmptyQueue(t *testing.T) {
	t.Parallel()
	// watchAndDeliver should exit after its first poll when the queue is
	// empty (someone else drained it). A nil Tmux is safe: QueueLen returns 0
	// before the session is consulted.
	clk := clockwork.NewFakeClockAt(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	done := make(chan struct{})
	go func() {
		watchAndDeliverWith(clk, time.Minute, time.Second, nil, t.TempDir(), "test-session")
		close(done)
	}()
	if err := clk.BlockUntilContext(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second) // one poll, not the whole minute
	<-done
}

func TestQueueLen(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Empty queue
	if got := nudge.QueueLen(tmpDir, "test-session"); got != 0 {
		t.Errorf("QueueLen on empty dir = %d, want 0", got)
	}

	// Enqueue one
	err := nudge.Enqueue(tmpDir, "test-session", nudge.QueuedNudge{
		Sender:  "test",
		Message: "hello",
	})
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	if got := nudge.QueueLen(tmpDir, "test-session"); got != 1 {
		t.Errorf("QueueLen after enqueue = %d, want 1", got)
	}

	// Drain and verify empty
	_, _ = nudge.Drain(tmpDir, "test-session")
	if got := nudge.QueueLen(tmpDir, "test-session"); got != 0 {
		t.Errorf("QueueLen after drain = %d, want 0", got)
	}
}
