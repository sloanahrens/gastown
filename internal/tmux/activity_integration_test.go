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

	// Wait until the wall clock is past tmux's 1-second activity resolution,
	// then produce pane output on this UNATTACHED session (no client ever
	// attaches in this test — that is the whole point).
	eventually(t, "a new wall-clock second", func() bool { return time.Now().Unix() > before.Unix() })
	if err := tm.SendKeys(sessionName, "echo hello-from-gt-2sln-test"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	eventually(t, "window_activity to advance on an unattended session", func() bool {
		after, err := tm.GetWindowActivity(sessionName)
		return err == nil && after.After(before)
	})

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
		t.Fatalf("session_activity advanced (session_activity=%v created=%v): something attached a client to the isolated test server", sessActivity, createdUnix)
	}
}
