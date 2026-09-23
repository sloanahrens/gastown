package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// channelWakeRig is a rig/prefix pair that exists only in these tests, so a
// resolved session name can never collide with a live town's.
const (
	channelWakeRig    = "wpf0rig"
	channelWakePrefix = "zzwpf0"
)

// setupChannelWakeRegistry binds channelWakeRig to its own session prefix.
func setupChannelWakeRegistry(t *testing.T) string {
	t.Helper()
	reg := session.NewPrefixRegistry()
	reg.Register(channelWakePrefix, channelWakeRig)
	old := session.DefaultRegistry()
	session.SetDefaultRegistry(reg)
	t.Cleanup(func() { session.SetDefaultRegistry(old) })
	return session.WitnessSessionName(channelWakePrefix)
}

// requireTmuxForWake skips when tmux is unavailable. The hermetic harness
// (internal/testutil) points tmux.NewTmux() at an isolated gt-test-* socket,
// so a session created here cannot reach a town's live server.
func requireTmuxForWake(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

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

// setEmitEventVars drives the emit-event command's package-level flags.
func setEmitEventVars(t *testing.T, channel, rig, eventType string, payload []string) {
	t.Helper()
	oldChannel, oldRig, oldType, oldPayload := emitEventChannel, emitEventRig, emitEventType, emitEventPayload
	oldJSON := moleculeJSON
	t.Cleanup(func() {
		emitEventChannel, emitEventRig, emitEventType, emitEventPayload = oldChannel, oldRig, oldType, oldPayload
		moleculeJSON = oldJSON
	})
	emitEventChannel, emitEventRig, emitEventType, emitEventPayload = channel, rig, eventType, payload
	moleculeJSON = false
}

// TestEmitEventWitnessChannelWakesSession is the gt-wpf0 acceptance test: an
// emit on the witness channel must reach the witness session, because nothing
// polls events/witness/<rig>/ for it.
func TestEmitEventWitnessChannelWakesSession(t *testing.T) {
	requireTmuxForWake(t)
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

// TestEmitEventWitnessChannelFailsLoudWithoutSession pins the loud half: an
// emit that cannot reach the witness must not exit 0. The previous behaviour
// wrote a file no process reads and reported success.
func TestEmitEventWitnessChannelFailsLoudWithoutSession(t *testing.T) {
	requireTmuxForWake(t)
	sessionName := setupChannelWakeRegistry(t)
	townRoot := makeTestTownRoot(t)

	tm := tmux.NewTmux()
	_ = tm.KillSession(sessionName)

	setEmitEventVars(t, "witness", channelWakeRig, "POLECAT_DONE", []string{"message=POLECAT_DONE nux exit=COMPLETED"})

	err := runMoleculeEmitEvent(nil, nil)
	if err == nil {
		t.Fatal("emit with no witness session exited 0; the wake went nowhere and nothing said so")
	}
	if !strings.Contains(err.Error(), sessionName) {
		t.Errorf("error must name the session it could not reach, got: %v", err)
	}
	if !strings.Contains(err.Error(), "event written to") {
		t.Errorf("error must report that the event file was still written, got: %v", err)
	}

	events, globErr := filepath.Glob(filepath.Join(townRoot, "events", "witness", channelWakeRig, "*.event"))
	if globErr != nil {
		t.Fatalf("glob events: %v", globErr)
	}
	if len(events) != 1 {
		t.Errorf("the durable record must survive a failed wake, got %d files: %v", len(events), events)
	}
}

// TestEmitEventRefineryChannelNeedsNoSession keeps the distinction: await-event
// polls the refinery channel, so its file is the delivery and a missing session
// is not a failure.
func TestEmitEventRefineryChannelNeedsNoSession(t *testing.T) {
	requireTmuxForWake(t)
	setupChannelWakeRegistry(t)
	makeTestTownRoot(t)

	setEmitEventVars(t, "refinery", channelWakeRig, "MQ_SUBMIT", []string{"branch=feat/x"})

	if err := runMoleculeEmitEvent(nil, nil); err != nil {
		t.Fatalf("refinery emit must not require a live session: %v", err)
	}
}

// TestNudgeWitnessDeliversPolecatDone covers the production POLECAT_DONE path
// (gt done) rather than the CLI wrapper, on the same fake session.
func TestNudgeWitnessDeliversPolecatDone(t *testing.T) {
	requireTmuxForWake(t)
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
