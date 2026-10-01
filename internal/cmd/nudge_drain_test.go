package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/nudge"
)

// TestDrainSessionNudgesNoTmuxSession verifies drainSessionNudges degrades to
// a no-op (nil, no error) when the caller isn't inside a tmux pane, which is
// the path any non-interactive test process takes.
func TestDrainSessionNudgesNoTmuxSession(t *testing.T) {
	t.Parallel()
	if got := drainNudgesFor(io.Discard, t.TempDir(), ""); got != nil {
		t.Errorf("expected nil with no tmux pane, got %v", got)
	}
}

// TestPrintSessionNudgesGoesToStderrNotStdout is the gt-dekkl regression: a
// queued nudge must reach the agent while staying out of the stdout that
// mol-refinery-patrol (MR=$(gt mq next <rig> --quiet)), stuck-work-dog and
// mol-boot-triage parse as machine output. On stdout the nudge block breaks
// the parse AND, because the drain already removed it from the queue, is lost
// with nothing legible delivered. writeSessionNudges is handed stderr only.
func TestPrintSessionNudgesGoesToStderrNotStdout(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-refinery"
	townRoot := fakeTownRoot(t)

	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		Sender:  "mayor/",
		Message: "stop gating and read the P0 first",
	}); err != nil {
		t.Fatalf("enqueue nudge: %v", err)
	}

	var stderr bytes.Buffer
	writeSessionNudges(&stderr, townRoot, session)

	if !strings.Contains(stderr.String(), "stop gating and read the P0 first") {
		t.Errorf("drained nudge missing from stderr: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "<system-reminder>") {
		t.Errorf("stderr missing the injection block: %q", stderr.String())
	}
	// The nudge must actually have been drained, or the assertions above pass
	// vacuously against a queue this test never filled.
	if left := nudge.QueueLen(townRoot, session); left != 0 {
		t.Errorf("nudge queue still holds %d entr(ies) after the drain", left)
	}
}

// TestPrintSessionNudgesSilentWhenQueueEmpty pins the empty-queue path: a
// per-cycle command must print nothing at all when there is nothing queued.
func TestPrintSessionNudgesSilentWhenQueueEmpty(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	writeSessionNudges(&stderr, fakeTownRoot(t), "gt-gastown-refinery")
	if stderr.Len() != 0 {
		t.Errorf("empty queue produced output: %q", stderr.String())
	}
}

// TestPrintSessionNudgesNoTmuxSession verifies the helper degrades to a no-op
// when the caller isn't inside a tmux pane.
func TestPrintSessionNudgesNoTmuxSession(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	writeSessionNudges(&stderr, fakeTownRoot(t), "")
	if stderr.Len() != 0 {
		t.Errorf("no tmux pane produced output: %q", stderr.String())
	}
}

// fakeTownRoot returns a directory that passes workspace.IsWorkspace.
func fakeTownRoot(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("create mayor marker dir: %v", err)
	}
	return townRoot
}
