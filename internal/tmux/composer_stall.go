package tmux

import (
	"fmt"
	"strings"
	"time"
)

// Detection of a composed-but-unsubmitted instruction (gt-hkhu).
//
// A Claude Code pane can hold input the agent will never act on: text typed
// into the composer, or messages sitting in Claude Code's input queue with the
// "ctrl+x ctrl+s to send now" footer below the prompt. When that happens while
// the session is otherwise producing no output, the agent is idle forever and
// every process-level liveness check still reports it as running — the session
// exists and the agent process is alive. Both gastown and om refineries idled
// 70-100 minutes in exactly this state (gt-hkhu).
//
// Pane shape alone is NOT sufficient evidence. An idle-await refinery between
// cycles legitimately shows a non-empty input box with the send-now footer: the
// queued nudge queue UI, from nudges that were typed while it was mid-turn and
// are still waiting their turn. The witness flagged exactly that as wedged on
// 2026-09-18 16:13, and the refinery had been sending mail normally all cycle.
// Neither is the "submit not verified: message stranded in composer" error from
// gt nudge a discriminator: it fires against busy and idle-await targets whose
// message is merely queueing, and every nudge the witness sent that night
// reporting that error was in fact received.
//
// What separates the two cases is progress. A working agent repaints its pane
// constantly (measured live 2026-09-18: every actively-working session showed
// #{window_activity} age 0s, while idle-await sessions went quiet for 19s, 38s
// and 254s). So a stall verdict here requires BOTH signals: unsubmitted input
// present AND no pane output at all for the whole threshold window.

// ComposerState classifies what a Claude Code pane's input box holds.
type ComposerState int

const (
	// ComposerUnknown means the pane could not be classified — the capture
	// failed, the agent has no prompt prefix, or no composer line was found.
	// Callers must not treat this as either healthy or stalled.
	ComposerUnknown ComposerState = iota
	// ComposerClean means the composer holds no unsubmitted input.
	ComposerClean
	// ComposerBusy means the agent is actively working. Whatever the composer
	// or queue holds is not stranded: the running turn will consume it.
	ComposerBusy
	// ComposerPending means input is sitting in the composer or in Claude
	// Code's queued-message list, unsubmitted.
	ComposerPending
)

// String returns a human-readable label for the composer state.
func (c ComposerState) String() string {
	switch c {
	case ComposerClean:
		return "clean"
	case ComposerBusy:
		return "busy"
	case ComposerPending:
		return "pending"
	default:
		return "unknown"
	}
}

// queuedMessagesIndicators are the literal substrings Claude Code's TUI renders
// in the footer while messages sit in its input queue, e.g.
//
//	Press up to edit queued messages · ctrl+x ctrl+s to send now
//
// FRAGILITY: like busyIndicators (see hasBusyIndicator), this couples to
// upstream TUI text and cannot survive a silent rename on its own. It is a
// SUPPLEMENT to the composer-content check, not a replacement: input typed
// straight into an idle composer is caught structurally by composerContent
// without any string matching. A rename therefore degrades this detector to
// "typed text only" rather than breaking it outright, and
// TestHasQueuedMessagesFooter pins the known wording so a change is
// intentional rather than accidental.
var queuedMessagesIndicators = []string{
	"queued messages",
	"to send now",
}

func hasQueuedMessagesFooter(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	for _, marker := range queuedMessagesIndicators {
		if strings.Contains(trimmed, marker) {
			return true
		}
	}
	return false
}

// composerProbe is the result of classifying a pane snapshot.
type composerProbe struct {
	State  ComposerState
	Queued bool
	// Evidence is a short human-readable justification, for logs and scan
	// output. It is descriptive only — no caller may parse it.
	Evidence string
}

// analyzeComposerState classifies a raw `capture-pane -e` snapshot. It takes
// the ANSI-carrying content, not plain text, because the dim (SGR 2) attribute
// is what separates real typed input from placeholder text the TUI renders in
// the input box — the same technique submit_verify.go uses to tell a cleared
// composer from a stranded one.
func analyzeComposerState(escContent, promptPrefix string) composerProbe {
	if strings.TrimSpace(promptPrefix) == "" {
		return composerProbe{State: ComposerUnknown, Evidence: "no prompt prefix for this agent"}
	}

	plain, dim := stripAnsiTrackDim(escContent)
	lines, lineDims := splitRunesAndDim(plain, dim)

	// Busy wins over everything: a running turn owns whatever is queued.
	for _, line := range lines {
		if hasBusyIndicator(string(line)) {
			return composerProbe{State: ComposerBusy, Evidence: "busy indicator in pane"}
		}
	}

	// The queued-message footer renders below the composer, so it can be
	// visible while the composer line itself is empty.
	for _, line := range lines {
		if hasQueuedMessagesFooter(string(line)) {
			return composerProbe{
				State:    ComposerPending,
				Queued:   true,
				Evidence: "queued-messages footer visible",
			}
		}
	}

	// Walk up to the last composer line in the capture and read what follows
	// the prompt prefix.
	for i := len(lines) - 1; i >= 0; i-- {
		if !matchesPromptPrefix(string(lines[i]), promptPrefix) {
			continue
		}
		content, contentDim := composerContent(lines[i], lineDims[i], promptPrefix)
		if len(content) == 0 || allDim(contentDim) {
			return composerProbe{State: ComposerClean, Evidence: "composer empty"}
		}
		return composerProbe{
			State:    ComposerPending,
			Evidence: fmt.Sprintf("composer holds unsubmitted text: %q", truncateRunes(string(content), 60)),
		}
	}

	return composerProbe{State: ComposerUnknown, Evidence: "no composer line in capture"}
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// ComposerStall reports what a session's composer holds and, when input is
// pending, how long the session has gone without producing any output.
type ComposerStall struct {
	Session string
	// State is the composer classification (see ComposerState).
	State ComposerState
	// Queued is true when the pending input is in Claude Code's input queue
	// (send-now footer visible) rather than typed into the composer.
	Queued bool
	// Inactivity is how long since the session last produced pane output.
	// Zero when the composer is not pending (nothing was measured).
	Inactivity time.Duration
	// FrozenFor is the threshold Inactivity was compared against.
	FrozenFor time.Duration
	// Stalled is the verdict: input is pending AND the session has produced
	// no output at all for FrozenFor.
	Stalled bool
	// Evidence is a short human-readable justification for the verdict.
	Evidence string
}

// DetectComposerStall reports whether a session is holding unsubmitted input
// while producing no output at all.
//
// frozenFor is the required silence before a pending composer counts as
// stalled. It is the guard against the known false positive: a working or
// idle-await agent with a legitimately queued nudge still repaints its pane, so
// its activity age stays far below any useful threshold. An agent that has been
// silent for the full window AND has input waiting is not waiting for its turn
// — that input will never be consumed without intervention.
//
// When the composer is not pending, Inactivity is left unmeasured and Stalled
// is false: this check never reports a bare quiet session as stalled.
func (t *Tmux) DetectComposerStall(session string, frozenFor time.Duration) (ComposerStall, error) {
	result := ComposerStall{Session: session, FrozenFor: frozenFor}

	// -e is required: the dim attribute distinguishes typed input from
	// placeholder text (see analyzeComposerState). -S -25 matches the capture
	// depth submit_verify.go uses; the composer, the queued-message list above
	// it and the footer below it all sit in that window.
	content, err := t.run("capture-pane", "-p", "-e", "-t", session, "-S", "-25")
	if err != nil {
		return result, fmt.Errorf("capturing pane for %q: %w", session, err)
	}

	probe := analyzeComposerState(content, readyPromptPrefixForSession(t, session))
	result.State = probe.State
	result.Queued = probe.Queued
	result.Evidence = probe.Evidence
	if probe.State != ComposerPending {
		return result, nil
	}

	activity, err := t.GetWindowActivity(session)
	if err != nil {
		return result, fmt.Errorf("reading activity for %q: %w", session, err)
	}
	result.Inactivity = time.Since(activity)
	result.Stalled = result.Inactivity >= frozenFor
	return result, nil
}

// SubmitPendingInput flushes input a pane is already holding, without typing
// anything new. It is the recovery for a stalled composer, and it assumes the
// caller has already established that the session is stalled — sending these
// keystrokes to a working agent is exactly the interrupt gt-cyyg removed.
//
// queued selects the mechanism, because the two pending states need different
// keystrokes:
//
//   - queued (send-now footer visible): ctrl+x ctrl+s, Claude Code's "send
//     queued messages now". This is the keystroke an operator used to unblock
//     both 2026-09-18 refinery stalls, and it is a no-op when nothing is queued.
//   - composer text: Enter, the ordinary submit.
func (t *Tmux) SubmitPendingInput(target string, queued bool) error {
	key := "Enter"
	if queued {
		key = "C-x C-s"
	}
	if _, err := t.run("send-keys", "-t", target, key); err != nil {
		return fmt.Errorf("submitting pending input to %q: %w", target, err)
	}
	return nil
}
