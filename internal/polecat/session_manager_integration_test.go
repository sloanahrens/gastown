//go:build integration

package polecat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// testSessionCounter provides unique session names across -count=N runs
// to prevent "duplicate session" races with tmux's async cleanup.
var testSessionCounter atomic.Int64

// fastStartupNudgeRig returns a rig whose town (the parent of the rig dir)
// configures a 200ms startup-nudge verify delay. verifyStartupNudgeDelivery
// reads operational.session.startup_nudge_verify_delay from
// <townRoot>/settings/config.json with townRoot = Dir(rig.Path); left at the
// compiled-in default the two retries sleep 25s each, and every test that
// drives the retry loop against a live fake pane paid ~57s for nothing
// (gt-0mbw).
func fastStartupNudgeRig(t *testing.T) *rig.Rig {
	t.Helper()
	townRoot := t.TempDir()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("mkdir settings: %v", err)
	}
	cfg := `{"type":"town-settings","version":1,"operational":{"session":{"startup_nudge_verify_delay":"200ms"}}}`
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return &rig.Rig{Name: "test-rig", Path: filepath.Join(townRoot, "test-rig")}
}

// TestIntegrationVerifyStartupNudgeDelivery_IdleAgent is the wiring guard for
// SessionManager.verifyStartupNudgeDelivery: through a real tmux session
// sitting at the ❯ prompt, on the real clock and the town's configured 200ms
// verify delay, the lost nudge is typed into the pane again.
func TestIntegrationVerifyStartupNudgeDelivery_IdleAgent(t *testing.T) {
	requireTmuxIntegration(t)

	tm := tmux.NewTmux()
	sessionName := fmt.Sprintf("gt-test-nudge-%d", testSessionCounter.Add(1))
	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	// Show the Claude prompt prefix with no busy indicator: an idle agent.
	_ = tm.SendKeys(sessionName, "export PS1='❯ '")
	deadline := time.Now().Add(30 * time.Second)
	for !tm.IsIdle(sessionName) {
		if time.Now().After(deadline) {
			t.Fatal("the pane never read as idle at the ❯ prompt")
		}
		time.Sleep(100 * time.Millisecond)
	}

	m := NewSessionManager(tm, fastStartupNudgeRig(t), nil)
	rc := &config.RuntimeConfig{Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "❯ "}}
	const retry = "check-your-hook-retry"
	m.verifyStartupNudgeDelivery(sessionName, rc, retry)

	out, err := tm.CapturePane(sessionName, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, retry) {
		t.Fatalf("the idle agent was never re-nudged; pane:\n%s", out)
	}
}
