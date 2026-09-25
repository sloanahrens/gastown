package tmux

import (
	"strings"
	"testing"
)

// claudeTrustDialogCancelFirst is the workspace trust dialog as Claude Code
// 2.1.274 renders it, captured from a pane during the gt-nc1t failure (the
// trailing spaces the capture carries have been trimmed). The TrustDialog
// component mounts its select list with cancelFirst and focus:"cancel", so
// "No, exit" is the focused option and a blind Enter quits Claude.
const claudeTrustDialogCancelFirst = ` Accessing workspace: /Users/sloan/gt/gastown/polecats/onyx/gastown
 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.
 Claude Code'll be able to read, edit, and execute files here.

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel`

// claudeTrustDialogYesFirst is the same dialog with the older option order in
// which the trust option is both first and focused. The navigation logic has to
// handle both, which is the reason it reads the pane instead of pressing Down a
// fixed number of times.
const claudeTrustDialogYesFirst = ` Accessing workspace: /Users/sloan/gt/gastown/polecats/onyx/gastown
 Quick safety check: Is this a project you created or one you trust?

 ❯ Yes, I trust this folder
   No, exit

 Enter to confirm · Esc to cancel`

// claudeTrustDialogWarning is the cancel-first dialog as it renders for a folder
// that pre-approves tool permissions, which is what a Gas Town worktree looks
// like. The warning block contains a line beginning "not ", and the closing
// prose mentions trust: neither may be read as an option.
const claudeTrustDialogWarning = ` Accessing workspace: /Users/sloan/gt/gastown/polecats/onyx/gastown
 Quick safety check: Is this a project you created or one you trust?

 ⚠ This folder pre-approves 3 tool permissions in .claude/settings.json, .mcp.json:
   Bash(npm run test:*), Edit(src/**), WebFetch
   These will apply without asking. Only proceed if you trust this configuration.

 ⚠ In Bypass Permissions mode, Claude Code will not ask for your approval before running
 potentially dangerous commands.
 Security guide

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel`

// codexTrustDialog is Codex's numbered variant, with the trust option focused.
const codexTrustDialog = `> You are in /tmp/demo

  Do you trust the contents of this directory? Working with untrusted contents comes with higher risk of prompt injection.

› 1. Yes, proceed (y)
  2. No, quit (n)`

// bypassPermissionsDialog is Claude's bypass-permissions warning, which uses the
// same select component with the same cancel-first ordering as the trust dialog.
const bypassPermissionsDialog = ` WARNING: Claude Code running in Bypass Permissions mode

 In Bypass Permissions mode, Claude Code will not ask for your approval before running
 potentially dangerous commands.

 By proceeding, you accept all responsibility for actions taken while running in Bypass Permissions mode.

 ❯ No, exit
   Yes, I accept

 Enter to confirm · Esc to cancel`

func TestTrustNavigation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		content     string
		wantKey     string
		wantPresses int
		wantErr     string // substring the error must contain; "" means no error
	}{
		{
			name:        "claude cancel-first focuses No, exit",
			content:     claudeTrustDialogCancelFirst,
			wantKey:     "Down",
			wantPresses: 1,
		},
		{
			name:        "claude legacy order already focuses the trust option",
			content:     claudeTrustDialogYesFirst,
			wantPresses: 0,
		},
		{
			name:        "claude warning block does not add phantom options",
			content:     claudeTrustDialogWarning,
			wantKey:     "Down",
			wantPresses: 1,
		},
		{
			name:        "codex numbered options with trust focused",
			content:     codexTrustDialog,
			wantPresses: 0,
		},
		{
			name: "codex numbered options with cancel focused moves up",
			content: `> You are in /tmp/demo

  Do you trust the contents of this directory?

  1. Yes, proceed (y)
› 2. No, quit (n)`,
			wantKey:     "Up",
			wantPresses: 1,
		},
		{
			name:        "bypass permissions dialog selects Yes, I accept",
			content:     bypassPermissionsDialog,
			wantKey:     "Down",
			wantPresses: 1,
		},
		{
			name:    "dialog text with no option list",
			content: "Quick safety check - do you trust this folder?",
			wantErr: "no selectable options",
		},
		{
			name: "options with no focused cursor",
			content: `Quick safety check: Is this a project you created or one you trust?

   No, exit
   Yes, I trust this folder`,
			wantErr: "no focused option",
		},
		{
			name: "options with no trust option",
			content: `Quick safety check: Is this a project you created or one you trust?

 ❯ No, exit
   No, continue without these permissions`,
			wantErr: "no trust-granting option",
		},
		{
			name: "two focused options in the same run",
			content: `Quick safety check: Is this a project you created or one you trust?

 ❯ No, exit
 ❯ Yes, I trust this folder`,
			wantErr: "multiple focused options",
		},
		{
			name: "wrapped cancel label breaks the run",
			content: `Quick safety check: Is this a project you created or one you trust?

 ❯ No, continue without these
   permissions
   Yes, I trust this folder`,
			wantErr: "no focused option",
		},
		{
			name:    "empty pane",
			content: "",
			wantErr: "no selectable options",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key, presses, err := trustNavigation(tt.content)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("trustNavigation() = (%q, %d, nil), want error containing %q", key, presses, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("trustNavigation() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("trustNavigation() unexpected error: %v", err)
			}
			if key != tt.wantKey || presses != tt.wantPresses {
				t.Errorf("trustNavigation() = (%q, %d), want (%q, %d)", key, presses, tt.wantKey, tt.wantPresses)
			}
		})
	}
}

// TestTrustOptionsUsesLastRun covers the pane carrying option-shaped text of its
// own above a live dialog: scrollback from an earlier dialog, an echoed command
// line, or a command line that itself contained the dialog's text.
func TestTrustOptionsUsesLastRun(t *testing.T) {
	t.Parallel()

	content := strings.Join([]string{
		"$ printf 'Quick safety check'", // an earlier dialog's text, echoed
		" ❯ No, exit",
		"   Yes, I trust this folder",
		"",
		" Accessing workspace: /tmp/live",
		" Quick safety check: Is this a project you created or one you trust?",
		" ❯ No, exit",
		"   Yes, I trust this folder",
	}, "\n")

	opts := trustOptions(content)
	if len(opts) != 2 {
		t.Fatalf("trustOptions() returned %d options (%s), want the 2 of the live dialog", len(opts), describeTrustOptions(opts))
	}
	if !opts[0].selected || opts[0].affirmative {
		t.Errorf("expected the focused option to be the cancel option, got %+v", opts[0])
	}
	if opts[1].selected || !opts[1].affirmative {
		t.Errorf("expected the second option to be the trust option, got %+v", opts[1])
	}

	if key, presses, err := trustNavigation(content); err != nil || key != "Down" || presses != 1 {
		t.Errorf("trustNavigation() = (%q, %d, %v), want (Down, 1, nil)", key, presses, err)
	}
}

// TestTrustOptionsRejectsProse guards the label match: dialog prose wraps, and a
// continuation line that reads "not ask for your approval..." must not be taken
// for a "No, ..." option. A phantom option between the focused option and the
// trust option would move the cursor one line too far.
func TestTrustOptionsRejectsProse(t *testing.T) {
	t.Parallel()

	content := strings.Join([]string{
		" In Bypass Permissions mode, Claude Code will not ask for your approval before running",
		" potentially dangerous commands.",
		" Nothing here is an option either.",
		" ❯ No, exit",
		"   Yes, I accept",
	}, "\n")

	opts := trustOptions(content)
	if len(opts) != 2 {
		t.Fatalf("trustOptions() returned %d options (%s), want 2", len(opts), describeTrustOptions(opts))
	}
	if key, presses, err := trustNavigation(content); err != nil || key != "Down" || presses != 1 {
		t.Errorf("trustNavigation() = (%q, %d, %v), want (Down, 1, nil)", key, presses, err)
	}
}

func TestTrustOptionLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want string // "" means not an option
	}{
		{"❯ No, exit", ""}, // marker is stripped by parseTrustOptionLine, not here
		{"No, exit", "No, exit"},
		{"Yes, I trust this folder", "Yes, I trust this folder"},
		{"1. Yes, proceed (y)", "Yes, proceed (y)"},
		{"2) No, quit (n)", "No, quit (n)"},
		{"Yes please", "Yes please"},
		{"No", "No"},
		{"not ask for your approval", ""},
		{"Nothing to see here", ""},
		{"Enter to confirm · Esc to cancel", ""},
		{"(rule names contain unprintable characters)", ""},
		{"These will apply without asking. Only proceed if you trust this configuration.", ""},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			t.Parallel()
			got, ok := trustOptionLabel(tt.text)
			if tt.want == "" {
				if ok {
					t.Fatalf("trustOptionLabel(%q) = (%q, true), want not an option", tt.text, got)
				}
				return
			}
			if !ok || got != tt.want {
				t.Fatalf("trustOptionLabel(%q) = (%q, %v), want (%q, true)", tt.text, got, ok, tt.want)
			}
		})
	}
}

func TestParseTrustOptionLineCursorMarkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		line         string
		wantSelected bool
		wantOK       bool
	}{
		{"claude cursor", " ❯ No, exit", true, true},
		{"codex cursor", "› 1. Yes, proceed (y)", true, true},
		{"unfocused option", "   Yes, I trust this folder", false, true},
		{"codex banner is not an option", "> You are in /tmp/demo", false, false},
		{"prompt line is not an option", "❯ ", false, false},
		{"claude composer prompt", ">", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opt, ok := parseTrustOptionLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("parseTrustOptionLine(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if ok && opt.selected != tt.wantSelected {
				t.Errorf("parseTrustOptionLine(%q) selected = %v, want %v", tt.line, opt.selected, tt.wantSelected)
			}
		})
	}
}

// TestLiveDialogsAreNotStale guards the order selectTrustDialogOption reads the
// pane in: the stale check runs before the option list is parsed, so a live
// dialog judged stale would go unanswered and fail the spawn. None of these
// render a prompt below their option list (gt-sd1o).
func TestLiveDialogsAreNotStale(t *testing.T) {
	t.Parallel()

	dialogs := map[string]string{
		"claude cancel-first": claudeTrustDialogCancelFirst,
		"claude yes-first":    claudeTrustDialogYesFirst,
		"claude warning":      claudeTrustDialogWarning,
		"codex":               codexTrustDialog,
		"bypass permissions":  bypassPermissionsDialog,
	}

	for name, content := range dialogs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if promptAppearsAfterStartupBlocker(content) {
				t.Error("live dialog read as stale scrollback: it would go unanswered")
			}
		})
	}
}

// TestStaleDialogWithOptionsIsStale is the state the stale check exists for: the
// pane holds a whole dismissed dialog, option list included, above a live prompt.
// The list still parses, so the prompt's position is the only signal that nothing
// on screen is waiting to be answered (gt-sd1o).
func TestStaleDialogWithOptionsIsStale(t *testing.T) {
	t.Parallel()

	content := claudeTrustDialogCancelFirst + "\n\n❯ "
	if !promptAppearsAfterStartupBlocker(content) {
		t.Fatal("dialog text above a live prompt must read as stale")
	}
	if key, presses, err := trustNavigation(content); err != nil || key != "Down" || presses != 1 {
		t.Fatalf("trustNavigation() = (%q, %d, %v), want (Down, 1, nil): the option list still parses, which is why the stale check cannot wait for a parse failure", key, presses, err)
	}
}
