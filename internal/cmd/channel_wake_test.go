package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
