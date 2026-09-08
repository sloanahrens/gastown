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

// testAuditor builds a cleanupAuditor whose bead and event writes are captured
// in-memory. beadErr simulates AddComment failure; beadID "" means no
// authorizing bead (human forced run).
func testAuditor(beadID string, beadErr error) (*cleanupAuditor, *[]string, *[]string) {
	var comments, eventTypes []string
	a := &cleanupAuditor{
		actor:  "gastown/polecats/onyx",
		beadID: beadID,
		addComment: func(id, text string) error {
			if beadErr != nil {
				return beadErr
			}
			comments = append(comments, id+": "+text)
			return nil
		},
		logAudit: func(eventType string, payload map[string]interface{}) {
			eventTypes = append(eventTypes, eventType)
		},
	}
	return a, &comments, &eventTypes
}

func TestCleanupAuditorIntent(t *testing.T) {
	t.Run("writes intent to bead before any removal", func(t *testing.T) {
		a, comments, events := testAuditor("hq-abc", nil)
		if err := a.recordIntent([]string{"testdb_1", "testdb_2"}); err != nil {
			t.Fatalf("expected intent to succeed, got: %v", err)
		}
		if len(*comments) != 1 {
			t.Fatalf("expected 1 bead comment, got %d", len(*comments))
		}
		c := (*comments)[0]
		for _, want := range []string{"hq-abc", "INTENT", "gastown/polecats/onyx", "testdb_1", "testdb_2"} {
			if !strings.Contains(c, want) {
				t.Errorf("intent comment missing %q: %s", want, c)
			}
		}
		if len(*events) != 1 {
			t.Errorf("expected 1 audit event, got %d", len(*events))
		}
	})

	t.Run("fails closed when intent cannot be recorded", func(t *testing.T) {
		a, _, _ := testAuditor("hq-abc", errors.New("dolt down"))
		err := a.recordIntent([]string{"testdb_1"})
		if err == nil {
			t.Fatal("expected fail-closed error when intent record cannot be written (gt-87a)")
		}
		if !strings.Contains(err.Error(), "gt-87a") {
			t.Errorf("error should reference the guardrail bead, got: %v", err)
		}
	})

	t.Run("human run without bead still logs an audit event", func(t *testing.T) {
		a, comments, events := testAuditor("", nil)
		if err := a.recordIntent([]string{"testdb_1"}); err != nil {
			t.Fatalf("no-bead intent must not fail, got: %v", err)
		}
		if len(*comments) != 0 {
			t.Errorf("expected no bead comments without an authorizing bead, got %d", len(*comments))
		}
		if len(*events) != 1 {
			t.Errorf("expected 1 audit event for human forced run, got %d", len(*events))
		}
	})
}

func TestCleanupAuditorCompletion(t *testing.T) {
	t.Run("records completion with counts and names", func(t *testing.T) {
		a, comments, _ := testAuditor("hq-abc", nil)
		if err := a.recordCompletion(2, 3, []string{"testdb_1", "testdb_2"}); err != nil {
			t.Fatalf("expected completion to succeed, got: %v", err)
		}
		if len(*comments) != 1 {
			t.Fatalf("expected 1 bead comment, got %d", len(*comments))
		}
		c := (*comments)[0]
		for _, want := range []string{"removed 2/3", "testdb_1", "testdb_2", "gastown/polecats/onyx"} {
			if !strings.Contains(c, want) {
				t.Errorf("completion comment missing %q: %s", want, c)
			}
		}
	})

	t.Run("records even when nothing was removed", func(t *testing.T) {
		a, comments, events := testAuditor("hq-abc", nil)
		if err := a.recordCompletion(0, 2, nil); err != nil {
			t.Fatalf("failed-attempt completion must still record, got: %v", err)
		}
		if len(*comments) != 1 {
			t.Fatalf("expected a bead comment for a failed attempt (removed==0), got %d", len(*comments))
		}
		if !strings.Contains((*comments)[0], "removed 0/2") {
			t.Errorf("failed-attempt comment should show 0/2, got: %s", (*comments)[0])
		}
		if len(*events) != 1 {
			t.Errorf("expected 1 audit event, got %d", len(*events))
		}
	})

	t.Run("recording failure is loud", func(t *testing.T) {
		a, _, _ := testAuditor("hq-abc", errors.New("dolt down"))
		err := a.recordCompletion(2, 2, []string{"testdb_1", "testdb_2"})
		if err == nil {
			t.Fatal("expected non-nil error when completion record fails (gt-87a: no silent exit 0)")
		}
		if !strings.Contains(err.Error(), "hq-abc") {
			t.Errorf("error should name the authorizing bead, got: %v", err)
		}
	})

	t.Run("human run without bead logs event only", func(t *testing.T) {
		a, comments, events := testAuditor("", nil)
		if err := a.recordCompletion(1, 1, []string{"testdb_1"}); err != nil {
			t.Fatalf("no-bead completion must not fail, got: %v", err)
		}
		if len(*comments) != 0 {
			t.Errorf("expected no bead comments without an authorizing bead, got %d", len(*comments))
		}
		if len(*events) != 1 {
			t.Errorf("expected 1 audit event, got %d", len(*events))
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
