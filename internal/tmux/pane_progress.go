package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PaneProgressSignature digests the part of a captured pane that changes when
// its agent does work: the transcript above the input box (gt-afa7).
//
// PaneContentSignature is not usable for this. It hashes the whole
// non-volatile pane, and the input box is not volatile — a nudge typed into a
// wedged composer is ordinary text to it. Every nudge the town sends to wake a
// session up would change that signature and restart the very clock those
// nudges are the evidence for, which is the feedback loop this bead exists to
// break.
//
// This digest stops at the rule that opens the input box, so nothing landing
// below it — the composer's own contents, Claude Code's queued-message list and
// send-now footer, the permission status bar — can move it. Everything the
// agent DOES renders above that rule: assistant prose, tool calls, their
// output. An unchanged digest across a run of observations is therefore
// evidence the session produced no work, and a changed one is evidence it did.
// That is the extra evidence the age clock requires before it may call a
// composer stalled, because pane shape alone cannot separate a wedged composer
// from a working agent holding a queued nudge (gt-hkhu).
//
// Volatile lines (spinner chrome, elapsed counters) are dropped first, for the
// reason PaneContentSignature drops them: they redraw on a timer, so they would
// otherwise date themselves as work.
//
// Returns "" when the capture has no non-volatile content above the input box,
// leaving it to the caller to decide what an absent digest means. Callers must
// treat "" as insufficient evidence, never as "unchanged".
//
// It is exported because the digest outlives the process that computed it: it
// is stored in PendingInputClock's stamps under the town's runtime directory,
// written by whichever of the daemon heartbeat and the witness patrol probes
// first and read by the other. A stamp whose digest a reader could not
// reproduce would be unreadable to that reader, so the function that produces
// it is part of the stamp format rather than a detail of the detector.
func PaneProgressSignature(captured, promptPrefix string) string {
	plain, _ := stripAnsiTrackDim(captured)
	lines := strings.Split(string(plain), "\n")

	end := len(lines)
	if composer := lastComposerLine(lines, promptPrefix); composer >= 0 {
		if rule := inputBoxTopRule(lines, composer); rule >= 0 {
			end = rule
		}
	}

	kept := make([]string, 0, end)
	for _, line := range lines[:end] {
		if isVolatilePaneLine(line) {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(kept, "\n")))
	return hex.EncodeToString(sum[:8])
}

// lastComposerLine returns the index of the last line in the capture that
// matches the agent's prompt prefix, or -1 when there is none. It is the last
// one because the transcript quotes earlier prompts — a previous turn's input
// line looks exactly like the live composer.
func lastComposerLine(lines []string, promptPrefix string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if matchesPromptPrefix(lines[i], promptPrefix) {
			return i
		}
	}
	return -1
}

// inputBoxScanLines bounds how far above the composer the rule opening the
// input box may sit. The box and Claude Code's queued-message list between them
// are a handful of lines; a rule further up than this is transcript content,
// and masking from it would hide the work this signature exists to see.
const inputBoxScanLines = 12

// inputBoxTopRule returns the index of the rule that opens the input box — the
// nearest box rule above the composer line — or -1 when there is none within
// inputBoxScanLines. A -1 means the box could not be located, and the caller
// falls back to hashing the whole pane.
func inputBoxTopRule(lines []string, composer int) int {
	low := composer - inputBoxScanLines
	if low < 0 {
		low = 0
	}
	for i := composer - 1; i >= low; i-- {
		if isBoxRule(lines[i]) {
			return i
		}
	}
	return -1
}

// boxRuleGlyphs are the characters Claude Code draws a horizontal rule with.
const boxRuleGlyphs = "─-=_"

// isBoxRule reports whether a line is a horizontal rule: a run of rule glyphs,
// long enough not to be prose and not to be a markdown emphasis underline.
func isBoxRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len([]rune(trimmed)) < 8 {
		return false
	}
	for _, r := range trimmed {
		if !strings.ContainsRune(boxRuleGlyphs, r) {
			return false
		}
	}
	return true
}
