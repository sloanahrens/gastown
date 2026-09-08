package cmd

import (
	"errors"
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

func TestHoldsGateError(t *testing.T) {
	t.Run("no error passes", func(t *testing.T) {
		if err := holdsGateError(nil, false); err != nil {
			t.Errorf("expected nil for successful holds query, got: %v", err)
		}
	})

	t.Run("dry run continues despite holds failure", func(t *testing.T) {
		if err := holdsGateError(errors.New("dolt down"), true); err != nil {
			t.Errorf("dry run is non-destructive and should continue, got: %v", err)
		}
	})

	t.Run("destructive run fails closed on holds failure", func(t *testing.T) {
		err := holdsGateError(errors.New("dolt down"), false)
		if err == nil {
			t.Fatal("expected fail-closed error when holds cannot be verified for a destructive run")
		}
		if !strings.Contains(err.Error(), "holds") {
			t.Errorf("error should mention holds, got: %v", err)
		}
	})
}

func TestResolveDestructiveActor(t *testing.T) {
	t.Run("env identity is an agent", func(t *testing.T) {
		actor, isAgent := resolveDestructiveActor("gastown/polecats/onyx", true)
		if !isAgent || actor != "gastown/polecats/onyx" {
			t.Errorf("expected agent with env identity, got actor=%q isAgent=%v", actor, isAgent)
		}
	})

	t.Run("no identity at a terminal is a human", func(t *testing.T) {
		actor, isAgent := resolveDestructiveActor("", true)
		if isAgent || actor != "" {
			t.Errorf("expected human operator, got actor=%q isAgent=%v", actor, isAgent)
		}
	})

	t.Run("no identity without a terminal is agent-by-default", func(t *testing.T) {
		actor, isAgent := resolveDestructiveActor("", false)
		if !isAgent {
			t.Error("unset identity off-terminal must be treated as an agent (spoofing guard, gt-2oy)")
		}
		if actor == "" {
			t.Error("agent-by-default actor should carry a descriptive label")
		}
	})
}
