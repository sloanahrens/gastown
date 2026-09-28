package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/constants"
)

// returnsWithoutClock runs fn and fails the test if it blocks: fn is called
// with a fake clock nobody advances, so any wait on it would hang.
func returnsWithoutClock(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal("blocked on the clock; expected an immediate return")
		return nil
	}
}

func TestIsTransientSendKeysError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"not in a mode", fmt.Errorf("tmux send-keys: not in a mode"), true},
		{"not in a mode wrapped", fmt.Errorf("nudge: %w", fmt.Errorf("tmux send-keys: not in a mode")), true},
		{"session not found", ErrSessionNotFound, false},
		{"no server", ErrNoServer, false},
		{"generic error", fmt.Errorf("something else"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTransientSendKeysError(tt.err)
			if got != tt.want {
				t.Errorf("isTransientSendKeysError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestSendKeysLiteralWithRetry_ImmediateSuccess(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	tm := unitTmux(s, nil)
	if err := returnsWithoutClock(t, func() error { return tm.sendKeysLiteralWithRetry("gt-x", "hello", 5*time.Second) }); err != nil {
		t.Errorf("sendKeysLiteralWithRetry() = %v, want nil", err)
	}
	if c := s.all(); len(c) != 1 || !c[0].has("send-keys", "-t", "gt-x", "-l", "--", "hello") {
		t.Errorf("calls = %v", c)
	}
}

// TestSendKeysLiteralWithRetry_NonTransientFails: a missing session fails on
// the first attempt without waiting out the timeout.
func TestSendKeysLiteralWithRetry_NonTransientFails(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"send-keys": fail("can't find session: gt-x")}))
	tm := unitTmux(s, nil)
	err := returnsWithoutClock(t, func() error { return tm.sendKeysLiteralWithRetry("gt-x", "hello", 5*time.Second) })
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
	if n := len(s.all()); n != 1 {
		t.Errorf("send-keys attempts = %d, want 1 (no retry on a non-transient error)", n)
	}
}

// TestSendKeysLiteralWithRetry_RetriesTransientError: "not in a mode" during
// agent TUI startup is retried until it clears.
func TestSendKeysLiteralWithRetry_RetriesTransientError(t *testing.T) {
	t.Parallel()
	attempts := 0
	s := newScripted(func(c tmuxCall) reply {
		attempts++
		if attempts < 3 {
			return fail("not in a mode")
		}
		return ok("")
	})
	clk := clockwork.NewFakeClock()
	tm := unitTmux(s, clk)
	if err := driven(t, clk, constants.NudgeRetryInterval, func() error {
		return tm.sendKeysLiteralWithRetry("gt-x", "hello", 5*time.Second)
	}); err != nil {
		t.Fatalf("sendKeysLiteralWithRetry = %v, want success on the third attempt", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestSendKeysLiteralWithRetry_TransientUntilTimeout(t *testing.T) {
	t.Parallel()
	s := newScripted(func(tmuxCall) reply { return fail("not in a mode") })
	clk := clockwork.NewFakeClock()
	tm := unitTmux(s, clk)
	err := driven(t, clk, constants.NudgeRetryInterval, func() error {
		return tm.sendKeysLiteralWithRetry("gt-x", "hello", 3*time.Second)
	})
	if err == nil || !strings.Contains(err.Error(), "not ready for input after 3s") {
		t.Fatalf("err = %v, want the timeout wrapping the last error", err)
	}
}

// Regression test for gt-cs0: messages beginning with a dash (e.g. "-r ...")
// were parsed by tmux as send-keys flags, failing with "unknown flag -r" and
// breaking the nudge path town-wide. Every send path must put "--" before
// the text.
func TestSendKeysLeadingDash_NotParsedAsFlags(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	clk := clockwork.NewFakeClock()
	tm := unitTmux(s, clk)
	longMsg := strings.Repeat("x", sendKeysChunkSize) + "-r second chunk starts with dash"
	if err := driven(t, clk, 10*time.Millisecond, func() error {
		if err := tm.sendKeysLiteralWithRetry("gt-x", "-r leading dash", 5*time.Second); err != nil {
			return err
		}
		if err := tm.sendMessageToTarget("gt-x", longMsg); err != nil {
			return err
		}
		if err := tm.SendKeysDebounced("gt-x", "-rf dash message", 0); err != nil {
			return err
		}
		return tm.SendKeysRaw("gt-x", "-r")
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.find("send-keys") {
		text := c.last()
		if !strings.HasPrefix(text, "-") {
			continue
		}
		if c.args[len(c.args)-2] != "--" {
			t.Errorf("dash-leading %q sent without a preceding --: %v", text, c.args)
		}
	}
}

// nudgeFixture is a session whose first pane is an idle agent composer. The
// session is named after the test: nudges serialize on a process-wide lock
// keyed by session name, so parallel tests must not share one.
func nudgeFixture(t *testing.T) (*fakeServer, *fpane, string) {
	f := newFakeServer()
	name := "gt-" + t.Name()
	s := f.addSession(name, "claude")
	f.withComposer(s.panes[0], "⏺ Waiting for work.")
	return f, s.panes[0], name
}

func nudge(t *testing.T, f *fakeServer, session, message string) (*scripted, error) {
	t.Helper()
	clk := clockwork.NewFakeClock()
	tm, s := f.tmux(clk)
	return s, driven(t, clk, 50*time.Millisecond, func() error { return tm.NudgeSession(session, message) })
}

// TestNudgeSession_Delivers: the message is typed into the agent pane and
// submitted, and the call reports success.
func TestNudgeSession_Delivers(t *testing.T) {
	t.Parallel()
	f, pane, name := nudgeFixture(t)
	if _, err := nudge(t, f, name, "test message"); err != nil {
		t.Fatalf("NudgeSession() = %v, want nil", err)
	}
	if got := f.submitted(pane); len(got) != 1 || got[0] != "test message" {
		t.Fatalf("submitted = %q, want [test message]", got)
	}
}

// TestNudgeSession_ReportsUnsubmittedInput: Enter that never clears the
// composer is an error, not a silent success (gt-0b5).
func TestNudgeSession_ReportsUnsubmittedInput(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	name := "gt-" + t.Name()
	s := f.addSession(name, "claude")
	f.with(func() { s.panes[0].content = "⏺ Waiting.\n❯ stuck text" })
	if _, err := nudge(t, f, name, "test message"); err == nil {
		t.Fatal("NudgeSession on a pane that never changes = nil, want an error")
	}
}

func TestNudgeSession_WithStoredPaneID(t *testing.T) {
	t.Parallel()
	f, pane, name := nudgeFixture(t)
	f.with(func() { f.sessions[name].env["GT_PANE_ID"] = pane.id })
	if _, err := nudge(t, f, name, "test message"); err != nil {
		t.Fatalf("NudgeSession() with GT_PANE_ID = %v, want nil", err)
	}
	if got := f.submitted(pane); len(got) != 1 {
		t.Fatalf("submitted = %q, want the nudge in the declared pane", got)
	}
}

// TestNudgeSession_WakesAgentWindowNotActiveWindow is a regression test for a
// missed-wake bug in multi-window sessions: the SIGWINCH resize dance must
// target the agent's pane, not the bare session (whose active window may be
// another one, e.g. a focused gt feed -w window).
func TestNudgeSession_WakesAgentWindowNotActiveWindow(t *testing.T) {
	t.Parallel()
	f, pane, name := nudgeFixture(t)
	f.with(func() { f.sessions[name].env["GT_PANE_ID"] = pane.id })
	feed := f.addWindow(name, "")
	s, err := nudge(t, f, name, "test message")
	if err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	resizes := s.find("resize-window")
	if len(resizes) != 2 {
		t.Fatalf("resize-window calls = %v, want the +1/restore pair", resizes)
	}
	for _, c := range resizes {
		if flagValue(c.args, "-t") != name+":0.0" {
			t.Errorf("resize-window targeted %q, want the agent pane %s:0.0", flagValue(c.args, "-t"), name)
		}
	}
	if pane.windowSize != "latest" || feed.windowSize != "" {
		t.Errorf("window-size agent=%q feed=%q, want the agent window reset to latest and the feed window untouched", pane.windowSize, feed.windowSize)
	}
}

func TestCanonicalPaneTargetFromDisplay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		expectedSession string
		out             string
		want            string
		wantOK          bool
	}{
		{
			name:            "valid target",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t0\t0",
			want:            "gt-alpha:0.0",
			wantOK:          true,
		},
		{
			name:            "valid multi window",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t12\t3",
			want:            "gt-alpha:12.3",
			wantOK:          true,
		},
		{
			name:            "wrong session",
			expectedSession: "gt-alpha",
			out:             "gt-beta\t0\t0",
			wantOK:          false,
		},
		{
			name:            "malformed combined target",
			expectedSession: "gt-alpha",
			out:             "gt-alpha:0.0",
			wantOK:          false,
		},
		{
			name:            "empty output",
			expectedSession: "gt-alpha",
			out:             "",
			wantOK:          false,
		},
		{
			name:            "non numeric window",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\tactive\t0",
			wantOK:          false,
		},
		{
			name:            "non numeric pane",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t0\tactive",
			wantOK:          false,
		},
		{
			name:            "negative index",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t0\t-1",
			wantOK:          false,
		},
		{
			name:            "signed index",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t+1\t0",
			wantOK:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := canonicalPaneTargetFromDisplay(tt.expectedSession, tt.out)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("target = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalPaneTargetResolvesAndFallsBack(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	target := f.addSession("gt-x", "")
	other := f.addSession("gt-other", "")
	second := f.addWindow("gt-x", "")
	tm, _ := f.tmux(nil)

	fallback := "gt-x:0.0"
	if got := tm.canonicalPaneTarget("gt-x", ""); got != fallback {
		t.Errorf("empty pane target = %q, want %q", got, fallback)
	}
	if got := tm.canonicalPaneTarget("gt-x", "%999999"); got != fallback {
		t.Errorf("missing pane target = %q, want %q", got, fallback)
	}
	if got := tm.canonicalPaneTarget("gt-x", target.panes[0].id); got != fallback {
		t.Errorf("live pane target = %q, want %q", got, fallback)
	}
	if got := tm.canonicalPaneTarget("gt-x", second.id); got != "gt-x:1.0" {
		t.Errorf("second-window pane target = %q, want gt-x:1.0", got)
	}
	if got := tm.canonicalPaneTarget("gt-x", other.panes[0].id); got != fallback {
		t.Errorf("cross-session pane target = %q, want %q", got, fallback)
	}
}

// TestNudgeSession_StalePaneIDFallsBackToFirstPane: a GT_PANE_ID naming a
// pane of another session sends the nudge to this session's first pane,
// not to that session and not to the active window.
func TestNudgeSession_StalePaneIDFallsBackToFirstPane(t *testing.T) {
	t.Parallel()
	f, pane, name := nudgeFixture(t)
	other := f.addSession("gt-other", "claude")
	f.withComposer(other.panes[0])
	f.with(func() { f.sessions[name].env["GT_PANE_ID"] = other.panes[0].id })
	active := f.addWindow(name, "")

	if _, err := nudge(t, f, name, "echo marker"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	if got := f.submitted(pane); len(got) != 1 || got[0] != "echo marker" {
		t.Fatalf("first pane submitted %q, want the nudge", got)
	}
	if got := f.submitted(other.panes[0]); len(got) != 0 {
		t.Fatalf("cross-session stale pane received %q", got)
	}
	if got := f.paneKeys(active); len(got) != 0 {
		t.Fatalf("active window received keys %q", got)
	}
}
