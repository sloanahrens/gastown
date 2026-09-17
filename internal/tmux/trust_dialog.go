package tmux

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Workspace-trust dialogs are select lists, and neither the order of their
// options nor the focused one is stable across agent releases. Claude Code
// 2.1.274 renders Claude's "Quick safety check" dialog with cancel FIRST and
// focused (TrustDialog component: cancelFirst+focus:"cancel", confirmLabel
// "Yes, I trust this folder", cancelLabel "No, exit" -> exit), so pressing Enter
// to "accept the pre-selected option" now quits Claude and kills the pane
// (gt-nc1t). The same component shape is used by the bypass-permissions dialog.
//
// The helpers below read the option list out of a captured pane and decide which
// arrow keys move the cursor onto the trust-granting option, so the choice is
// driven by what is on screen instead of by an assumption about ordering.

// trustCursorMarkers are the glyphs a select list uses to mark the focused
// option. Claude Code renders "❯"; Codex renders "›". ASCII ">" is included
// because it is the conventional marker, but a line is only treated as an option
// if its label reads like one, which keeps non-option lines such as Codex's
// "> You are in /tmp/demo" banner out of the list.
var trustCursorMarkers = []string{"❯", "›", "»", ">"}

// trustOption is one selectable line of a workspace trust dialog.
type trustOption struct {
	// label is the option text with the cursor marker and any list numbering
	// stripped, e.g. "Yes, I trust this folder" or "No, exit".
	label string
	// selected reports that the pane shows this option focused.
	selected bool
	// affirmative reports that choosing this option grants trust. Every other
	// option either exits or continues without the trusted permissions.
	affirmative bool
}

// trustKeyInterval is the pause between arrow keystrokes while moving the
// selection, and before the Enter that confirms it. Sending the keys in one
// burst lets the TUI read them as a single escape sequence and lose the arrow,
// which would confirm whichever option was focused — the exit option (gt-nc1t).
// 250ms matches the delay the bypass dialog has been driven with.
const trustKeyInterval = 250 * time.Millisecond

// trustOptions parses the selectable options out of captured pane content,
// in the order they are rendered. It returns nil when the pane shows no
// recognizable option list.
//
// Only the last contiguous run of option-shaped lines is returned: a live dialog
// is rendered at the bottom of the pane, and text above it can contain
// option-shaped lines of its own (scrollback from an earlier dialog, an echoed
// command line, warning prose).
func trustOptions(content string) []trustOption {
	var opts []trustOption
	prevLine := -1
	for i, line := range strings.Split(content, "\n") {
		opt, ok := parseTrustOptionLine(line)
		if !ok {
			continue
		}
		if prevLine >= 0 && i != prevLine+1 {
			opts = nil // gap: a new run starts
		}
		opts = append(opts, opt)
		prevLine = i
	}
	return opts
}

// parseTrustOptionLine decides whether a single pane line is a trust dialog
// option and, if so, reports which one.
func parseTrustOptionLine(line string) (trustOption, bool) {
	trimmed := strings.TrimSpace(line)
	selected := false
	for _, marker := range trustCursorMarkers {
		rest, found := strings.CutPrefix(trimmed, marker)
		if !found {
			continue
		}
		trimmed = strings.TrimSpace(rest)
		selected = true
		break
	}

	label, ok := trustOptionLabel(trimmed)
	if !ok {
		return trustOption{}, false
	}
	return trustOption{label: label, selected: selected, affirmative: isAffirmativeTrustOption(label)}, true
}

// trustOptionLabel reports whether text reads as a yes/no choice, returning the
// label with any list numbering removed ("1. Yes, proceed (y)" -> "Yes, proceed
// (y)").
//
// The check is on the whole first word, not a prefix: dialog body prose wraps,
// and a continuation line beginning "not ask for your approval..." must not be
// read as a "No, ..." option. A phantom option between the focused one and the
// trust option would send the cursor past it, and overshooting the end of the
// list either wraps onto the exit option or parks on nothing (gt-nc1t).
func trustOptionLabel(text string) (string, bool) {
	label := stripTrustOptionNumber(text)
	switch firstWord(label) {
	case "yes", "no":
		return label, true
	}
	return "", false
}

// firstWord returns the leading run of letters of text, lowercased. Anything
// that is not a letter ends the word, so "" and "(rule names ...)" yield "".
func firstWord(text string) string {
	end := strings.IndexFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r)
	})
	if end < 0 {
		end = len(text)
	}
	return strings.ToLower(text[:end])
}

// stripTrustOptionNumber removes a leading list number ("1. ", "2) ") from an
// option label. Codex numbers its options; Claude Code renders its trust dialogs
// with hideIndexes and does not.
func stripTrustOptionNumber(text string) string {
	digits := 0
	for digits < len(text) && text[digits] >= '0' && text[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits >= len(text) {
		return text
	}
	if text[digits] != '.' && text[digits] != ')' {
		return text
	}
	return strings.TrimSpace(text[digits+1:])
}

// isAffirmativeTrustOption reports whether an option grants trust. Claude labels
// it "Yes, I trust this folder" ("Yes, I trust these settings" for the settings
// variant) and accepts it as "Yes, I accept" in the bypass dialog; Codex labels
// it "Yes, proceed". Every cancel variant starts with "No": "No, exit",
// "No, continue without these permissions", "No, quit".
func isAffirmativeTrustOption(label string) bool {
	return firstWord(label) == "yes"
}

// trustNavigation decides how to move the selection onto the trust-granting
// option. It returns the tmux key to send ("Down" or "Up") and how many times.
// Presses is 0 when the affirmative option is already focused, in which case the
// caller only needs to confirm.
//
// It fails rather than guessing when the pane does not show exactly one focused
// option and one trust-granting option: a wrong keypress here exits the agent and
// destroys the session, which is the failure this logic exists to prevent
// (gt-nc1t).
func trustNavigation(content string) (key string, presses int, err error) {
	opts := trustOptions(content)
	if len(opts) == 0 {
		return "", 0, fmt.Errorf("no selectable options found in pane")
	}

	selected, affirmative := -1, -1
	for i, opt := range opts {
		if opt.selected {
			if selected >= 0 {
				return "", 0, fmt.Errorf("multiple focused options in pane (%q and %q)", opts[selected].label, opt.label)
			}
			selected = i
		}
		if opt.affirmative && affirmative < 0 {
			affirmative = i
		}
	}
	switch {
	case affirmative < 0:
		return "", 0, fmt.Errorf("no trust-granting option among %s", describeTrustOptions(opts))
	case selected < 0:
		return "", 0, fmt.Errorf("no focused option among %s", describeTrustOptions(opts))
	}

	if selected == affirmative {
		return "", 0, nil
	}
	if selected < affirmative {
		return "Down", affirmative - selected, nil
	}
	return "Up", selected - affirmative, nil
}

// describeTrustOptions renders an option list for error messages, marking the
// focused option. The text comes from the live pane, so a failure names the
// dialog copy that the parser did not recognize.
func describeTrustOptions(opts []trustOption) string {
	if len(opts) == 0 {
		return "any option line"
	}
	labels := make([]string, len(opts))
	for i, opt := range opts {
		marker := "  "
		if opt.selected {
			marker = "> "
		}
		labels[i] = marker + fmt.Sprintf("%q", opt.label)
	}
	return "[" + strings.Join(labels, " ") + "]"
}
