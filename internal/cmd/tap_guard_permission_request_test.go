package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// bashPermissionPayload is the hook payload shape Claude Code sends for a Bash
// permission request, with the command under test substituted in.
func bashPermissionPayload(command string) string {
	payload := map[string]any{
		"session_id":      "test-session",
		"cwd":             "/home/u/gt/gastown/polecats/obsidian/gastown",
		"tool_name":       "Bash",
		"hook_event_name": "PermissionRequest",
		"tool_input":      map[string]any{"command": command},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// payloadInput decodes a payload the way the guard does, so the decision tests
// exercise the same parse path the command uses.
func payloadInput(t *testing.T, raw string) permissionRequestInput {
	t.Helper()
	var hook permissionRequestInput
	if err := json.Unmarshal([]byte(raw), &hook); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	return hook
}

// asPolecat marks the process env as an autonomous polecat session, which is
// what makes the guard deny rather than leave the prompt standing.
func asPolecat(t *testing.T) {
	t.Helper()
	t.Setenv("GT_ROLE", "gastown/polecats/obsidian")
	t.Setenv("GT_POLECAT", "obsidian")
}

// TestPermissionRequestDeniesForAutonomousSession pins the decision the bead
// asks for: an ask raised in a session nobody can answer becomes a denial whose
// message carries a retry formulation that will not ask again (gt-8stz). The
// count assertion fails a version that denies nothing or denies everything.
func TestPermissionRequestDeniesForAutonomousSession(t *testing.T) {
	tests := []struct {
		name         string
		command      string
		wantGuidance string
	}{
		{
			name:         "cd compound write, the live park",
			command:      "mkdir -p /tmp/test-initbeads && cd /tmp/test-initbeads && rm -rf * && go test -v -run TestInitBeadsWritesConfigOnFailure ./internal/rig/... 2>&1",
			wantGuidance: "Retry without cd",
		},
		{
			name:         "glob removal, the second live park",
			command:      `rm -rf /Users/sloan/gt/gastown/polecats/obsidian/gastown/*`,
			wantGuidance: "paths named explicitly",
		},
		{
			name:         "unrecognised shape",
			command:      "git push origin main",
			wantGuidance: "absolute paths and no cd",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asPolecat(t)
			hook := payloadInput(t, bashPermissionPayload(tt.command))
			if !unattendedPromptSession(hook.Cwd) {
				t.Fatalf("polecat session not recognised as unattended")
			}
			message := promptDeniedMessage(hook, "")
			if !strings.Contains(message, "nobody at the pane") {
				t.Errorf("message does not say why the ask cannot be answered: %q", message)
			}
			if !strings.Contains(message, tt.wantGuidance) {
				t.Errorf("message does not name the retry formulation %q: %q", tt.wantGuidance, message)
			}
			if !strings.Contains(message, tt.command) && !strings.Contains(message, "Bash command") {
				t.Errorf("message does not name what was denied: %q", message)
			}
		})
	}
}

// TestPermissionRequestKeepsPromptForInteractiveSession is the other half of
// the bead's requirement: a session with a person at the pane keeps its
// prompt, so the guard reports no decision for every attended role.
func TestPermissionRequestKeepsPromptForInteractiveSession(t *testing.T) {
	roles := []string{"gastown/crew/sloan", "gastown/witness", "gastown/refinery", "mayor", "deacon", "boot", ""}
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			if unattendedPromptRole(role) {
				t.Errorf("attended role %q is treated as unattended — its prompt would be denied", role)
			}
			t.Setenv("GT_ROLE", role)
			t.Setenv("GT_POLECAT", "")
			hook := payloadInput(t, bashPermissionPayload("mkdir -p /tmp/x && cd /tmp/x && touch f"))
			hook.Cwd = "/Users/sloan/gt/gastown/crew"
			if unattendedPromptSession(hook.Cwd) {
				t.Errorf("attended role %q treated as unattended — its prompt would be denied", role)
			}
		})
	}
}

// TestUnattendedPromptSessionPrefersRole pins the precedence: a role in the
// environment decides, so a crew member working inside a polecat worktree — or
// a session that inherited a polecat marker from a parent — keeps its prompt.
func TestUnattendedPromptSessionPrefersRole(t *testing.T) {
	t.Setenv("GT_POLECAT", "obsidian")
	t.Setenv("GT_ROLE", "gastown/crew/sloan")
	if unattendedPromptSession("/home/u/gt/rig/polecats/worker/rig") {
		t.Error("a crew role was overridden by a polecat marker or by the payload cwd")
	}
	t.Setenv("GT_ROLE", "")
	if !unattendedPromptSession("/home/u/gt/rig/polecats/worker/rig") {
		t.Error("a polecat worktree cwd is unattended when no role says otherwise")
	}
	t.Setenv("GT_POLECAT", "")
	if unattendedPromptSession("/Users/sloan/gt/gastown/crew") {
		t.Error("a crew directory with no polecat signal is unattended")
	}
}

// TestUnattendedPromptRole pins both directions of the role split: the roles
// the town leaves alone deny, and everything else keeps its prompt.
func TestUnattendedPromptRole(t *testing.T) {
	unattended := []string{"gastown/polecats/obsidian", "polecat", "dog", "deacon/dogs/alpha"}
	attended := []string{"gastown/crew/sloan", "gastown/witness", "gastown/refinery", "mayor", "deacon", "boot", ""}
	for _, role := range unattended {
		if !unattendedPromptRole(role) {
			t.Errorf("role %q must be treated as unattended", role)
		}
	}
	for _, role := range attended {
		if unattendedPromptRole(role) {
			t.Errorf("role %q must keep its prompt", role)
		}
	}
}

// TestPermissionRequestHoldsForUnparsablePayload: a payload this guard cannot
// read is not grounds to deny a call, so the prompt flow stays untouched.
func TestPermissionRequestHoldsForUnparsablePayload(t *testing.T) {
	for _, raw := range []string{"", "not json", "[1,2,3]"} {
		var hook permissionRequestInput
		err := json.Unmarshal([]byte(raw), &hook)
		if err == nil && hook.ToolName != "" {
			t.Errorf("payload %q was read as tool %q; the guard must not decide", raw, hook.ToolName)
		}
	}
}

// TestPermissionRequestDenialShape pins the wire contract: the harness reads
// the decision off hookSpecificOutput, and a wrong shape parks the session
// silently rather than failing loudly.
func TestPermissionRequestDenialShape(t *testing.T) {
	asPolecat(t)
	hook := payloadInput(t, bashPermissionPayload("cd /tmp/x && rm -rf *"))
	out := captureStdout(t, func() {
		writePermissionRequestDenial(hook, " Escalation: recorded.")
	})

	var decoded struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &decoded); err != nil {
		t.Fatalf("denial is not one JSON object on stdout (%v): %q", err, out)
	}
	if got := decoded.HookSpecificOutput.HookEventName; got != "PermissionRequest" {
		t.Errorf("hookEventName = %q, want PermissionRequest", got)
	}
	if got := decoded.HookSpecificOutput.Decision.Behavior; got != "deny" {
		t.Errorf("decision.behavior = %q, want deny", got)
	}
	if !strings.Contains(decoded.HookSpecificOutput.Decision.Message, "cd") {
		t.Errorf("decision.message lost the guidance: %q", decoded.HookSpecificOutput.Decision.Message)
	}
}

// TestPermissionRequestEscalatesOncePerShape keeps the escalation off the Dolt
// churn path: one report per session and shape, however often the agent
// retries the denied call.
func TestPermissionRequestEscalatesOncePerShape(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	asPolecat(t)
	hook := payloadInput(t, bashPermissionPayload("cd /tmp/x && rm -rf *"))

	first := escalateParkedPromptOnce(hook)
	if strings.Contains(first, "already reported") {
		t.Fatalf("first call reported itself as a repeat: %q", first)
	}
	if _, err := os.Stat(promptEscalationMarker(hook, promptShape(hook))); err != nil {
		t.Fatalf("no marker written for the first call: %v", err)
	}
	if second := escalateParkedPromptOnce(hook); !strings.Contains(second, "already reported") {
		t.Errorf("second call did not reuse the marker: %q", second)
	}
	// A different shape in the same session is a different incident.
	other := payloadInput(t, bashPermissionPayload("cd /tmp/x && rm -rf *"))
	other.ToolInput.Command = `rm -rf /tmp/x/*`
	if got := escalateParkedPromptOnce(other); strings.Contains(got, "already reported") {
		t.Errorf("second shape was suppressed by the first shape's marker: %q", got)
	}
}

// TestPermissionRequestShapeLabels pins the labels the mail subject and the
// rate-limit key are built from.
func TestPermissionRequestShapeLabels(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{`cd /tmp/x && rm -rf *`, "bash cd-compound-write"},
		{`rm -rf /tmp/x/*`, "bash rm-glob"},
		{`go test ./internal/cmd/ -run TestFoo`, "bash"},
	}
	for _, tt := range tests {
		hook := payloadInput(t, bashPermissionPayload(tt.command))
		if got := promptShape(hook); got != tt.want {
			t.Errorf("promptShape(%q) = %q, want %q", tt.command, got, tt.want)
		}
	}
}

