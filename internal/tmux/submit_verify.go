package tmux

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrSubmitNotVerified reports that a nudge payload was typed, but the
// transport could not prove it left the target composer.
var ErrSubmitNotVerified = errors.New("submit not verified: message stranded in composer")

type submitProbe int

const (
	probeUnknown submitProbe = iota
	probeTurnStarted
	probeComposerCleared
	probeStranded
	probeComposerDirty
)

func (p submitProbe) String() string {
	switch p {
	case probeTurnStarted:
		return "turn-started"
	case probeComposerCleared:
		return "composer-cleared"
	case probeStranded:
		return "stranded"
	case probeComposerDirty:
		return "composer-dirty"
	default:
		return "unknown"
	}
}

const (
	submitProbeAttempts  = 3
	submitProbeInterval  = 700 * time.Millisecond
	submitNeedleMaxRunes = 32
	minStrandPrefixRunes = 8
)

func submitNeedle(message string) string {
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > submitNeedleMaxRunes {
			return string(runes[:submitNeedleMaxRunes])
		}
		return line
	}
	return ""
}

func applySGR(params string, dim bool) bool {
	if params == "" {
		return false
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "", "0":
			dim = false
		case "2":
			dim = true
		case "22":
			dim = false
		case "38", "48", "58":
			if i+1 >= len(fields) {
				continue
			}
			switch fields[i+1] {
			case "5":
				i += 2
			case "2":
				i += 4
			}
		}
	}
	return dim
}

func stripAnsiTrackDim(s string) ([]rune, []bool) {
	var plain []rune
	var dim []bool
	curDim := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j >= len(s) {
					break
				}
				if s[j] == 'm' {
					curDim = applySGR(s[i+2:j], curDim)
				}
				i = j + 1
				continue
			}
			if i+1 < len(s) && s[i+1] == ']' {
				j := i + 2
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
					j++
				}
				if j >= len(s) {
					break
				}
				if s[j] == 0x1b {
					j++
				}
				i = j + 1
				continue
			}
			i += 2
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		plain = append(plain, r)
		dim = append(dim, curDim)
		i += size
	}
	return plain, dim
}

func runeIndex(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func allDim(flags []bool) bool {
	if len(flags) == 0 {
		return false
	}
	for _, flag := range flags {
		if !flag {
			return false
		}
	}
	return true
}

func splitRunesAndDim(plain []rune, dim []bool) ([][]rune, [][]bool) {
	var lines [][]rune
	var lineDims [][]bool
	start := 0
	for i := 0; i <= len(plain); i++ {
		if i == len(plain) || plain[i] == '\n' {
			lines = append(lines, plain[start:i])
			lineDims = append(lineDims, dim[start:i])
			start = i + 1
		}
	}
	return lines, lineDims
}

func trimRunesAndDim(runes []rune, dim []bool) ([]rune, []bool) {
	isSpace := func(r rune) bool { return r == ' ' || r == '\t' || r == '\u00a0' }
	for len(runes) > 0 && isSpace(runes[0]) {
		runes = runes[1:]
		dim = dim[1:]
	}
	for len(runes) > 0 && isSpace(runes[len(runes)-1]) {
		runes = runes[:len(runes)-1]
		dim = dim[:len(dim)-1]
	}
	return runes, dim
}

func composerContent(line []rune, dim []bool, promptPrefix string) ([]rune, []bool) {
	prefix := []rune(strings.TrimSpace(strings.ReplaceAll(promptPrefix, "\u00a0", " ")))
	if len(prefix) == 0 {
		return nil, nil
	}
	idx := runeIndex(line, prefix)
	if idx < 0 {
		idx = runeIndex(line, prefix[:1])
	}
	if idx < 0 {
		return nil, nil
	}
	after := line[idx+len(prefix):]
	afterDim := dim[idx+len(prefix):]
	return trimRunesAndDim(after, afterDim)
}

func analyzeComposerLine(line []rune, dim []bool, needle, promptPrefix string) submitProbe {
	needleRunes := []rune(needle)
	if idx := runeIndex(line, needleRunes); idx >= 0 {
		if allDim(dim[idx : idx+len(needleRunes)]) {
			return probeComposerCleared
		}
		return probeStranded
	}

	content, contentDim := composerContent(line, dim, promptPrefix)
	if len(content) == 0 {
		return probeComposerCleared
	}
	if allDim(contentDim) {
		return probeComposerCleared
	}
	if len(content) >= minStrandPrefixRunes && strings.HasPrefix(needle, string(content)) {
		return probeStranded
	}
	return probeComposerDirty
}

func analyzeSubmission(escContent, needle, promptPrefix string) submitProbe {
	if needle == "" || promptPrefix == "" {
		return probeUnknown
	}
	plain, dim := stripAnsiTrackDim(escContent)
	lines, lineDims := splitRunesAndDim(plain, dim)

	for i := len(lines) - 1; i >= 0; i-- {
		if matchesPromptPrefix(string(lines[i]), promptPrefix) {
			return analyzeComposerLine(lines[i], lineDims[i], needle, promptPrefix)
		}
	}

	for _, line := range lines {
		if hasBusyIndicator(string(line)) {
			return probeTurnStarted
		}
	}
	return probeUnknown
}

func (t *Tmux) probeSubmission(target, needle, promptPrefix string) submitProbe {
	content, err := t.run("capture-pane", "-p", "-e", "-t", target, "-S", "-25")
	if err != nil {
		return probeUnknown
	}
	return analyzeSubmission(content, needle, promptPrefix)
}

func (t *Tmux) pollSubmission(target, needle, promptPrefix string, attempts int) submitProbe {
	last := probeUnknown
	stranded := false
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(submitProbeInterval)
		}
		probe := t.probeSubmission(target, needle, promptPrefix)
		switch probe {
		case probeTurnStarted, probeComposerCleared:
			return probe
		case probeComposerDirty:
			return probe
		case probeStranded:
			stranded = true
		}
		last = probe
	}
	if stranded {
		return probeStranded
	}
	return last
}

// submitTurnTimeout is how long to wait for a turn to start after submitting
// a nudge. If no turn starts within this window, the nudge delivery is
// considered stuck and an error is returned.
const submitTurnTimeout = 5 * time.Second

func (t *Tmux) submitComposer(target, message, promptPrefix string) error {
	return t.submitComposerWithTimeout(target, message, promptPrefix, submitTurnTimeout)
}

// submitComposerWithTimeout is like submitComposer but with a configurable
// timeout for turn-start detection. This is used by nudge immediate mode to
// detect wedged sessions that appear alive but are not processing input.
// (Item 3 of gt-eigw: report when pane doesn't start a turn within a few seconds)
func (t *Tmux) submitComposerWithTimeout(target, message, promptPrefix string, timeout time.Duration) error {
	enterErr := t.sendEnterVerified(target)
	needle := submitNeedle(message)
	if needle == "" {
		return enterErr
	}

	// First, wait for the initial submission verification (composer cleared or turn started)
	start := time.Now()
	probe := t.pollSubmission(target, needle, promptPrefix, submitProbeAttempts)
	elapsed := time.Since(start)

	switch probe {
	case probeTurnStarted:
		// Turn started immediately - success
		return nil
	case probeComposerCleared:
		// Composer cleared but no turn started yet. Wait up to timeout for a turn.
		return t.waitForTurnWithTimeout(target, promptPrefix, timeout-elapsed)
	case probeUnknown:
		return enterErr
	case probeComposerDirty:
		return fmt.Errorf("%w (composer contains other text after Enter)", ErrSubmitNotVerified)
	case probeStranded:
		return t.recoverStrandedComposerWithTimeout(target, message, needle, promptPrefix, timeout-elapsed)
	default:
		return enterErr
	}
}

// waitForTurnWithTimeout polls the pane until a turn starts or the timeout is reached.
// It returns nil if a turn starts within the timeout, otherwise returns an error.
func (t *Tmux) waitForTurnWithTimeout(target, promptPrefix string, timeout time.Duration) error {
	// Poll frequently for turn start, but don't exceed the timeout
	pollInterval := 200 * time.Millisecond
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if probe := t.probeSubmission(target, "", promptPrefix); probe == probeTurnStarted {
			return nil
		}
		// If the agent is idle (prompt visible, no busy indicator), the nudge
		// has been delivered and will be processed when the agent starts a turn.
		// Don't fail for idle agents - they're not wedged.
		if isIdleAgent(target, promptPrefix, t) {
			return nil
		}
		// Use remaining time for the sleep, but cap at pollInterval
		remaining := time.Until(deadline)
		if remaining < pollInterval {
			pollInterval = remaining
		}
		if pollInterval <= 0 {
			break
		}
		time.Sleep(pollInterval)
	}

	return fmt.Errorf("%w (turn did not start within timeout)", ErrSubmitNotVerified)
}

// isIdleAgent checks if the agent appears to be idle (prompt visible, no busy indicator).
// This is used to distinguish idle agents (which will process nudges when input arrives)
// from wedged agents (which have been stuck for 5+ minutes with no output changes).
func isIdleAgent(target, promptPrefix string, t *Tmux) bool {
	content, err := t.run("capture-pane", "-p", "-e", "-t", target, "-S", "-25")
	if err != nil {
		return false
	}
	return hasPromptAndNoBusyIndicator(content, promptPrefix)
}

// hasPromptAndNoBusyIndicator checks if the pane shows an idle prompt.
// Returns true if the prompt prefix is visible and no busy indicator is present.
func hasPromptAndNoBusyIndicator(content, promptPrefix string) bool {
	lines := strings.Split(content, "\n")
	promptFound := false

	for _, line := range lines {
		if hasBusyIndicator(line) {
			return false // Busy indicator found - not idle
		}
		if !promptFound && matchesPromptPrefix(strings.TrimSpace(line), promptPrefix) {
			promptFound = true
		}
	}

	return promptFound
}

// recoverStrandedComposerWithTimeout is like recoverStrandedComposer but with
// a configurable timeout for turn-start detection after re-typing.
func (t *Tmux) recoverStrandedComposerWithTimeout(target, message, needle, promptPrefix string, timeout time.Duration) error {
	if _, err := t.run("send-keys", "-t", target, "C-j"); err != nil {
		return fmt.Errorf("%w (C-j reset failed: %v)", ErrSubmitNotVerified, err)
	}
	time.Sleep(500 * time.Millisecond)

	switch probe := t.probeSubmission(target, needle, promptPrefix); probe {
	case probeTurnStarted:
		return nil
	case probeComposerCleared:
		if err := t.sendMessageToTarget(target, message); err != nil {
			return fmt.Errorf("%w (retype failed: %v)", ErrSubmitNotVerified, err)
		}
		time.Sleep(adaptiveTextDelay(len(message)))
		_ = t.sendEnterVerified(target)
		// Wait for turn start after re-submission
		if err := t.waitForTurnWithTimeout(target, promptPrefix, timeout-750*time.Millisecond); err != nil {
			return fmt.Errorf("%w (turn did not start after retype)", ErrSubmitNotVerified)
		}
	case probeStranded, probeComposerDirty, probeUnknown:
		return fmt.Errorf("%w (composer state after C-j: %s)", ErrSubmitNotVerified, probe)
	}

	// Final verification with timeout
	start := time.Now()
	finalProbe := t.pollSubmission(target, needle, promptPrefix, submitProbeAttempts)
	elapsed := time.Since(start)
	if finalProbe == probeTurnStarted {
		return nil
	}
	if elapsed < timeout {
		if err := t.waitForTurnWithTimeout(target, promptPrefix, timeout-elapsed); err != nil {
			return fmt.Errorf("nudge submit to %q: %w (turn did not start after timeout, final state: %s)", target, ErrSubmitNotVerified, finalProbe)
		}
		return nil
	}
	return fmt.Errorf("nudge submit to %q: %w (final state: %s)", target, ErrSubmitNotVerified, finalProbe)
}

func (t *Tmux) recoverStrandedComposer(target, message, needle, promptPrefix string) error {
	if _, err := t.run("send-keys", "-t", target, "C-j"); err != nil {
		return fmt.Errorf("%w (C-j reset failed: %v)", ErrSubmitNotVerified, err)
	}
	time.Sleep(500 * time.Millisecond)

	switch probe := t.probeSubmission(target, needle, promptPrefix); probe {
	case probeTurnStarted:
		return nil
	case probeComposerCleared:
		if err := t.sendMessageToTarget(target, message); err != nil {
			return fmt.Errorf("%w (retype failed: %v)", ErrSubmitNotVerified, err)
		}
		time.Sleep(adaptiveTextDelay(len(message)))
		_ = t.sendEnterVerified(target)
	case probeStranded, probeComposerDirty, probeUnknown:
		return fmt.Errorf("%w (composer state after C-j: %s)", ErrSubmitNotVerified, probe)
	}

	switch probe := t.pollSubmission(target, needle, promptPrefix, submitProbeAttempts); probe {
	case probeTurnStarted, probeComposerCleared:
		return nil
	default:
		return fmt.Errorf("nudge submit to %q: %w (final state: %s)", target, ErrSubmitNotVerified, probe)
	}
}
