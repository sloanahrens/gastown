//go:build integration

package tmux

import (
	"strings"
	"testing"
)

// TestIntegrationCapturePaneVisibleTail_ExcludesHistory guards the capture-window defect
// behind gt-dq6pi: CapturePane/CapturePaneLines(session, N) pass N to tmux's
// "-S -N" (start-line) flag, which starts N lines into HISTORY and, absent an
// explicit -E, still ends at the bottom of the visible pane — so on a pane
// taller than N it returns the whole visible screen PLUS N history lines, not
// the last N lines. Busy detection asking for "the last 20 lines" this way
// actually scans the full screen plus 20 rows of old transcript, so content
// that scrolled out of view (e.g. a relayed nudge quoting another pane's
// spinner) never ages out. capturePaneVisibleTail must return only the
// visible screen's own tail, regardless of how deep the history goes.
func TestIntegrationCapturePaneVisibleTail_ExcludesHistory(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-visible-tail-" + t.Name()

	_ = tm.KillSession(session)
	if err := tm.NewSession(session, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(session) }()

	// A pane a bit taller than busyCaptureLines (20), matching the real
	// Claude Code panes (40-63 rows) the bug was reproduced against.
	if _, err := tm.run("resize-window", "-t", session, "-x", "80", "-y", "24"); err != nil {
		t.Fatalf("resize-window: %v", err)
	}

	// One command that scrolls the marker into history: the marker, then more
	// lines than the pane is tall. Wait for the last one to render.
	if err := tm.SendKeys(session, "echo HISTORY_MARKER_TOKEN; seq -f fill-%g 1 40"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	eventually(t, "the output to render", func() bool {
		out, _ := tm.CapturePane(session, 5)
		return strings.Contains(out, "fill-40")
	})

	// Sanity check: confirm this environment actually reproduces the "-S -N"
	// over-capture before asserting the fix, so a future tmux behavior change
	// fails loudly here instead of silently passing the negative assertion.
	broken, err := tm.CapturePaneLines(session, busyCaptureLines)
	if err != nil {
		t.Fatalf("CapturePaneLines: %v", err)
	}
	if !linesContain(broken, "HISTORY_MARKER_TOKEN") {
		t.Fatal("tmux did not reproduce the -S window behavior this test guards against; its capture semantics changed")
	}

	tail, err := tm.capturePaneVisibleTail(session)
	if err != nil {
		t.Fatalf("capturePaneVisibleTail: %v", err)
	}
	if len(tail) > busyCaptureLines {
		t.Errorf("capturePaneVisibleTail returned %d lines, want at most %d", len(tail), busyCaptureLines)
	}
	if linesContain(tail, "HISTORY_MARKER_TOKEN") {
		t.Errorf("capturePaneVisibleTail leaked scrollback history into the tail: %v", tail)
	}
}

func linesContain(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}
