package cmd

import (
	"strings"
	"testing"
)

func TestAgentActor(t *testing.T) {
	t.Run("human terminal has no actor", func(t *testing.T) {
		t.Setenv("GT_ROLE", "")
		t.Setenv("BD_ACTOR", "")
		if actor := agentActor(); actor != "" {
			t.Errorf("expected empty actor, got %q", actor)
		}
	})

	t.Run("GT_ROLE wins", func(t *testing.T) {
		t.Setenv("GT_ROLE", "deacon/dogs/alpha")
		t.Setenv("BD_ACTOR", "deacon-alpha")
		if actor := agentActor(); actor != "deacon/dogs/alpha" {
			t.Errorf("expected GT_ROLE value, got %q", actor)
		}
	})

	t.Run("BD_ACTOR fallback", func(t *testing.T) {
		t.Setenv("GT_ROLE", "")
		t.Setenv("BD_ACTOR", "deacon-alpha")
		if actor := agentActor(); actor != "deacon-alpha" {
			t.Errorf("expected BD_ACTOR value, got %q", actor)
		}
	})
}

func TestCheckAgentForceAuthorization(t *testing.T) {
	t.Run("refuses without authorization bead", func(t *testing.T) {
		err := checkAgentForceAuthorization("deacon/dogs/alpha", "")
		if err == nil {
			t.Fatal("expected refusal for agent --force without --authorized-by")
		}
		if !strings.Contains(err.Error(), "gt-61x") {
			t.Errorf("error should reference the guardrail bead, got: %v", err)
		}
		if !strings.Contains(err.Error(), "--authorized-by") {
			t.Errorf("error should explain the remedy, got: %v", err)
		}
	})

	t.Run("allows with authorization bead", func(t *testing.T) {
		if err := checkAgentForceAuthorization("deacon/dogs/alpha", "hq-abc"); err != nil {
			t.Errorf("expected authorization to pass, got: %v", err)
		}
	})
}
