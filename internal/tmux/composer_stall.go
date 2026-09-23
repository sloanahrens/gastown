package tmux

import (
	"errors"
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
// and 254s). So a stall verdict here requires unsubmitted input AND a clock that
// has run out on it.
//
// The silence window alone is too weak a guard (gt-afa7): #{window_activity}
// advances on ANY pane write, including each nudge typed into the composer by
// the patrols trying to wake the session, so the one signal that could answer
// "how long has this input been waiting?" is reset by the input itself. The
// second clock is the pending-input age (PendingInputClock), which lives outside
// the pane. Because an age alone cannot separate a wedged composer from a
// working agent carrying a queued nudge (gt-hkhu), that clock only counts a run
// it has EARNED: several observations spanning a minimum window, in the same tmux
// session, with the pane's transcript region byte-identical throughout
// (PaneProgressSignature). An agent that did anything at all during the window
// restarts the clock rather than tripping it.

// ErrComposerUnobservable means the pane was captured but could not be
// classified: no prompt prefix to reason about, or no composer line in the
// capture. It is an error rather than a zero-valued verdict because callers
// read a nil error with Stalled false as a clean bill of health (gt-afa7).
var ErrComposerUnobservable = errors.New("composer state could not be classified")

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
	// PendingFor is how long the composer has been continuously observed
	// holding unsubmitted input, as reported by the caller's PendingInputClock.
	// It is non-zero whenever an earlier probe started the run — including when
	// the silence window is what tripped the verdict, and when both clocks ran
	// out. It is zero only on the first observation of a run, and when no clock
	// was supplied (see DetectComposerStallTracked). Use it for logs, not to
	// tell which clock fired: Evidence names that in prose.
	PendingFor time.Duration
	// PendingSamples is how many consecutive observations have seen the
	// composer pending. A run needs several before its age may be acted on, so
	// a large PendingFor under a small PendingSamples means the clock was
	// restarted mid-run by progress (gt-afa7).
	PendingSamples int
	// FrozenFor is the threshold Inactivity and PendingFor were compared
	// against.
	FrozenFor time.Duration
	// Stalled is the verdict: input is pending AND a clock has run out on it —
	// no pane output for FrozenFor, or a continuous run of the input itself
	// observed waiting for FrozenFor. Either one means nothing is coming to
	// consume it (gt-afa7).
	Stalled bool
	// Evidence is a short human-readable justification for the verdict.
	Evidence string
}

// DetectComposerStall reports whether a session is holding unsubmitted input
// while producing no output at all.
//
// frozenFor is the threshold both clocks are measured against: how long the
// session must produce no pane output, or how long the input must be observed
// waiting unattended, before a pending composer counts as stalled.
//
// Only unsubmitted input counts. An agent that is simply idle at the prompt
// with a clean composer is healthy and is never reported: the check returns
// early whenever the composer holds nothing.
//
// A busy indicator in the pane does NOT suppress the detection if the session
// has been silent for the full frozen window. A stale busy indicator (from a
// previous turn that has ended) can linger in the capture and would otherwise
// blind the detector to a real stall (gt-ncon). The activity check is the
// authoritative signal: if the session has produced no output for frozenFor,
// the busy indicator is stale and the pending input is stranded.
//
// An unclassifiable pane returns ErrComposerUnobservable rather than a
// zero-valued verdict, so a caller cannot read "I could not tell" as "healthy"
// (gt-afa7).
//
// This form has no pending clock, so a stall requires the silence window.
// Callers that can persist per-session state use DetectComposerStallTracked.
func (t *Tmux) DetectComposerStall(session string, frozenFor time.Duration) (ComposerStall, error) {
	return t.DetectComposerStallTracked(session, frozenFor, nil)
}

// DetectComposerStallTracked is DetectComposerStall with a PendingInputClock:
// it reports a stall when input is pending and either the session has been
// silent for frozenFor or the clock reports the input has been waiting that
// long. A nil clock leaves only the silence window.
//
// The age clock short-circuits a signal that the thing being detected can
// reset: #{window_activity} advances on any pane write, including every nudge
// typed into the stalled composer, so a session collecting nudges from an
// anxious patrol never accumulates enough silence to be noticed (gt-afa7).
//
// The age clock is gated on evidence beyond "input was seen twice". It only
// acts on a run whose samples are consecutive and span a minimum window, are
// keyed to this tmux session, and kept the pane's transcript region
// byte-identical throughout (see PendingInputClock and
// PaneProgressSignature). An agent that did any work during the window — a
// working or idle-await one carrying a legitimately queued nudge — advances
// that region and restarts the clock, which is what keeps the gt-hkhu false
// positive from coming back.
func (t *Tmux) DetectComposerStallTracked(session string, frozenFor time.Duration, clock *PendingInputClock) (ComposerStall, error) {
	result := ComposerStall{Session: session, FrozenFor: frozenFor}

	// -e is required: the dim attribute distinguishes typed input from
	// placeholder text (see analyzeComposerState). -S -25 matches the capture
	// depth submit_verify.go uses; the composer, the queued-message list above
	// it and the footer below it all sit in that window.
	content, err := t.run("capture-pane", "-p", "-e", "-t", session, "-S", "-25")
	if err != nil {
		return result, fmt.Errorf("capturing pane for %q: %w", session, err)
	}

	promptPrefix := readyPromptPrefixForSession(t, session)
	probe := analyzeComposerState(content, promptPrefix)
	result.State = probe.State
	result.Queued = probe.Queued
	result.Evidence = probe.Evidence

	// The pane was read but says nothing we can classify, and an unreadable
	// composer must not read as a healthy one (gt-afa7).
	if probe.State == ComposerUnknown {
		return result, fmt.Errorf("%w for %q: %s", ErrComposerUnobservable, session, probe.Evidence)
	}

	// If the pane is busy, check the activity to distinguish a genuinely
	// working agent from a stalled one with a stale busy indicator (gt-ncon).
	// A stale busy indicator can linger in the pane capture after a turn ends,
	// and would otherwise blind the detector to a real stall.
	if probe.State == ComposerBusy {
		// The age clock is not consulted here: a long-running turn holds
		// queued input past any useful threshold, so an age alone cannot
		// separate that from a stale indicator (gt-cyyg). End the run — the
		// running turn owns whatever is queued.
		clock.Reset(session)
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
			reprobe := analyzeComposerStateNoBusy(content, promptPrefix)
			if reprobe.State == ComposerPending {
				result.State = ComposerPending
				result.Queued = reprobe.Queued
				result.Evidence = "stale busy indicator masking pending input (gt-ncon)"
				result.Stalled = true
			}
		}
		return result, nil
	}

	// If the pane is not pending, there's nothing to detect. End any pending
	// run so a later wait starts its own clock.
	if probe.State != ComposerPending {
		clock.Reset(session)
		return result, nil
	}

	// Input is waiting and no turn is running (the busy check above returned
	// early), so nothing is coming to consume it. Either clock running out is
	// the stall.
	//
	// The age clock is fed the pane's transcript-region digest and the tmux
	// session id, so it can tell a run that has been genuinely unattended from
	// one where the agent kept working — or the session died and a fresh one
	// took its name (gt-afa7).
	wait := clock.Observe(session, t.sessionID(session), PaneProgressSignature(content, promptPrefix), time.Now())
	result.PendingFor = wait.Waiting
	result.PendingSamples = wait.Samples

	activity, err := t.GetWindowActivity(session)
	if err != nil {
		return result, fmt.Errorf("reading activity for %q: %w", session, err)
	}
	result.Inactivity = time.Since(activity)

	frozen := frozenFor > 0 && result.Inactivity >= frozenFor
	waiting := frozenFor > 0 && wait.Continuous && result.PendingFor >= frozenFor
	result.Stalled = frozen || waiting
	switch {
	case frozen && waiting:
		result.Evidence = fmt.Sprintf("%s (no pane output for %s, input waiting %s)",
			probe.Evidence, result.Inactivity.Round(time.Second), result.PendingFor.Round(time.Second))
	case waiting:
		// The pane is still being written to, so the silence clock cannot run
		// out — the age is the only evidence there is.
		result.Evidence = fmt.Sprintf("%s (input waiting %s; pane still repainting, so the silence clock cannot run out)",
			probe.Evidence, result.PendingFor.Round(time.Second))
	case frozen:
		// The pane has gone quiet, so the silence clock is the evidence.
		result.Evidence = fmt.Sprintf("%s (no pane output for %s)", probe.Evidence, result.Inactivity.Round(time.Second))
	}
	return result, nil
}

// sessionID resolves a session's tmux id, or "" when it cannot be read.
//
// An unreadable identity is not an error for the stall check: it only costs the
// age clock this observation, leaving the silence window, which is the
// conservative direction — an unidentified run has no age to act on. In
// practice this cannot silently disable the age clock for long: the read goes
// through the same display-message call as GetWindowActivity, so a tmux that
// cannot answer this one has already failed the activity read above and
// returned an error.
func (t *Tmux) sessionID(session string) string {
	id, err := t.SessionID(session)
	if err != nil {
		return ""
	}
	return id
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
//
// A strand also has to be DATED by content, not merely inferred from a pane that
// did not move (gt-7xnv). NotConsumed claims the session did no work during the
// window, and a capture is evidence of that only when it has non-volatile
// content whose recency it can testify to — the gt-xb27 rule that "no output" is
// defined by content, never by a pane counter. PaneProgressSignature is that
// evidence: it digests the pane's transcript region with spinner chrome
// stripped, and returns "" when there is no such region to hash. An undated pane
// therefore reads Inconclusive, as AssessStall reads an unavailable pane
// signature: insufficient evidence, not a verdict.
//
// What this probe cannot do is separate a slow start from a wedge by time alone:
// three seconds of a frozen pending pane is identical in both cases. The counter
// that would answer it, #{window_activity}, is the one the delivery itself
// resets (gt-afa7, see DetectComposerStallTracked), so requiring it here would
// silently disable the report rather than sharpen it. The time-based claim stays
// with the minutes-scale probe above, on the out-of-pane PendingInputClock.
// Before tightening this further, read gt-oxzf8: it records both the residual
// and the clock-gated design that closes it.

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
	// while still holding unsubmitted input, over a window the pane's own
	// content dates (see consumptionVerdict). That input will not be acted on
	// without intervention — this is the wedged state. It remains an
	// observation over a seconds-scale window, not a stall verdict: a target
	// that is merely slow to start looks the same this early.
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
	if analyzeComposerState(current, promptPrefix).State != ComposerPending {
		return InputConsumptionInconclusive
	}

	// The input is still held and the pane did not move. Before calling that a
	// strand, the pane has to be able to date itself: a capture with nothing
	// non-volatile above the input box cannot testify that the session did no
	// work, only that nothing it did landed in this capture (gt-7xnv). An
	// undated pane is insufficient evidence, not a verdict — the same rule
	// AssessStall applies to an empty pane signature.
	if PaneProgressSignature(current, promptPrefix) == "" {
		return InputConsumptionInconclusive
	}
	return InputConsumptionNotConsumed
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
//
// Fail closed on the error (gt-7xnv): a pane that could not be read says nothing
// about whether the input was consumed, so a caller must not turn this error
// into silence — silence is what a consumed nudge looks like. It is not a wedge
// claim in the other direction either. The verdict is Unknown and the error
// names what failed; the caller's job is to report the unknown and re-probe,
// never to act on it as if the session were alive.
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
