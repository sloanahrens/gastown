package tmux

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// These tests drive AcceptWorkspaceTrustDialog against a scripted pane that
// renders a trust dialog and records the keys it receives, so the assertion is
// on the keys the function actually sends rather than on the fact that it
// returned without error (gt-nc1t: the old blind Enter also returned without
// error, while quitting Claude).

// acceptTrust runs AcceptWorkspaceTrustDialog against pane on a fake clock.
func acceptTrust(t *testing.T, pane *fakePane) error {
	t.Helper()
	s := newScripted(pane.answer)
	clk := newFixedClock()
	done := make(chan error, 1)
	go func() { done <- unitTmux(s, clk).AcceptWorkspaceTrustDialog("gt-x") }()
	return driveClock(t, clk, constants.DialogPollInterval, done)
}

// TestAcceptWorkspaceTrustDialog_SelectsTrustOption is the regression test for
// gt-nc1t: with the trust option rendered second and "No, exit" focused, the
// function must move the selection down before confirming. Sending Enter alone
// selected "No, exit" and exited Claude.
func TestAcceptWorkspaceTrustDialog_SelectsTrustOption(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: claudeTrustDialogCancelFirst, confirm: showPrompt}
	if err := acceptTrust(t, pane); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}
	if got := pane.sentKeys(); !reflect.DeepEqual(got, []string{"Down", "Enter"}) {
		t.Errorf("keys = %q, want Down then Enter", got)
	}
}

// TestAcceptWorkspaceTrustDialog_TrustOptionAlreadyFocused covers the reverse
// option order: the function must confirm without moving the selection, so a
// dialog whose trust option is already focused is not walked off it.
func TestAcceptWorkspaceTrustDialog_TrustOptionAlreadyFocused(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: claudeTrustDialogYesFirst, confirm: showPrompt}
	if err := acceptTrust(t, pane); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}
	if got := pane.sentKeys(); !reflect.DeepEqual(got, []string{"Enter"}) {
		t.Errorf("keys = %q, want Enter only", got)
	}
}

// TestAcceptWorkspaceTrustDialog_ReportsSessionDeath covers the outcome the
// function exists to name: the dialog's exit option was selected and the pane is
// gone. Before gt-nc1t this surfaced later as the opaque
// "starting session: startup blocked: tmux capture-pane: can't find pane".
func TestAcceptWorkspaceTrustDialog_ReportsSessionDeath(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: claudeTrustDialogCancelFirst, confirm: exitPane}
	err := acceptTrust(t, pane)
	if err == nil {
		t.Fatal("expected an error after the pane died, got nil")
	}
	if !strings.Contains(err.Error(), "died after answering the workspace trust prompt") {
		t.Errorf("error = %q, want it to name the dialog that killed the session", err)
	}
}

// TestAcceptWorkspaceTrustDialog_StillVisibleTimesOut: a dialog that stays up
// after Enter is reported once the poll window passes.
func TestAcceptWorkspaceTrustDialog_StillVisibleTimesOut(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: claudeTrustDialogYesFirst}
	err := acceptTrust(t, pane)
	if err == nil || !strings.Contains(err.Error(), "still visible") {
		t.Fatalf("err = %v, want the dialog reported as still visible", err)
	}
}

// TestAcceptWorkspaceTrustDialog_UnreadableDialogLeavesPaneAlone covers the
// fail-closed branch: dialog text with no readable option list sends nothing,
// rather than pressing a key into an unread dialog.
func TestAcceptWorkspaceTrustDialog_UnreadableDialogLeavesPaneAlone(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: " Quick safety check: Is this a project you created or one you trust?"}
	err := acceptTrust(t, pane)
	if err == nil {
		t.Fatal("expected an error for a dialog with no readable options, got nil")
	}
	if !strings.Contains(err.Error(), "no selectable options") {
		t.Errorf("error = %q, want it to report that no options could be read", err)
	}
	if got := pane.sentKeys(); len(got) != 0 {
		t.Errorf("keys = %q, want none: an unreadable dialog must not be answered", got)
	}
}

// TestAcceptWorkspaceTrustDialog_StaleDialogTextIgnoresPrompt covers dialog text
// left in the pane above a live prompt, which is not a blocking dialog: nothing is
// selected and nothing is pressed.
func TestAcceptWorkspaceTrustDialog_StaleDialogTextIgnoresPrompt(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: " Quick safety check: Is this a project you created or one you trust?\n❯ "}
	if err := acceptTrust(t, pane); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}
	if got := pane.sentKeys(); len(got) != 0 {
		t.Errorf("keys = %q, want none: stale dialog text is not a dialog", got)
	}
}

// TestAcceptWorkspaceTrustDialog_StaleOptionsIgnorePrompt covers the same stale
// pane carrying a *parseable* option list, which is what a fully rendered and
// then dismissed dialog leaves behind. The list parses, so the stale state has to
// be caught before navigation or the function sends Down+Enter into the agent's
// live composer (gt-sd1o).
func TestAcceptWorkspaceTrustDialog_StaleOptionsIgnorePrompt(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: claudeTrustDialogCancelFirst + "\n❯ "}
	if err := acceptTrust(t, pane); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}
	if got := pane.sentKeys(); len(got) != 0 {
		t.Errorf("keys = %q, want none: a stale option list is not a dialog to answer", got)
	}
}

// TestAcceptWorkspaceTrustDialog_WaitsForLateDialog: the dialog renders after
// the first capture; the poll picks it up.
func TestAcceptWorkspaceTrustDialog_WaitsForLateDialog(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: "", confirm: showPrompt}
	s := newScripted(pane.answer)
	clk := newFixedClock()
	done := make(chan error, 1)
	go func() { done <- unitTmux(s, clk).AcceptWorkspaceTrustDialog("gt-x") }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatal(err)
	}
	pane.set(claudeTrustDialogYesFirst)
	if err := driveClock(t, clk, constants.DialogPollInterval, done); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}
	if got := pane.sentKeys(); !reflect.DeepEqual(got, []string{"Enter"}) {
		t.Errorf("keys = %q, want Enter", got)
	}
}
