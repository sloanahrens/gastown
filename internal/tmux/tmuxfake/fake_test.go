package tmuxfake

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestFakeSessionsContract(t *testing.T) {
	t.Parallel()
	RunSessionsContract(t, func(t *testing.T) Sessions { return New(clockwork.NewFakeClock()) })
}

func TestFakeWaitForIdleWakesOnSetIdle(t *testing.T) {
	t.Parallel()
	s := New(clockwork.NewFakeClock())
	if err := s.NewSession("a", ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.WaitForIdle("a", time.Minute) }()
	s.SetIdle("a", true)
	if err := <-done; err != nil {
		t.Fatalf("WaitForIdle = %v, want nil", err)
	}
}

func TestFakeWaitForCommandTimesOutOnClock(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClock()
	s := New(clk)
	if err := s.NewSession("a", ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.WaitForCommand("a", []string{"zsh"}, time.Second) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	if err := <-done; err == nil {
		t.Fatal("WaitForCommand = nil, want timeout")
	}
}

func TestFakeScriptingHelpers(t *testing.T) {
	t.Parallel()
	s := New(clockwork.NewFakeClock())
	if err := s.NewSessionWithCommandAndEnv("a", "", "claude --x", map[string]string{"K": "v"}); err != nil {
		t.Fatal(err)
	}
	if !s.IsAgentRunning("a") || !s.IsAgentRunning("a", "claude") || s.IsAgentRunning("a", "node") {
		t.Fatal("IsAgentRunning disagrees with pane command claude")
	}
	s.SetPaneCommand("a", "bash")
	if s.IsAgentRunning("a") {
		t.Fatal("IsAgentRunning true for a shell")
	}
	s.SetScreen("a", "one", "two", "three")
	if got, _ := s.CapturePane("a", 2); got != "two\nthree" {
		t.Fatalf("CapturePane = %q", got)
	}
	if err := s.NudgeSession("a:0.0", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.SendKeys("missing", "x"); err == nil {
		t.Fatal("SendKeys to missing session = nil")
	}
	if got := s.Sent("a"); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("Sent = %v", got)
	}
	if err := s.RespawnPane("a:0", "node app.js"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.GetPaneCommand("a"); c != "node" {
		t.Fatalf("pane command after respawn = %q", c)
	}
	if s.Env("a")["K"] != "v" {
		t.Fatalf("Env = %v", s.Env("a"))
	}
	ready := make(chan error, 1)
	rc := &config.RuntimeConfig{Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "❯ "}}
	go func() { ready <- s.WaitForRuntimeReady("a", rc, time.Minute) }()
	s.SetScreen("a", "banner", "❯ ")
	if err := <-ready; err != nil {
		t.Fatalf("WaitForRuntimeReady = %v, want nil once the prompt shows", err)
	}
	s.Exit("a")
	if ok, _ := s.HasSession("a"); ok {
		t.Fatal("session survived Exit")
	}
}

// TestFakeWaitForCommandHonoursAgentReady mirrors *tmux.Tmux: a wrapped agent
// whose pane is still a shell counts once GT_AGENT_READY=1 is set, and a
// sentinel left from before the wait does not count.
func TestFakeWaitForCommandHonoursAgentReady(t *testing.T) {
	t.Parallel()
	s := New(clockwork.NewFakeClock())
	if err := s.NewSessionWithCommandAndEnv("a", "", "", map[string]string{tmux.EnvAgentReady: "1"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.WaitForCommand("a", []string{"zsh"}, time.Minute) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for s.Env("a")[tmux.EnvAgentReady] != "" { // the stale sentinel is cleared first
		if ctx.Err() != nil {
			t.Fatal("stale GT_AGENT_READY was not cleared")
		}
		runtime.Gosched()
	}
	if err := s.SetEnvironment("a", tmux.EnvAgentReady, "1"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForCommand = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("WaitForCommand ignored GT_AGENT_READY")
	}
}

func TestFakeWaitForRuntimeReadyReadsLastTenLines(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClock()
	s := New(clk)
	if err := s.NewSession("a", ""); err != nil {
		t.Fatal(err)
	}
	lines := []string{"❯ "}
	for i := 0; i < 10; i++ {
		lines = append(lines, "output")
	}
	s.SetScreen("a", lines...) // prompt is 11 lines up: out of reach
	rc := &config.RuntimeConfig{Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "❯ "}}
	done := make(chan error, 1)
	go func() { done <- s.WaitForRuntimeReady("a", rc, time.Second) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	if err := <-done; err == nil {
		t.Fatal("WaitForRuntimeReady saw a prompt beyond the last 10 lines")
	}
}
