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
//
// A busy indicator in the pane does NOT suppress the detection if the session
// has been silent for the full frozen window. A stale busy indicator (from a
// previous turn that has ended) can linger in the capture and would otherwise
// blind the detector to a real stall (gt-ncon). The activity check is the
// authoritative signal: if the session has produced no output for frozenFor,
// the busy indicator is stale and the pending input is stranded.
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

	// If the pane is busy, check the activity to distinguish a genuinely
	// working agent from a stalled one with a stale busy indicator (gt-ncon).
	// A stale busy indicator can linger in the pane capture after a turn ends,
	// and would otherwise blind the detector to a real stall.
	if probe.State == ComposerBusy {
		activity, err := t.GetWindowActivity(session)
		if err != nil {
			return result, fmt.Errorf("reading activity for %q: %w", session, err)
		}
		result.Inactivity = time.Since(activity)
		// If the session has been silent for the frozen window, the busy
		// indicator is stale and the agent is stalled. Re-probe the pane to
		// see if there's pending input that was hidden by the busy indicator.
		if result.Inactivity >= frozenFor {
			// The pane is silent and shows a busy indicator. This is the
			// gt-ncon signature: a stale busy indicator masking a real stall.
			// Re-classify the pane without the busy check to see if there's
			// pending input.
			reprobe := analyzeComposerStateNoBusy(content, readyPromptPrefixForSession(t, session))
			if reprobe.State == ComposerPending {
				result.State = ComposerPending
				result.Queued = reprobe.Queued
				result.Evidence = "stale busy indicator masking pending input (gt-ncon)"
				result.Stalled = true
			}
		}
		return result, nil
	}

	// If the pane is not pending, there's nothing to detect.
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

// analyzeComposerStateNoBusy is like analyzeComposerState but skips the busy
// indicator check. It is used by DetectComposerStall to re-probe a pane that
// shows a busy indicator but has been silent for the frozen window — the
// signature of a stale busy indicator masking a real stall (gt-ncon).
func analyzeComposerStateNoBusy(escContent, promptPrefix string) composerProbe {
	if strings.TrimSpace(promptPrefix) == "" {
		return composerProbe{State: ComposerUnknown, Evidence: "no prompt prefix for this agent"}
	}

	plain, dim := stripAnsiTrackDim(escContent)
	lines, lineDims := splitRunesAndDim(plain, dim)

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
	keys := []string{"Enter"}
	if queued {
		// tmux send-keys takes each key as its own argument; passing "C-x C-s"
		// as a single argument makes tmux type it as literal text instead of
		// sending the two keystrokes (gt-rbfj).
		keys = []string{"C-x", "C-s"}
	}
	args := append([]string{"send-keys", "-t", target}, keys...)
	if _, err := t.run(args...); err != nil {
		return fmt.Errorf("submitting pending input to %q: %w", target, err)
	}
	return nil
}

// Input-consumption liveness (gt-eigw).
//
// Every other liveness signal in the town answers "is the process alive?".
// None answers "is it consuming what we give it?" — and on 2026-09-09 those
// were different questions. be-refinery sat at the prompt for ~15 minutes with
// wait-idle nudges stranded in Claude Code's input queue: the pane process was
// alive, so the daemon logged "Refinery for beads already running, skipping
// spawn" every four minutes while three MRs, one of them P0, aged unclaimed
// behind it. An Enter keystroke, an immediate-mode nudge and mail all failed to
// start a turn. Only 'gt refinery restart beads' cleared it.
//
// DetectComposerStall (above) answers this over a MINUTES-scale window, which
// is what a patrol can afford: input pending AND no output for the whole frozen
// window. The probe below answers it over the SECONDS-scale window a delivery
// needs — after typing a nudge and pressing Enter, did the target react at all?
//
// The verdict is deliberately four-valued rather than two. The one thing this
// probe must never do is call a healthy target wedged: the previous attempt at
// gt-eigw rejected idle targets and was correctly blocked for it. So a strand is
// claimed only on positive evidence — input still demonstrably sitting in the
// composer or queue at the end of the window. A clean composer that simply
// produced no output is Inconclusive, never NotConsumed: the turn may have
// completed before the first observation.

// InputConsumption classifies whether a session consumed input it was given.
type InputConsumption int

const (
	// InputConsumptionInconclusive means nothing was observed either way: the
	// pane produced no output, but it also holds no unsubmitted input, so the
	// turn may simply have finished before the first observation. Callers must
	// not treat this as a healthy verdict or as a wedged one.
	InputConsumptionInconclusive InputConsumption = iota
	// InputConsumptionUnknown means the pane could not be observed at all —
	// the capture failed, or the agent has no prompt prefix to reason about.
	InputConsumptionUnknown
	// InputConsumptionStartedTurn means the agent is generating: a busy
	// indicator is on screen. The input was consumed.
	InputConsumptionStartedTurn
	// InputConsumptionPaneChanged means the pane produced output that was not
	// there at the baseline. The input was consumed.
	InputConsumptionPaneChanged
	// InputConsumptionNotConsumed means the pane produced no output at all
	// while still holding unsubmitted input. That input will not be acted on
	// without intervention — this is the wedged state.
	InputConsumptionNotConsumed
)

// Consumed reports whether the verdict is positive evidence that the session
// acted on the input. Inconclusive and Unknown are both false: no evidence of
// consumption is not evidence of a strand, and callers must distinguish them
// via NotConsumed.
func (c InputConsumption) Consumed() bool {
	return c == InputConsumptionStartedTurn || c == InputConsumptionPaneChanged
}

func (c InputConsumption) String() string {
	switch c {
	case InputConsumptionStartedTurn:
		return "turn-started"
	case InputConsumptionPaneChanged:
		return "pane-changed"
	case InputConsumptionNotConsumed:
		return "not-consumed"
	case InputConsumptionUnknown:
		return "unknown"
	default:
		return "inconclusive"
	}
}

// inputConsumptionPollInterval is how often the probe re-reads the pane while
// waiting for a reaction. It is a var so tests can shrink it.
var inputConsumptionPollInterval = 250 * time.Millisecond

// consumptionVerdict folds a baseline pane capture and a later one into a
// verdict. Pure, so the classification is testable against captured panes
// without a tmux server.
//
// Order matters: positive evidence wins first, because a working agent
// legitimately holds queued input (the running turn owns it) and a repaint
// routinely lands in the same capture as a still-visible queue footer.
func consumptionVerdict(baseline, current, promptPrefix string) InputConsumption {
	if strings.TrimSpace(promptPrefix) == "" {
		return InputConsumptionUnknown
	}

	// analyzeComposerState already ranks busy above every pending state, so a
	// busy indicator here means a turn is running and owns whatever is queued.
	if analyzeComposerState(current, promptPrefix).State == ComposerBusy {
		return InputConsumptionStartedTurn
	}

	if current != baseline {
		return InputConsumptionPaneChanged
	}

	// Byte-identical capture. Only now is a strand claimable — and only if the
	// input is still demonstrably in the composer or the input queue.
	//
	// FRAGILITY: this reads the capture the nudge text was typed into, so a
	// nudge body that literally contains a busy marker ("esc to interrupt")
	// would read as StartedTurn above. That direction is safe — it reports a
	// healthy session as healthy — but it does mean the probe can be blinded
	// by a coincidence, never triggered by one.
	if analyzeComposerState(current, promptPrefix).State == ComposerPending {
		return InputConsumptionNotConsumed
	}
	return InputConsumptionInconclusive
}

// captureForConsumption takes the pane snapshot the probe reasons about. -e is
// required for the dim attribute analyzeComposerState reads; -S -25 matches the
// depth DetectComposerStall uses, which covers the composer, the queued-message
// list above it and the footer below it.
func (t *Tmux) captureForConsumption(session string) (string, error) {
	content, err := t.run("capture-pane", "-p", "-e", "-t", session, "-S", "-25")
	if err != nil {
		return "", fmt.Errorf("capturing pane for %q: %w", session, err)
	}
	return content, nil
}

// WaitForInputConsumed reports whether a session acted on input it was just
// given, within window. It takes its own baseline capture and then watches for
// a reaction; a session that is already mid-repaint when the baseline is taken
// still counts, because any subsequent change is a reaction.
//
// It returns as soon as there is positive evidence (a turn started, or the pane
// changed) and otherwise waits out the whole window before judging — returning
// NotConsumed early would misreport a target that is merely slow to start.
//
// This cannot hang a caller: window bounds it, and it never blocks on the
// session itself. An error means the pane could not be observed at all; a nil
// error with InputConsumptionInconclusive means the pane was observed and had
// nothing to say either way.
func (t *Tmux) WaitForInputConsumed(session string, window time.Duration) (InputConsumption, error) {
	if strings.TrimSpace(session) == "" {
		return InputConsumptionUnknown, fmt.Errorf("input-consumption probe: no session given")
	}
	promptPrefix := readyPromptPrefixForSession(t, session)
	if strings.TrimSpace(promptPrefix) == "" {
		return InputConsumptionUnknown, fmt.Errorf(
			"input-consumption probe for %q: agent has no prompt prefix, cannot classify", session)
	}

	baseline, err := t.captureForConsumption(session)
	if err != nil {
		return InputConsumptionUnknown, err
	}

	deadline := time.Now().Add(window)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		sleep := inputConsumptionPollInterval
		if remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)

		current, err := t.captureForConsumption(session)
		if err != nil {
			return InputConsumptionUnknown, err
		}
		verdict := consumptionVerdict(baseline, current, promptPrefix)
		if verdict.Consumed() {
			return verdict, nil
		}
	}

	// The window elapsed with no reaction. Re-read once and judge: only a pane
	// that is still holding the input is a strand.
	final, err := t.captureForConsumption(session)
	if err != nil {
		return InputConsumptionUnknown, err
	}
	return consumptionVerdict(baseline, final, promptPrefix), nil
}
