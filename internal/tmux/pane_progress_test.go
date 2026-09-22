package tmux

import (
	"strings"
	"testing"
)

// The rule Claude Code draws its input box with. Long enough to be a rule by
// any measure, and the same glyph the live captures use.
var progressTestRule = strings.Repeat("─", 80)

// idleAwaitPane is the gt-hkhu shape with its input box assembled from parts,
// so a test can vary what lands in the box while holding the transcript still.
func idleAwaitPane(transcript, composer, footer string) string {
	var b strings.Builder
	b.WriteString(transcript)
	b.WriteString("\n\n")
	b.WriteString(progressTestRule + "\n")
	b.WriteString(composer + "\n")
	if footer != "" {
		b.WriteString(footer + "\n")
	}
	b.WriteString(progressTestRule + "\n")
	b.WriteString("  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage")
	return b.String()
}

// The defect this signature exists to close: the patrols' own nudges land in
// the input box, and every pane-write signal the town had was reset by them. A
// signature that moved when a nudge arrived would restart the stall clock with
// the very input it is measuring, which is the feedback loop of gt-afa7.
func TestPaneProgressSignatureIgnoresTheInputBox(t *testing.T) {
	t.Parallel()
	const transcript = "⏺ Waiting on the review."

	empty := idleAwaitPane(transcript, "❯ ", "  Press up to edit queued messages · ctrl+x ctrl+s to send now")
	typed := idleAwaitPane(transcript, "❯ [from gastown/sling] MERGE_READY received - check inbox", "")
	otherNudge := idleAwaitPane(transcript, "❯ [from gastown/deacon] HEALTH_CHECK: refinery", "")

	base := PaneProgressSignature(empty, DefaultReadyPromptPrefix)
	if base == "" {
		t.Fatal("the signature of a pane with a transcript line is empty")
	}
	for name, pane := range map[string]string{"typed nudge": typed, "another nudge": otherNudge} {
		if got := PaneProgressSignature(pane, DefaultReadyPromptPrefix); got != base {
			t.Errorf("%s moved the progress signature (%s != %s); the stall clock would restart on its own nudges",
				name, got, base)
		}
	}
}

// The converse, and the reason the age clock cannot be unconditional: an agent
// that produced output between two observations restarted its own clock. This
// is what keeps the age off a working or idle-await session, the gt-hkhu false
// positive.
func TestPaneProgressSignatureTracksWorkAboveTheInputBox(t *testing.T) {
	t.Parallel()
	before := idleAwaitPane("⏺ Waiting on the review.", "❯ ", "")
	after := idleAwaitPane("⏺ Waiting on the review.\n⏺ Reviewed MR gt-abc: 3 files.", "❯ ", "")

	if PaneProgressSignature(before, DefaultReadyPromptPrefix) == PaneProgressSignature(after, DefaultReadyPromptPrefix) {
		t.Error("new transcript output did not move the progress signature")
	}
}

// Spinner chrome and the status bar redraw on a timer, so they are not work and
// must not be mistaken for it — otherwise a stalled agent's own animation would
// keep its clock restarted forever and the stall would never be caught.
func TestPaneProgressSignatureIgnoresVolatileChrome(t *testing.T) {
	t.Parallel()
	const transcript = "⏺ Waiting on the review."
	// A spinner line above the rule: volatile wherever it renders.
	spinning := strings.Replace(
		idleAwaitPane(transcript, "❯ ", ""),
		transcript, transcript+"\n✻ Scampering… (1m 17s · ↓ 2.4k tokens)", 1)

	base := PaneProgressSignature(idleAwaitPane(transcript, "❯ ", ""), DefaultReadyPromptPrefix)
	if got := PaneProgressSignature(spinning, DefaultReadyPromptPrefix); got != base {
		t.Errorf("spinner chrome moved the progress signature (%s != %s)", got, base)
	}
}

// A capture with nothing above the input box has no progress to report. Callers
// must see that as "no evidence", which is why it is "" and not a digest of the
// empty string — an all-empty pane would otherwise hash identically to every
// other all-empty pane and read as "unchanged".
func TestPaneProgressSignatureEmptyAboveTheBox(t *testing.T) {
	t.Parallel()
	pane := idleAwaitPane("", "❯ ", "")
	if got := PaneProgressSignature(pane, DefaultReadyPromptPrefix); got != "" {
		t.Errorf("signature of a pane with nothing above the input box = %q, want \"\"", got)
	}
}

// Without a prompt prefix the input box cannot be located, so the signature
// falls back to the whole non-volatile pane. That direction is safe: more of the
// pane is hashed, so it is harder for the digest to stand still, and a stall
// that is never tripped is the failure this detector may make.
func TestPaneProgressSignatureFallsBackWhenTheBoxCannotBeLocated(t *testing.T) {
	t.Parallel()
	pane := queuedMessagesPane
	if got := PaneProgressSignature(pane, ""); got == "" {
		t.Error("signature = \"\" with no prompt prefix; want the whole-pane fallback")
	}
	// The fallback still excludes the volatile status bar lines.
	if got := PaneProgressSignature(queuedMessagesPane+"\n✻ Simmering… (12s)", ""); got != PaneProgressSignature(queuedMessagesPane, "") {
		t.Error("the whole-pane fallback counted a spinner line as progress")
	}
}

// A box rule further up than the input box belongs to the transcript, not the
// box, and masking from it would hide the work this signature exists to see.
func TestInputBoxTopRuleDoesNotReachIntoTheTranscript(t *testing.T) {
	t.Parallel()
	lines := strings.Split(idleAwaitPane("⏺ Done.", "❯ ", ""), "\n")
	composer := lastComposerLine(lines, DefaultReadyPromptPrefix)
	if composer < 0 {
		t.Fatal("no composer line found in the fixture")
	}
	if rule := inputBoxTopRule(lines, composer); rule != 2 {
		t.Errorf("inputBoxTopRule = %d, want 2 (the rule opening the box)", rule)
	}

	// A composer with no rule anywhere above it: the box cannot be located, so
	// the caller must fall back to the whole pane rather than mask from some
	// earlier rule.
	bare := []string{"⏺ Done.", "", "❯ ", "  ⏵⏵ bypass permissions on"}
	if rule := inputBoxTopRule(bare, lastComposerLine(bare, DefaultReadyPromptPrefix)); rule != -1 {
		t.Errorf("inputBoxTopRule = %d, want -1 when there is no rule above the composer", rule)
	}

	// A rule further up than the scan window is transcript content even if it
	// was once a box rule, so it is out of reach too.
	filler := make([]string, 0, inputBoxScanLines)
	for i := 0; i < inputBoxScanLines; i++ {
		filler = append(filler, "⏺ transcript line")
	}
	// rule, then filler, then the composer: the rule is now past the window.
	far := append([]string{progressTestRule}, append(filler, "❯ ")...)
	if rule := inputBoxTopRule(far, lastComposerLine(far, DefaultReadyPromptPrefix)); rule != -1 {
		t.Errorf("inputBoxTopRule = %d, want -1 when the only rule is past the scan window", rule)
	}
}

func TestIsBoxRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"live rule", progressTestRule, true},
		{"short dashes are prose", "  --  ", false},
		{"markdown emphasis underline", "  ===  ", false},
		{"rule with a caption", strings.Repeat("─", 20) + " done", false},
		{"transcript prose", "⏺ Waiting on the review.", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isBoxRule(tt.line); got != tt.want {
				t.Errorf("isBoxRule(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}
