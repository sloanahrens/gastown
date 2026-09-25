package tmux

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests drive AcceptWorkspaceTrustDialog against a pane that renders a
// trust dialog and records the keystrokes it receives, so the assertion is on the
// keys the function actually sends rather than on the fact that it returned
// without error (gt-nc1t: the old blind Enter also returned without error, while
// quitting Claude).
//
// The recording fixture is a bash script because the pane's login shell is not
// necessarily bash and `read -n1` is a bash extension.

// trustFixtureSleep keeps the pane's process alive while the test inspects it.
const trustFixtureSleep = 30

// trustFixtureOptions shapes the recording fixture beyond its defaults.
type trustFixtureOptions struct {
	// exitAfterConfirm makes the process exit without printing anything once
	// confirmed: a session that chose "No, exit" tears its UI down and dies
	// without ever drawing a prompt.
	exitAfterConfirm bool
	// promptFirst prints a Claude-like prompt below the dialog text before
	// blocking, which is what a pane that still holds an old dialog's text above
	// a live prompt looks like.
	promptFirst bool
}

// writeTrustDialogFixture writes a script that prints dialog, then records every
// key it receives to keysPath, appending "ENTER" when the confirm key arrives.
// After confirming it prints a Claude-like prompt, so the pane stops looking
// blocked exactly as a real dismissed dialog does.
func writeTrustDialogFixture(t *testing.T, dialog string, opts trustFixtureOptions) (script, dialogPath, keysPath string) {
	t.Helper()

	dir := t.TempDir()
	dialogPath = filepath.Join(dir, "dialog.txt")
	keysPath = filepath.Join(dir, "keys.out")
	script = filepath.Join(dir, "fixture.sh")

	if err := os.WriteFile(dialogPath, []byte(dialog), 0o600); err != nil {
		t.Fatalf("writing dialog fixture: %v", err)
	}

	before := ""
	if opts.promptFirst {
		// Leading newline: the dialog file has no trailing one, so without it the
		// prompt would be appended to the dialog's last line and the prompt would
		// share a line with the blocker.
		before = "printf '\\n\\xe2\\x9d\\xaf '\n"
	}
	after := "printf '\\n\\xe2\\x9d\\xaf '\nsleep " + strconv.Itoa(trustFixtureSleep)
	if opts.exitAfterConfirm {
		after = "exit 0"
	}
	body := `#!/bin/bash
cat "$1"
` + before + `: > "$2"
while IFS= read -r -n1 c; do
  if [ -z "$c" ]; then printf 'ENTER' >> "$2"; break; fi
  printf '%s' "$c" >> "$2"
done
` + after + "\n"

	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("writing fixture script: %v", err)
	}
	return script, dialogPath, keysPath
}

// startTrustDialogSession starts a tmux session running the recording fixture and
// waits until the dialog text is visible in the pane.
func startTrustDialogSession(t *testing.T, tm *Tmux, sessionName string, dialog string, opts trustFixtureOptions) (keysPath string) {
	t.Helper()

	script, dialogPath, keysPath := writeTrustDialogFixture(t, dialog, opts)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommand(sessionName, "", "bash "+script+" "+dialogPath+" "+keysPath); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	deadline := time.Now().Add(shellPromptWaitTimeout)
	for time.Now().Before(deadline) {
		if content, err := tm.CapturePane(sessionName, 30); err == nil && containsWorkspaceTrustDialog(content) {
			// With promptFirst the fixture prints the prompt after the dialog text;
			// wait for it too, or the function can capture the pane in between and
			// judge the dialog live rather than stale.
			if !opts.promptFirst || containsPromptIndicator(content) {
				return keysPath
			}
		}
		time.Sleep(shellPromptPollInterval)
	}
	t.Skipf("dialog fixture never rendered in %s; the environment cannot supply this test's precondition", sessionName)
	return ""
}

func readRecordedKeys(t *testing.T, keysPath string) string {
	t.Helper()

	data, err := os.ReadFile(keysPath)
	if err != nil {
		t.Fatalf("reading recorded keys: %v", err)
	}
	return strings.ReplaceAll(string(data), "\x1b", "^[")
}

// TestAcceptWorkspaceTrustDialog_SelectsTrustOption is the regression test for
// gt-nc1t: with the trust option rendered second and "No, exit" focused, the
// function must move the selection down before confirming. Sending Enter alone
// selected "No, exit" and exited Claude.
func TestAcceptWorkspaceTrustDialog_SelectsTrustOption(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-trust-select-" + t.Name()
	keysPath := startTrustDialogSession(t, tm, sessionName, claudeTrustDialogCancelFirst, trustFixtureOptions{})

	if err := tm.AcceptWorkspaceTrustDialog(sessionName); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}

	if got := readRecordedKeys(t, keysPath); got != "^[[BENTER" {
		t.Errorf("recorded keys = %q, want %q (Down then Enter)", got, "^[[BENTER")
	}
}

// TestAcceptWorkspaceTrustDialog_TrustOptionAlreadyFocused covers the reverse
// option order: the function must confirm without moving the selection, so a
// dialog whose trust option is already focused is not walked off it.
func TestAcceptWorkspaceTrustDialog_TrustOptionAlreadyFocused(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-trust-focus-" + t.Name()
	keysPath := startTrustDialogSession(t, tm, sessionName, claudeTrustDialogYesFirst, trustFixtureOptions{})

	if err := tm.AcceptWorkspaceTrustDialog(sessionName); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}

	if got := readRecordedKeys(t, keysPath); got != "ENTER" {
		t.Errorf("recorded keys = %q, want %q (Enter only)", got, "ENTER")
	}
}

// TestAcceptWorkspaceTrustDialog_CodexTrustPrompt covers Codex's dialog, whose
// leading "> You are in <dir>" banner looks like a prompt: the dialog must still
// be answered rather than skipped as "no dialog here", and the numbered option
// list it renders must not confuse the selection.
func TestAcceptWorkspaceTrustDialog_CodexTrustPrompt(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-trust-codex-" + t.Name()
	keysPath := startTrustDialogSession(t, tm, sessionName, codexTrustDialog, trustFixtureOptions{})

	if err := tm.AcceptWorkspaceTrustDialog(sessionName); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}

	if got := readRecordedKeys(t, keysPath); got != "ENTER" {
		t.Errorf("recorded keys = %q, want %q (Enter only, trust option already focused)", got, "ENTER")
	}
}

// TestAcceptWorkspaceTrustDialog_ReportsSessionDeath covers the outcome the
// function exists to name: the dialog's exit option was selected and the pane is
// gone. Before gt-nc1t this surfaced later as the opaque
// "starting session: startup blocked: tmux capture-pane: can't find pane".
func TestAcceptWorkspaceTrustDialog_ReportsSessionDeath(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-trust-death-" + t.Name()
	startTrustDialogSession(t, tm, sessionName, claudeTrustDialogCancelFirst, trustFixtureOptions{exitAfterConfirm: true})

	err := tm.AcceptWorkspaceTrustDialog(sessionName)
	if err == nil {
		t.Fatal("expected an error after the pane died, got nil")
	}
	if !strings.Contains(err.Error(), "died after answering the workspace trust prompt") {
		t.Errorf("error = %q, want it to name the dialog that killed the session", err)
	}
}

// TestAcceptWorkspaceTrustDialog_UnreadableDialogLeavesPaneAlone covers the
// fail-closed branch: dialog text with no readable option list sends nothing,
// rather than pressing a key into an unread dialog.
func TestAcceptWorkspaceTrustDialog_UnreadableDialogLeavesPaneAlone(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-trust-unreadable-" + t.Name()
	keysPath := startTrustDialogSession(t, tm, sessionName,
		" Quick safety check: Is this a project you created or one you trust?", trustFixtureOptions{})

	err := tm.AcceptWorkspaceTrustDialog(sessionName)
	if err == nil {
		t.Fatal("expected an error for a dialog with no readable options, got nil")
	}
	if !strings.Contains(err.Error(), "no selectable options") {
		t.Errorf("error = %q, want it to report that no options could be read", err)
	}

	if got := readRecordedKeys(t, keysPath); got != "" {
		t.Errorf("recorded keys = %q, want none: an unreadable dialog must not be answered", got)
	}
}

// TestAcceptWorkspaceTrustDialog_StaleDialogTextIgnoresPrompt covers dialog text
// left in the pane above a live prompt, which is not a blocking dialog: nothing is
// selected and nothing is pressed.
func TestAcceptWorkspaceTrustDialog_StaleDialogTextIgnoresPrompt(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-trust-stale-" + t.Name()
	keysPath := startTrustDialogSession(t, tm, sessionName,
		" Quick safety check: Is this a project you created or one you trust?",
		trustFixtureOptions{promptFirst: true})

	if err := tm.AcceptWorkspaceTrustDialog(sessionName); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}

	if got := readRecordedKeys(t, keysPath); got != "" {
		t.Errorf("recorded keys = %q, want none: stale dialog text is not a dialog", got)
	}
}

// TestAcceptWorkspaceTrustDialog_StaleOptionsIgnorePrompt covers the same stale
// pane carrying a *parseable* option list, which is what a fully rendered and
// then dismissed dialog leaves behind. The list parses, so the stale state has to
// be caught before navigation or the function sends Down+Enter into the agent's
// live composer (gt-sd1o).
func TestAcceptWorkspaceTrustDialog_StaleOptionsIgnorePrompt(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-trust-stale-options-" + t.Name()
	keysPath := startTrustDialogSession(t, tm, sessionName,
		claudeTrustDialogCancelFirst, trustFixtureOptions{promptFirst: true})

	if err := tm.AcceptWorkspaceTrustDialog(sessionName); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}

	if got := readRecordedKeys(t, keysPath); got != "" {
		t.Errorf("recorded keys = %q, want none: a stale option list is not a dialog to answer", got)
	}
}
