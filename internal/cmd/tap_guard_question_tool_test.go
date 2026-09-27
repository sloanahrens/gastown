package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// questionToolPayload is the hook payload shape Claude Code sends for a
// PreToolUse event on the question tool.
func questionToolPayload(cwd string) string {
	payload := map[string]any{
		"session_id":      "test-session",
		"cwd":             cwd,
		"tool_name":       askUserQuestionTool,
		"hook_event_name": "PreToolUse",
		"tool_input": map[string]any{
			"questions": []map[string]any{{
				"question": "n/a",
				"header":   "n/a",
				"options": []map[string]any{
					{"label": "a", "description": "a"},
					{"label": "b", "description": "b"},
				},
			}},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// runQuestionToolGuard invokes the guard exactly as the PreToolUse hook does:
// a payload on stdin, the returned error as the verdict (a non-nil
// *SilentExitError is exit 2, which Claude Code reads as BLOCK).
func runQuestionToolGuard(t *testing.T, payload string) error {
	t.Helper()
	var err error
	withStdin(t, payload, func() {
		err = runTapGuardQuestionTool(tapGuardQuestionToolCmd, nil)
	})
	return err
}

// TestQuestionToolGuardBlocksUnattendedSession is the live-block leg for
// gt-163k8: the degenerate placeholder question a polecat raises to park
// itself ('n/a', options a/b) must be blocked in a session with nobody at the
// pane, and the block reason must leave a way forward.
func TestQuestionToolGuardBlocksUnattendedSession(t *testing.T) {
	const worktree = "/home/u/gt/gastown/polecats/garnet/gastown"
	t.Setenv("GT_ROLE", "gastown/polecats/garnet")
	t.Setenv("GT_POLECAT", "garnet")

	err := runQuestionToolGuard(t, questionToolPayload(worktree))
	if err == nil {
		t.Fatal("expected the question tool to be blocked in a polecat session, got nil error")
	}
	if code, ok := IsSilentExit(err); !ok || code != 2 {
		t.Fatalf("expected a silent exit 2, got %v (ok=%v)", err, ok)
	}

	reason := questionToolDenial()
	if !strings.Contains(reason, "nobody at the pane") {
		t.Errorf("reason does not say why the question cannot be answered: %q", reason)
	}
	if !strings.Contains(reason, askUserQuestionTool) {
		t.Errorf("reason does not name the denied tool: %q", reason)
	}
	// The deny removes the polecat's only interactive channel, so it is only
	// a fix if it names the channel that does reach a human.
	if !strings.Contains(reason, "gt escalate") {
		t.Errorf("reason does not name escalation as the way forward: %q", reason)
	}
}

// TestQuestionToolGuardBlocksDogSession pins the other unattended role. A dog's
// GT_ROLE is the bare "dog", which the shared unattended-session check resolves
// through constants.RoleDog.
func TestQuestionToolGuardBlocksDogSession(t *testing.T) {
	for _, role := range []string{"dog", "deacon/dogs/alpha"} {
		t.Run(role, func(t *testing.T) {
			t.Setenv("GT_ROLE", role)
			t.Setenv("GT_POLECAT", "")

			err := runQuestionToolGuard(t, questionToolPayload("/home/u/gt/deacon/dogs/alpha"))
			if err == nil {
				t.Fatalf("expected the question tool to be blocked for role %q", role)
			}
			if code, ok := IsSilentExit(err); !ok || code != 2 {
				t.Fatalf("expected a silent exit 2 for role %q, got %v", role, err)
			}
		})
	}
}

// TestQuestionToolGuardKeepsQuestionForInteractiveSession is the other half:
// a session with a person at the pane keeps the tool, so the guard reports no
// verdict for every attended role.
func TestQuestionToolGuardKeepsQuestionForInteractiveSession(t *testing.T) {
	roles := []string{"gastown/crew/sloan", "gastown/witness", "gastown/refinery", "mayor", "deacon", "boot", ""}
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			t.Setenv("GT_ROLE", role)
			t.Setenv("GT_POLECAT", "")

			err := runQuestionToolGuard(t, questionToolPayload("/Users/sloan/gt/gastown/crew"))
			if err != nil {
				t.Errorf("attended role %q had its question denied: %v", role, err)
			}
		})
	}
}

// TestQuestionToolGuardAllowsOtherTools pins the guard to its one tool: a
// polecat session must be able to call anything else, or the deny becomes a
// session-wide wedge.
func TestQuestionToolGuardAllowsOtherTools(t *testing.T) {
	t.Setenv("GT_ROLE", "gastown/polecats/garnet")
	t.Setenv("GT_POLECAT", "garnet")

	for _, tool := range []string{"Bash", "Edit", "Read", "Task"} {
		payload := strings.Replace(questionToolPayload("/home/u/gt/gastown/polecats/garnet/gastown"),
			`"tool_name":"`+askUserQuestionTool+`"`, `"tool_name":"`+tool+`"`, 1)
		if err := runQuestionToolGuard(t, payload); err != nil {
			t.Errorf("tool %q was denied in a polecat session: %v", tool, err)
		}
	}
}

// TestQuestionToolGuardHoldsForUnparsablePayload: a payload this guard cannot
// read is not grounds to deny a call, so the session is left alone rather than
// wedged.
func TestQuestionToolGuardHoldsForUnparsablePayload(t *testing.T) {
	t.Setenv("GT_ROLE", "gastown/polecats/garnet")
	t.Setenv("GT_POLECAT", "garnet")

	for _, raw := range []string{"", "not json"} {
		if err := runQuestionToolGuard(t, raw); err != nil {
			t.Errorf("payload %q produced a verdict: %v", raw, err)
		}
	}
}
