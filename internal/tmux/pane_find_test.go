package tmux

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

func TestFindAgentPane_SinglePane(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-x", "")
	tm, _ := f.tmux(nil)
	paneID, err := tm.FindAgentPane("gt-x")
	if err != nil {
		t.Fatalf("FindAgentPane: %v", err)
	}
	if paneID != "" {
		t.Errorf("FindAgentPane single pane = %q, want empty (no disambiguation needed)", paneID)
	}
}

// TestFindAgentPane_UsesDeclaredPane: GT_PANE_ID naming a pane of this
// session is returned without scanning (gt-qmsx).
func TestFindAgentPane_UsesDeclaredPane(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	s := f.addSession("gt-x", "")
	second := f.addPane("gt-x", "claude")
	f.with(func() { s.env["GT_PANE_ID"] = second.id })
	tm, sc := f.tmux(nil)
	if got, err := tm.FindAgentPane("gt-x"); err != nil || got != second.id {
		t.Fatalf("FindAgentPane = %q, %v; want %q", got, err, second.id)
	}
	if len(sc.find("list-panes")) != 0 {
		t.Error("scanned panes although GT_PANE_ID resolved")
	}
}

// TestFindAgentPane_IgnoresPaneIDFromOtherSession: pane IDs are tmux-global,
// so a stale GT_PANE_ID can name a pane in a different session; it must not
// be returned.
func TestFindAgentPane_IgnoresPaneIDFromOtherSession(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	target := f.addSession("gt-target", "")
	other := f.addSession("gt-other", "claude")
	f.with(func() { target.env["GT_PANE_ID"] = other.panes[0].id })
	tm, _ := f.tmux(nil)
	paneID, err := tm.FindAgentPane("gt-target")
	if err != nil {
		t.Fatalf("FindAgentPane: %v", err)
	}
	if paneID == other.panes[0].id {
		t.Fatalf("FindAgentPane returned pane %q from another session", paneID)
	}
}

// TestFindAgentPane_MultiPaneWithAgent: the scan picks the pane running the
// session's declared process.
func TestFindAgentPane_MultiPaneWithAgent(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	s := f.addSession("gt-x", "")
	agent := f.addPane("gt-x", "sleep")
	f.with(func() { s.env["GT_PROCESS_NAMES"] = "sleep" })
	tm, _ := f.tmux(nil)
	if got, err := tm.FindAgentPane("gt-x"); err != nil || got != agent.id {
		t.Fatalf("FindAgentPane = %q, %v; want %q", got, err, agent.id)
	}
}

func TestFindAgentPane_NonexistentSession(t *testing.T) {
	t.Parallel()
	tm, _ := newFakeServer().tmux(nil)
	if _, err := tm.FindAgentPane("nonexistent-session-findagent-xyz"); err == nil {
		t.Error("FindAgentPane on nonexistent session should return error")
	}
}

func TestFindAgentPane_MultiPaneNoAgent(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-x", "")
	f.addPane("gt-x", "")
	tm, _ := f.tmux(nil)
	paneID, err := tm.FindAgentPane("gt-x")
	if err != nil {
		t.Fatalf("FindAgentPane: %v", err)
	}
	if paneID != "" {
		t.Errorf("FindAgentPane with no agent = %q, want empty", paneID)
	}
}

func TestNudgeLockTimeout(t *testing.T) {
	t.Parallel()
	session := "test-nudge-" + t.Name()
	clk := clockwork.NewFakeClock()
	if !acquireNudgeLock(clk, session, time.Second) {
		t.Fatal("initial acquireNudgeLock should succeed")
	}
	got := make(chan bool, 1)
	go func() { got <- acquireNudgeLock(clk, session, 100*time.Millisecond) }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(100 * time.Millisecond)
	if <-got {
		t.Error("acquireNudgeLock should return false when lock is held")
		releaseNudgeLock(session)
	}
	releaseNudgeLock(session)
	if !acquireNudgeLock(clk, session, time.Second) {
		t.Error("acquireNudgeLock should succeed after release")
	}
	releaseNudgeLock(session)
}

// TestNudgeLockConcurrency: with the lock held and five waiters, a release
// admits exactly one; the rest time out.
func TestNudgeLockConcurrency(t *testing.T) {
	t.Parallel()
	session := "test-nudge-" + t.Name()
	const waiters = 5
	clk := clockwork.NewFakeClock()
	if !acquireNudgeLock(clk, session, time.Second) {
		t.Fatal("initial acquire should succeed")
	}
	acquired := make(chan bool, waiters)
	for i := 0; i < waiters; i++ {
		go func() { acquired <- acquireNudgeLock(clk, session, 200*time.Millisecond) }()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, waiters); err != nil {
		t.Fatal(err)
	}
	releaseNudgeLock(session)
	// Wait for one waiter to take the freed slot before any time passes.
	for sem := getSessionNudgeSem(session); len(sem) == 0; {
		if ctx.Err() != nil {
			t.Fatal("no waiter took the released lock")
		}
		runtime.Gosched()
	}
	if err := clk.BlockUntilContext(ctx, waiters-1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(200 * time.Millisecond)
	successes := 0
	for i := 0; i < waiters; i++ {
		if <-acquired {
			successes++
		}
	}
	releaseNudgeLock(session)
	if successes != 1 {
		t.Errorf("%d/%d waiters acquired the lock, want exactly 1", successes, waiters)
	}
}

func TestNudgeLockDifferentSessions(t *testing.T) {
	t.Parallel()
	session1 := "test-nudge-a-" + t.Name()
	session2 := "test-nudge-b-" + t.Name()
	clk := clockwork.NewFakeClock()
	if !acquireNudgeLock(clk, session1, time.Second) {
		t.Fatal("acquire session1 should succeed")
	}
	defer releaseNudgeLock(session1)
	if !acquireNudgeLock(clk, session2, time.Second) {
		t.Error("acquire session2 should succeed even when session1 is locked")
	} else {
		releaseNudgeLock(session2)
	}
}

func TestValidateSessionName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		session string
		wantErr bool
	}{
		{"valid alphanumeric", "gt-gastown-crew-tom", false},
		{"valid with underscore", "hq_deacon", false},
		{"valid simple", "test123", false},
		{"empty string", "", true},
		{"contains dot", "my.session", true},
		{"contains colon", "my:session", true},
		{"contains space", "my session", true},
		{"contains slash", "rig/crew/tom", true},
		{"contains single quote", "it's", true},
		{"contains semicolon", "a;rm -rf /", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSessionName(tc.session)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateSessionName(%q) error = %v, wantErr %v", tc.session, err, tc.wantErr)
			}
		})
	}
}

// TestNewSessionWithCommandAndEnv: -e values land in the session
// environment, where GetEnvironment reads them back.
func TestNewSessionWithCommandAndEnv(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	env := map[string]string{
		"GT_ROLE": "testrig/crew/testname",
		"GT_RIG":  "testrig",
		"GT_CREW": "testname",
	}
	if err := createAndDrive(t, newScripted(f.answer), func(tm *Tmux) error {
		return tm.NewSessionWithCommandAndEnv("gt-x", "", `bash -c "echo GT_ROLE=$GT_ROLE; sleep 5"`, env)
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	tm, _ := f.tmux(nil)
	for k, want := range env {
		if got, err := tm.GetEnvironment("gt-x", k); err != nil || got != want {
			t.Errorf("GetEnvironment(%s) = %q, %v; want %q", k, got, err, want)
		}
	}
}

func TestNewSessionWithCommandAndEnvEmpty(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	s := newScripted(f.answer)
	if err := createAndDrive(t, s, func(tm *Tmux) error {
		return tm.NewSessionWithCommandAndEnv("gt-x", "", "sleep 5", nil)
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv with nil env: %v", err)
	}
	if !f.has("gt-x") {
		t.Fatal("expected session to exist after creation with empty env")
	}
	if ns := s.find("new-session"); len(ns) != 1 || ns[0].has("-e") {
		t.Errorf("new-session = %v, want no -e flags", ns)
	}
}
