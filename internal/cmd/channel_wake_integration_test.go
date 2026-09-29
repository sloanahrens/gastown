//go:build integration

package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// These two drive a real tmux session end to end, so they live in the
// integration tier; the emit and nudge decisions around them are unit tests
// in channel_wake_test.go.

// startFakeWitnessSession creates a shell session under the name the wake code
// resolves, and waits for its prompt. The nudge protocol decides whether Enter
// was processed by diffing pane content, so a still-blank pane reports the
// submission as unverified (gt-32pv).
func startFakeWitnessSession(t *testing.T, tm *tmux.Tmux, sessionName string) {
	t.Helper()
	_ = tm.KillSession(sessionName) // a leftover from a failed earlier run is not a failure here

	if err := tm.NewSession(sessionName, t.TempDir()); err != nil {
		t.Fatalf("NewSession %s: %v", sessionName, err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if content, err := tm.CapturePane(sessionName, 30); err == nil && hasShellPrompt(content) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Skipf("no shell prompt in %s after 30s: the environment cannot supply this test's precondition", sessionName)
}

// hasShellPrompt reports whether any pane line ends in a prompt indicator.
func hasShellPrompt(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for _, suffix := range []string{"$", "%", "#", ">", "❯"} {
			if strings.HasSuffix(trimmed, suffix) {
				return true
			}
		}
	}
	return false
}

// paneShows polls the pane until it renders want, returning false on timeout.
// It is a barrier rather than a sleep: the wait ends as soon as the delivered
// text appears.
func paneShows(tm *tmux.Tmux, sessionName, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if content, err := tm.CapturePane(sessionName, 40); err == nil &&
			strings.Contains(strings.ReplaceAll(content, "\n", ""), want) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// requireTmuxForWakeIntegration fails when tmux is missing: an integration
// test whose tool is absent has checked nothing.
func requireTmuxForWakeIntegration(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux is required for this integration test: %v", err)
	}
}

// TestIntegrationEmitEventWitnessChannelWakesSession is the gt-wpf0 acceptance test: an
// emit on the witness channel must reach the witness session, because nothing
// polls events/witness/<rig>/ for it.
func TestIntegrationEmitEventWitnessChannelWakesSession(t *testing.T) {
	requireTmuxForWakeIntegration(t)
	sessionName := setupChannelWakeRegistry(t)
	townRoot := makeTestTownRoot(t)

	tm := tmux.NewTmux()
	startFakeWitnessSession(t, tm, sessionName)

	setEmitEventVars(t, "witness", channelWakeRig, "POLECAT_DONE",
		[]string{"source=polecat", "message=POLECAT_DONE nux exit=COMPLETED"})

	if err := runMoleculeEmitEvent(nil, nil); err != nil {
		t.Fatalf("runMoleculeEmitEvent: %v", err)
	}

	if !paneShows(tm, sessionName, "exit=COMPLETED", 15*time.Second) {
		content, _ := tm.CapturePane(sessionName, 40)
		t.Fatalf("witness session %s never received the wake; pane:\n%s", sessionName, content)
	}

	events, err := filepath.Glob(filepath.Join(townRoot, "events", "witness", channelWakeRig, "*.event"))
	if err != nil {
		t.Fatalf("glob events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected the event file to remain as the durable record, got %d: %v", len(events), events)
	}
}

// TestIntegrationNudgeWitnessDeliversPolecatDone covers the production POLECAT_DONE path
// (gt done) rather than the CLI wrapper, on the same fake session.
func TestIntegrationNudgeWitnessDeliversPolecatDone(t *testing.T) {
	requireTmuxForWakeIntegration(t)
	sessionName := setupChannelWakeRegistry(t)
	makeTestTownRoot(t)

	tm := tmux.NewTmux()
	startFakeWitnessSession(t, tm, sessionName)

	nudgeWitness(channelWakeRig, "POLECAT_DONE nux exit=COMPLETED")

	if !paneShows(tm, sessionName, "exit=COMPLETED", 15*time.Second) {
		content, _ := tm.CapturePane(sessionName, 40)
		t.Fatalf("nudgeWitness never reached %s; pane:\n%s", sessionName, content)
	}
}
