//go:build integration

package tmux

import (
	"testing"
	"time"
)

// TestIntegrationGetWindowActivity_AdvancesOnUnattendedOutput is the regression guard
// for gt-2sln: an unattached session's #{session_activity} freezes at
// session_created and never moves, while #{window_activity} advances when
// the pane produces output. This distinguishes "gate 2 discriminates
// hung-from-working" (correct fix) from "gate 2 is comparing a constant
// against a constant" (the bug this replaces).
func TestIntegrationGetWindowActivity_AdvancesOnUnattendedOutput(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-winactivity-adv-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	before, err := tm.GetWindowActivity(sessionName)
	if err != nil {
		t.Fatalf("GetWindowActivity (before): %v", err)
	}

	// Sleep past tmux's 1-second activity resolution, then produce pane
	// output on this UNATTACHED session (no client ever attaches in this
	// test — that is the whole point).
	time.Sleep(1100 * time.Millisecond)
	if err := tm.SendKeys(sessionName, "echo hello-from-gt-2sln-test"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	after, err := tm.GetWindowActivity(sessionName)
	if err != nil {
		t.Fatalf("GetWindowActivity (after): %v", err)
	}

	if !after.After(before) {
		t.Errorf("window_activity did not advance on an unattended session: before=%v after=%v", before, after)
	}

	// Sanity check on the bug this replaces: session_activity should NOT
	// have advanced, confirming the session genuinely stayed unattached.
	sessActivity, err := tm.GetSessionActivity(sessionName)
	if err != nil {
		t.Fatalf("GetSessionActivity: %v", err)
	}
	createdUnix, err := tm.GetSessionCreatedUnix(sessionName)
	if err != nil {
		t.Fatalf("GetSessionCreatedUnix: %v", err)
	}
	if sessActivity.Unix() != createdUnix {
		t.Skipf("session_activity advanced (session_activity=%v created=%v) — environment attached the session, precondition for this regression guard not met", sessActivity, createdUnix)
	}
}
