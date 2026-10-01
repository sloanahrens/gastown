package tmux

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// bypassDialog is the bypass-permissions warning with its exit option focused.
const bypassDialog = ` WARNING: Claude Code running in Bypass Permissions mode

 ❯ 1. No, exit
   2. Yes, I accept

 Enter to confirm · Esc to cancel`

// runDialog runs f against pane on a fake clock, driving it through any poll.
func runDialog(t *testing.T, pane *fakePane, f func(tm *Tmux) error) (*scripted, error) {
	t.Helper()
	s := newScripted(pane.answer)
	clk := newFixedClock()
	done := make(chan error, 1)
	go func() { done <- f(unitTmux(s, clk)) }()
	return s, driveClock(t, clk, constants.DialogPollInterval, done)
}

// TestAcceptWorkspaceTrustDialog_NoDialog verifies that when no trust dialog
// is present (agent prompt visible), the function returns on its first
// capture without waiting on the clock or pressing anything.
func TestAcceptWorkspaceTrustDialog_NoDialog(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: "user@host:~$ "}
	s := newScripted(pane.answer)
	if err := unitTmux(s, nil).AcceptWorkspaceTrustDialog("gt-x"); err != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", err)
	}
	if n := len(s.find("capture-pane")); n != 1 {
		t.Errorf("capture-pane calls = %d, want 1 (early exit on the prompt)", n)
	}
	if got := pane.sentKeys(); len(got) != 0 {
		t.Errorf("keys = %q, want none", got)
	}
}

// TestAcceptBypassPermissionsWarning_NoDialog verifies that when no bypass
// permissions dialog is present, the function returns on its first capture.
func TestAcceptBypassPermissionsWarning_NoDialog(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: "user@host:~$ "}
	s := newScripted(pane.answer)
	if err := unitTmux(s, nil).AcceptBypassPermissionsWarning("gt-x"); err != nil {
		t.Fatalf("AcceptBypassPermissionsWarning: %v", err)
	}
	if n := len(s.find("capture-pane")); n != 1 {
		t.Errorf("capture-pane calls = %d, want 1", n)
	}
}

// TestAcceptBypassPermissionsWarning_DetectsDialog verifies that the bypass
// dialog is answered by moving onto "Yes, I accept" and confirming.
func TestAcceptBypassPermissionsWarning_DetectsDialog(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: bypassDialog, confirm: showPrompt}
	_, err := runDialog(t, pane, func(tm *Tmux) error { return tm.AcceptBypassPermissionsWarning("gt-x") })
	if err != nil {
		t.Fatalf("AcceptBypassPermissionsWarning: %v", err)
	}
	if got := pane.sentKeys(); !reflect.DeepEqual(got, []string{"Down", "Enter"}) {
		t.Errorf("keys = %q, want Down then Enter", got)
	}
}

// TestAcceptBypassPermissionsWarning_UnreadableFallsBackToDownEnter pins the
// documented fallback: an unreadable bypass dialog still gets Down+Enter.
func TestAcceptBypassPermissionsWarning_UnreadableFallsBackToDownEnter(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: "Bypass Permissions mode is enabled", confirm: showPrompt}
	_, err := runDialog(t, pane, func(tm *Tmux) error { return tm.AcceptBypassPermissionsWarning("gt-x") })
	if err != nil {
		t.Fatalf("AcceptBypassPermissionsWarning: %v", err)
	}
	if got := pane.sentKeys(); !reflect.DeepEqual(got, []string{"Down", "Enter"}) {
		t.Errorf("keys = %q, want Down then Enter", got)
	}
}

// TestAcceptBypassPermissionsWarning_StaleDialogTextIsNotAnswered is the state
// the phrase match alone gets wrong: the workspace-trust warning carries
// "Bypass Permissions mode" in its prose, so a dismissed trust dialog left
// above a live prompt reaches this function, and its stale option list still
// parses — Down+Enter would be typed into the agent's composer with nothing
// reporting it (gt-g1f9s).
func TestAcceptBypassPermissionsWarning_StaleDialogTextIsNotAnswered(t *testing.T) {
	t.Parallel()

	staleDialogs := map[string]string{
		"trust dialog prose": claudeTrustDialogWarning,
		"bypass dialog":      bypassPermissionsDialog,
	}
	for name, dialog := range staleDialogs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pane := &fakePane{content: dialog + "\n\n❯ "}
			// Driven through the poll window: an unfixed build answers the
			// stale option list and only then waits on verifyDialogDismissed,
			// so a direct call would hang instead of failing.
			_, err := runDialog(t, pane, func(tm *Tmux) error { return tm.AcceptBypassPermissionsWarning("gt-x") })
			if err != nil {
				t.Fatalf("AcceptBypassPermissionsWarning: %v", err)
			}
			if got := pane.sentKeys(); len(got) != 0 {
				t.Errorf("keys = %q, want none: the dialog text above the prompt is stale", got)
			}
		})
	}
}

// TestAcceptBypassPermissionsWarning_StaleTextWithoutAgentPrompt covers the
// same scrollback with no agent prompt to exit on: the stale check has to rule
// the text out on its own, and the shell prompt below it is that check's
// signal, not the agent-prompt early exit's (gt-g1f9s).
func TestAcceptBypassPermissionsWarning_StaleTextWithoutAgentPrompt(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: "Bypass Permissions mode\n1. No\n2. Yes, I accept\nuser@host:~$"}
	_, err := runDialog(t, pane, func(tm *Tmux) error { return tm.AcceptBypassPermissionsWarning("gt-x") })
	if err != nil {
		t.Fatalf("AcceptBypassPermissionsWarning: %v", err)
	}
	if got := pane.sentKeys(); len(got) != 0 {
		t.Errorf("keys = %q, want none: the dialog text above the shell prompt is stale", got)
	}
}

// TestAcceptBypassPermissionsWarning_AnswersLiveDialogUnderOldPrompt keeps the
// stale check narrow: a prompt line *above* the modal is scrollback, not a live
// prompt, so the dialog on screen is still answered (gt-g1f9s).
func TestAcceptBypassPermissionsWarning_AnswersLiveDialogUnderOldPrompt(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: "user@host:~$\n" + bypassDialog, confirm: showPrompt}
	_, err := runDialog(t, pane, func(tm *Tmux) error { return tm.AcceptBypassPermissionsWarning("gt-x") })
	if err != nil {
		t.Fatalf("AcceptBypassPermissionsWarning: %v", err)
	}
	if got := pane.sentKeys(); !reflect.DeepEqual(got, []string{"Down", "Enter"}) {
		t.Errorf("keys = %q, want Down then Enter", got)
	}
}

// TestAcceptStartupDialogs_NoDialogs verifies the combined function returns
// on first captures when no dialogs are present.
func TestAcceptStartupDialogs_NoDialogs(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: "user@host:~$ "}
	s := newScripted(pane.answer)
	if err := unitTmux(s, nil).AcceptStartupDialogs("gt-x"); err != nil {
		t.Fatalf("AcceptStartupDialogs: %v", err)
	}
	if got := pane.sentKeys(); len(got) != 0 {
		t.Errorf("keys = %q, want none", got)
	}
}

// TestAcceptWorkspaceTrustDialog_InvalidSession: capture errors are retried
// for the whole poll window, then the function returns nil and leaves the
// failure for CheckStartupBlocked to report.
func TestAcceptWorkspaceTrustDialog_InvalidSession(t *testing.T) {
	t.Parallel()
	pane := &fakePane{dead: true}
	s, err := runDialog(t, pane, func(tm *Tmux) error { return tm.AcceptWorkspaceTrustDialog("gt-x") })
	if err != nil {
		t.Fatalf("expected nil error for nonexistent session, got: %v", err)
	}
	if n := len(s.find("capture-pane")); n < 2 {
		t.Errorf("capture-pane calls = %d, want retries across the poll window", n)
	}
}

// TestCheckStartupBlocked reports a dialog that outlives the poll window and
// passes a pane with none.
func TestCheckStartupBlocked(t *testing.T) {
	t.Parallel()
	_, err := runDialog(t, &fakePane{content: bypassDialog}, func(tm *Tmux) error { return tm.CheckStartupBlocked("gt-x") })
	if err == nil || !strings.Contains(err.Error(), "bypass permissions prompt") {
		t.Fatalf("CheckStartupBlocked(dialog) = %v, want it named", err)
	}
	_, err = runDialog(t, &fakePane{content: "❯ "}, func(tm *Tmux) error { return tm.CheckStartupBlocked("gt-x") })
	if err != nil {
		t.Fatalf("CheckStartupBlocked(prompt) = %v, want nil", err)
	}
	_, err = runDialog(t, &fakePane{dead: true}, func(tm *Tmux) error { return tm.CheckStartupBlocked("gt-x") })
	if err == nil {
		t.Fatal("CheckStartupBlocked(dead pane) = nil, want the capture error")
	}
}

// TestContainsPromptIndicator verifies the prompt detection helper
// recognizes various shell and agent prompt patterns.
func TestContainsPromptIndicator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"claude prompt", "Hello! How can I help?\n>", true},
		{"bash prompt", "user@host:~$", true},
		{"zsh prompt", "╰─❯", true},
		{"root prompt", "root@host:~#", true},
		{"csh prompt", "host%", true},
		{"dialog text only", "Quick safety check\nDo you trust this folder?", false},
		{"empty", "", false},
		{"whitespace only", "   \n  \n  ", false},
		{"bypass dialog", "Bypass Permissions mode\n1. No\n2. Yes, I accept", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsPromptIndicator(tt.content)
			if got != tt.want {
				t.Errorf("containsPromptIndicator(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestContainsWorkspaceTrustDialog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"claude trust prompt", "Quick safety check\nDo you trust this folder?", true},
		{"bypass dialog", "Bypass Permissions mode\n1. No\n2. Yes, I accept", false},
		{"shell prompt", "user@host:~$", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsWorkspaceTrustDialog(tt.content)
			if got != tt.want {
				t.Errorf("containsWorkspaceTrustDialog(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestContainsBlockingStartupDialog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		content     string
		wantBlocked bool
		wantName    string
	}{
		{
			name:        "bypass modal",
			content:     "Bypass Permissions mode\n1. No\n2. Yes, I accept",
			wantBlocked: true,
			wantName:    "bypass permissions prompt",
		},
		{
			name:        "ready prompt",
			content:     "❯ ",
			wantBlocked: false,
		},
		{
			name: "stale bypass dialog before claude prompt",
			content: `Bypass Permissions mode
1. No
2. Yes, I accept
❯ `,
			wantBlocked: false,
		},
		{
			name: "stale bypass dialog before prompt and status",
			content: `Bypass Permissions mode
1. No
2. Yes, I accept
❯
session ready`,
			wantBlocked: false,
		},
		{
			name: "stale trust dialog before shell prompt",
			content: `Quick safety check
Do you trust this folder?
user@host:~$`,
			wantBlocked: false,
		},
		{
			name: "old shell prompt before current bypass dialog",
			content: `user@host:~$
Bypass Permissions mode
1. No
2. Yes, I accept`,
			wantBlocked: true,
			wantName:    "bypass permissions prompt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotBlocked := containsBlockingStartupDialog(tt.content)
			if gotBlocked != tt.wantBlocked {
				t.Fatalf("blocked = %v, want %v", gotBlocked, tt.wantBlocked)
			}
			if gotName != tt.wantName {
				t.Fatalf("name = %q, want %q", gotName, tt.wantName)
			}
		})
	}
}

// TestDismissStartupDialogsBlind_SendsKeys verifies that with no trust dialog
// on screen the blind dismiss sends only the bypass sequence, Down then Enter.
func TestDismissStartupDialogsBlind_SendsKeys(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: "❯ "}
	_, err := runDialog(t, pane, func(tm *Tmux) error { return tm.DismissStartupDialogsBlind("gt-x") })
	if err != nil {
		t.Fatalf("DismissStartupDialogsBlind: %v", err)
	}
	if got := pane.sentKeys(); !reflect.DeepEqual(got, []string{"Down", "Enter"}) {
		t.Errorf("keys = %q, want Down then Enter", got)
	}
}

// TestDismissStartupDialogsBlind_AnswersTrustDialogFirst: a visible trust
// dialog is read and answered before the blind bypass keys (gt-nc1t).
func TestDismissStartupDialogsBlind_AnswersTrustDialogFirst(t *testing.T) {
	t.Parallel()
	pane := &fakePane{content: claudeTrustDialogCancelFirst, confirm: showPrompt}
	_, err := runDialog(t, pane, func(tm *Tmux) error { return tm.DismissStartupDialogsBlind("gt-x") })
	if err != nil {
		t.Fatalf("DismissStartupDialogsBlind: %v", err)
	}
	want := []string{"Down", "Enter", "Down", "Enter"}
	if got := pane.sentKeys(); !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %q, want %q", got, want)
	}
}

// TestDismissStartupDialogsBlind_InvalidSession verifies error handling
// when the session doesn't exist.
func TestDismissStartupDialogsBlind_InvalidSession(t *testing.T) {
	t.Parallel()
	s := newScripted((&fakePane{dead: true}).answer)
	if err := unitTmux(s, nil).DismissStartupDialogsBlind("gt-x"); err == nil {
		t.Error("expected error for nonexistent session, got nil")
	}
}

func TestContainsBlockingQuestionDialog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		content      string
		wantBlocked  bool
		wantQuestion string
	}{
		{
			name: "degenerate placeholder question with lettered options",
			content: `Which approach should we take?
❯ a. Option A
  b. Option B

Enter to select · Esc to cancel`,
			wantBlocked:  true,
			wantQuestion: "Which approach should we take?",
		},
		{
			name: "numbered options",
			content: `Proceed with deployment?
1. Yes, deploy now
2. No, cancel

(Enter to confirm, Esc to cancel)`,
			wantBlocked:  true,
			wantQuestion: "Proceed with deployment?",
		},
		{
			name:        "ready prompt, no dialog",
			content:     "> ",
			wantBlocked: false,
		},
		{
			name: "hint text without an option list is not enough",
			content: `I'll press Enter to select the next step, then Esc to cancel if needed.
Continuing with the plan.`,
			wantBlocked: false,
		},
		{
			name: "option-list-shaped lines without the select hint",
			content: `Here are two options:
1. Refactor the module
2. Leave it as-is
`,
			wantBlocked: false,
		},
		{
			name: "rewind mode is handled separately, not as a question dialog",
			content: `Rewind — browse conversation history
❯ 1. Restore to here
  2. Cancel

Enter to select · Esc to cancel`,
			wantBlocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotQuestion, gotBlocked := containsBlockingQuestionDialog(tt.content)
			if gotBlocked != tt.wantBlocked {
				t.Fatalf("blocked = %v, want %v", gotBlocked, tt.wantBlocked)
			}
			if tt.wantBlocked && gotQuestion != tt.wantQuestion {
				t.Fatalf("question = %q, want %q", gotQuestion, tt.wantQuestion)
			}
		})
	}
}

// TestDismissBlockingQuestionDialog_SendsEscape verifies that dismissing a
// blocking question dialog sends a single Escape keystroke without polling
// or screen-scraping (gt-z83).
func TestDismissBlockingQuestionDialog_SendsEscape(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	if err := unitTmux(s, nil).DismissBlockingQuestionDialog("gt-x"); err != nil {
		t.Fatalf("DismissBlockingQuestionDialog: %v", err)
	}
	got := s.all()
	if len(got) != 1 || !got[0].has("send-keys", "-t", "gt-x", "Escape") {
		t.Fatalf("calls = %v, want one send-keys Escape", got)
	}
}

// TestDismissBlockingQuestionDialog_InvalidSession verifies error handling
// when the session doesn't exist.
func TestDismissBlockingQuestionDialog_InvalidSession(t *testing.T) {
	t.Parallel()
	// tmux 3.7c: send-keys to a missing session is "can't find pane".
	s := newScripted(bySub(map[string]reply{"send-keys": fail("can't find pane: gt-x")}))
	if err := unitTmux(s, nil).DismissBlockingQuestionDialog("gt-x"); !errors.Is(err, ErrPaneNotFound) {
		t.Errorf("err = %v, want ErrPaneNotFound", err)
	}
}
