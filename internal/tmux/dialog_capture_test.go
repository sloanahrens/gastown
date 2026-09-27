package tmux

import (
	"os"
	"path/filepath"
	"testing"
)

// TestContainsBlockingQuestionDialog_RealCapture pins the detector against a
// pane captured from a real Claude Code session parked on its own
// AskUserQuestion dialog (gt-163k8).
//
// The fixture is the verbatim output of:
//
//	tmux capture-pane -p -t <session> -S -45
//
// for a session asked to call AskUserQuestion once with question "n/a" and
// options "a" and "b" — the degenerate placeholder call a polecat makes when it
// uses the question tool to park itself instead of working. That is the exact
// shape recorded in gt-163k8 (garnet, 4h26m) and gt-z83 (pyrite), so a
// hand-written fixture that does not match this rendering is not evidence that
// detection works.
func TestContainsBlockingQuestionDialog_RealCapture(t *testing.T) {
	t.Parallel()

	path := filepath.Join("testdata", "askuserquestion_pane_capture.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading captured pane fixture: %v", err)
	}

	question, blocked := containsBlockingQuestionDialog(string(raw))
	if !blocked {
		t.Fatalf("detector missed a real AskUserQuestion pane (%s); "+
			"an unattended polecat parked on this dialog is never recovered", path)
	}

	// The escalation the recovery sends carries this text to a human, so it has
	// to be the question the polecat actually asked. In this capture that is
	// the literal placeholder "n/a" — the shape both incidents recorded — which
	// is exactly the case a "?"-suffixed-line search returns nothing for.
	if question != "n/a" {
		t.Errorf("captured question = %q, want %q", question, "n/a")
	}
}
