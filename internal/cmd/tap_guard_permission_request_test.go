package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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

// polecatEnv is an autonomous polecat session's environment, which is what
// makes the guard deny rather than leave the prompt standing.
var polecatEnv = map[string]string{
	"GT_ROLE":    "gastown/polecats/obsidian",
	"GT_POLECAT": "obsidian",
}

// envGetter reads env the way os.Getenv reads the process environment.
func envGetter(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

// TestPermissionRequestDeniesForAutonomousSession pins the decision the bead
// asks for: an ask raised in a session nobody can answer becomes a denial whose
// message carries a retry formulation that will not ask again (gt-8stz). The
// count assertion fails a version that denies nothing or denies everything.
func TestPermissionRequestDeniesForAutonomousSession(t *testing.T) {
	t.Parallel()
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
			hook := payloadInput(t, bashPermissionPayload(tt.command))
			if !unattendedPromptSession(hook.Cwd, envGetter(polecatEnv)) {
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
	t.Parallel()
	roles := []string{"gastown/crew/sloan", "gastown/witness", "gastown/refinery", "mayor", "deacon", ""}
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			if unattendedPromptRole(role) {
				t.Errorf("attended role %q is treated as unattended — its prompt would be denied", role)
			}
			hook := payloadInput(t, bashPermissionPayload("mkdir -p /tmp/x && cd /tmp/x && touch f"))
			hook.Cwd = "/Users/sloan/gt/gastown/crew"
			if unattendedPromptSession(hook.Cwd, envGetter(map[string]string{"GT_ROLE": role})) {
				t.Errorf("attended role %q treated as unattended — its prompt would be denied", role)
			}
		})
	}
}

// TestUnattendedPromptSessionPrefersRole pins the precedence: a role in the
// environment decides, so a crew member working inside a polecat worktree — or
// a session that inherited a polecat marker from a parent — keeps its prompt.
func TestUnattendedPromptSessionPrefersRole(t *testing.T) {
	t.Parallel()
	crewWithMarker := envGetter(map[string]string{"GT_POLECAT": "obsidian", "GT_ROLE": "gastown/crew/sloan"})
	if unattendedPromptSession("/home/u/gt/rig/polecats/worker/rig", crewWithMarker) {
		t.Error("a crew role was overridden by a polecat marker or by the payload cwd")
	}
	markerOnly := envGetter(map[string]string{"GT_POLECAT": "obsidian"})
	if !unattendedPromptSession("/home/u/gt/rig/polecats/worker/rig", markerOnly) {
		t.Error("a polecat worktree cwd is unattended when no role says otherwise")
	}
	if unattendedPromptSession("/Users/sloan/gt/gastown/crew", envGetter(nil)) {
		t.Error("a crew directory with no polecat signal is unattended")
	}
}

// TestUnattendedPromptRole pins both directions of the role split: the roles
// the town leaves alone deny, and everything else keeps its prompt.
func TestUnattendedPromptRole(t *testing.T) {
	t.Parallel()
	unattended := []string{"gastown/polecats/obsidian", "polecat"}
	attended := []string{"gastown/crew/sloan", "gastown/witness", "gastown/refinery", "mayor", "deacon", ""}
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
	t.Parallel()
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
	t.Parallel()
	hook := payloadInput(t, bashPermissionPayload("cd /tmp/x && rm -rf *"))
	var buf strings.Builder
	writePermissionRequestDenial(&buf, hook, " Escalation: recorded.")
	out := buf.String()

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

// fakeParkedPromptEscalator is a parkedPromptEscalator that records each call
// and answers result, so a test can verify escalateParkedPromptOnce reaches
// (or does not reach) the escalation call without ever touching the town's
// real beads (gt-8stz review, finding d1058e444298).
func fakeParkedPromptEscalator(result bool) (parkedPromptEscalator, *[]string) {
	var sent []string
	return func(subject, body string) bool {
		sent = append(sent, subject+"\n"+body)
		return result
	}, &sent
}

// TestPermissionRequestEscalatesOncePerShape keeps the escalation off the Dolt
// churn path: one report per session and shape, however often the agent
// retries the denied call.
func TestPermissionRequestEscalatesOncePerShape(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	escalator, sent := fakeParkedPromptEscalator(true)
	hook := payloadInput(t, bashPermissionPayload("cd /tmp/x && rm -rf *"))

	first, escalated := escalateParkedPromptOnce(hook, tmp, escalator)
	if !escalated {
		t.Fatalf("first call did not report an escalation: %q", first)
	}
	if strings.Contains(first, "already reported") {
		t.Fatalf("first call reported itself as a repeat: %q", first)
	}
	if _, err := os.Stat(promptEscalationMarker(hook, promptShape(hook), tmp)); err != nil {
		t.Fatalf("no marker written for the first call: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("fake escalator received %d calls, want 1: %v", len(*sent), *sent)
	}
	second, escalated := escalateParkedPromptOnce(hook, tmp, escalator)
	if !strings.Contains(second, "already reported") {
		t.Errorf("second call did not reuse the marker: %q", second)
	}
	if escalated {
		t.Errorf("repeat call reported an escalation; the steer would be written twice")
	}
	if len(*sent) != 1 {
		t.Errorf("repeat call reached the escalator again: %v", *sent)
	}
	// A different shape in the same session is a different incident.
	other := payloadInput(t, bashPermissionPayload("cd /tmp/x && rm -rf *"))
	other.ToolInput.Command = `rm -rf /tmp/x/*`
	if got, _ := escalateParkedPromptOnce(other, tmp, escalator); strings.Contains(got, "already reported") {
		t.Errorf("second shape was suppressed by the first shape's marker: %q", got)
	}
	if len(*sent) != 2 {
		t.Errorf("second shape did not reach the escalator: %v", *sent)
	}
}

// TestPermissionRequestEscalationFailureMessage pins the failure-message
// distinction the review asked for: a failed escalation must not read like no
// escalation was attempted at all (gt-8stz review, finding 4ce05cf3cf09).
func TestPermissionRequestEscalationFailureMessage(t *testing.T) {
	t.Parallel()
	escalator, _ := fakeParkedPromptEscalator(false)
	hook := payloadInput(t, bashPermissionPayload("cd /tmp/x && rm -rf *"))

	got, escalated := escalateParkedPromptOnce(hook, t.TempDir(), escalator)
	if !strings.Contains(got, "Recording the escalation failed") {
		t.Errorf("escalation failure did not surface a distinct message: %q", got)
	}
	if escalated {
		t.Errorf("a failed escalation reported itself as raised")
	}
}

// TestPermissionRequestEscalationBodyRedactsSecrets pins the mail-safety
// requirement: a token-shaped substring in the denied command must never
// reach the mayor's mail unredacted, and the raw command itself must not
// appear at all (gt-8stz review, finding 36adb619b71a).
func TestPermissionRequestEscalationBodyRedactsSecrets(t *testing.T) {
	t.Parallel()
	token := "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	command := `curl -H "Authorization: Bearer ` + token + `" https://example.com`
	hook := payloadInput(t, bashPermissionPayload(command))

	body := promptEscalationBody(hook, promptShape(hook))
	if strings.Contains(body, token) {
		t.Fatalf("escalation body leaked the raw token: %q", body)
	}
	if strings.Contains(body, command) {
		t.Fatalf("escalation body carried the raw command text: %q", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("escalation body does not show a redaction marker: %q", body)
	}
}

// TestPermissionRequestEscalationMarkerSessionFallback pins the rate-limit
// fix: an empty session id must not collapse every session's dedup key onto
// one shared marker (gt-8stz review, finding 8de95c69660d).
func TestPermissionRequestEscalationMarkerSessionFallback(t *testing.T) {
	t.Parallel()
	shape := "bash cd-compound-write"
	a := permissionRequestInput{SessionID: "", Cwd: "/Users/sloan/gt/gastown/polecats/alpha/gastown", ToolName: "Bash"}
	b := permissionRequestInput{SessionID: "", Cwd: "/Users/sloan/gt/gastown/polecats/beta/gastown", ToolName: "Bash"}
	if promptEscalationMarker(a, shape, "/tmp") == promptEscalationMarker(b, shape, "/tmp") {
		t.Error("two sessions with no session_id but different cwds collapsed onto one marker")
	}
}

// TestPermissionRequestShapeLabels pins the labels the mail subject and the
// rate-limit key are built from.
func TestPermissionRequestShapeLabels(t *testing.T) {
	t.Parallel()
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

// TestPermissionRequestTreatsMonitorAsShell pins gt-nol0q: Monitor carries a
// shell command like Bash, so its denial names the command and steers the retry
// by command shape rather than falling to the non-shell wording.
func TestPermissionRequestTreatsMonitorAsShell(t *testing.T) {
	t.Parallel()
	raw := strings.Replace(bashPermissionPayload("cd /tmp/w && touch f"), `"tool_name":"Bash"`, `"tool_name":"Monitor"`, 1)
	hook := payloadInput(t, raw)
	if hook.ToolName != "Monitor" {
		t.Fatalf("payload tool = %q, want Monitor", hook.ToolName)
	}
	message := promptDeniedMessage(hook, "")
	for _, want := range []string{"the Monitor command", "cd /tmp/w && touch f", "Retry without cd"} {
		if !strings.Contains(message, want) {
			t.Errorf("message missing %q: %q", want, message)
		}
	}
	if got := promptShape(hook); got != "bash cd-compound-write" {
		t.Errorf("promptShape = %q, want bash cd-compound-write", got)
	}
}

// fakeParkedPromptSteer records each steer and answers err, so a test can
// verify the guard reaches (or does not reach) the bead comment without a
// town store.
func fakeParkedPromptSteer(err error) (parkedPromptSteerFunc, *[]string) {
	var sent []string
	return func(_ context.Context, seat, text string) error {
		sent = append(sent, seat+"\n"+text)
		return err
	}, &sent
}

// permissionRequestProc is an autonomous polecat session whose markers live in
// a test temp dir.
func permissionRequestProc(t *testing.T) guardProcess {
	t.Helper()
	return fakeGuardProcess(map[string]string{
		"GT_ROLE":    "gastown/polecats/obsidian",
		"GT_POLECAT": "obsidian",
		"TMPDIR":     t.TempDir(),
	}, "/home/u/gt/gastown/polecats/obsidian/gastown")
}

// TestPermissionRequestSteersTheActiveBeadOncePerShape pins the bead's rule:
// a parked-prompt denial leaves the worktree steer on the session's active
// bead (named with the denied shape), and the existing escalation marker keeps
// it to one comment per shape (gt-cg96w).
func TestPermissionRequestSteersTheActiveBeadOncePerShape(t *testing.T) {
	t.Parallel()
	proc := permissionRequestProc(t)
	escalator, _ := fakeParkedPromptEscalator(true)
	steer, steered := fakeParkedPromptSteer(nil)

	payload := bashPermissionPayload("cd /tmp/x && rm -rf *")
	for i := 0; i < 2; i++ {
		if err := tapGuardPermissionRequest(strings.NewReader(payload), io.Discard, proc, escalator, steer); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	if len(*steered) != 1 {
		t.Fatalf("steer written %d times for one shape, want 1: %v", len(*steered), *steered)
	}
	if !strings.Contains((*steered)[0], "gastown/polecats/obsidian") {
		t.Errorf("steer is not addressed to the session's seat: %q", (*steered)[0])
	}
	for _, want := range []string{"bash cd-compound-write", "outside the worktree", "under ~/gt", "never /tmp"} {
		if !strings.Contains((*steered)[0], want) {
			t.Errorf("steer missing %q: %q", want, (*steered)[0])
		}
	}

	// A different shape is a different incident and earns its own comment.
	other := bashPermissionPayload(`rm -rf /tmp/x/*`)
	if err := tapGuardPermissionRequest(strings.NewReader(other), io.Discard, proc, escalator, steer); err != nil {
		t.Fatalf("second shape: %v", err)
	}
	if len(*steered) != 2 {
		t.Fatalf("second shape did not get its own steer: %v", *steered)
	}
	if !strings.Contains((*steered)[1], "bash rm-glob") {
		t.Errorf("second steer does not name its shape: %q", (*steered)[1])
	}
}

// TestPermissionRequestSteerSkippedWithoutAnActiveBead pins the store half: a
// seat with no active bead leaves the comment unwritten and reports no error.
func TestPermissionRequestSteerSkippedWithoutAnActiveBead(t *testing.T) {
	t.Parallel()
	var commented []string
	comment := func(id, text string) error {
		commented = append(commented, id)
		return nil
	}

	if err := steerParkedPromptBead("gastown/polecats/obsidian", "steer", func(string) string { return "" }, comment); err != nil {
		t.Fatalf("no active bead: %v", err)
	}
	if len(commented) != 0 {
		t.Fatalf("commented on %v with no active bead", commented)
	}

	if err := steerParkedPromptBead("gastown/polecats/obsidian", "steer", func(string) string { return "gt-abc" }, comment); err != nil {
		t.Fatalf("active bead: %v", err)
	}
	if len(commented) != 1 || commented[0] != "gt-abc" {
		t.Fatalf("commented on %v, want [gt-abc]", commented)
	}
}

// TestPermissionRequestSteerFailureIsNonFatal pins the other half of the rule:
// a bead write that fails changes neither the deny nor the escalation.
func TestPermissionRequestSteerFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	proc := permissionRequestProc(t)
	escalator, sent := fakeParkedPromptEscalator(true)
	steer, steered := fakeParkedPromptSteer(errors.New("bd comment failed"))

	var out bytes.Buffer
	payload := bashPermissionPayload("cd /tmp/x && rm -rf *")
	if err := tapGuardPermissionRequest(strings.NewReader(payload), &out, proc, escalator, steer); err != nil {
		t.Fatalf("a failed steer changed the guard result: %v", err)
	}
	if len(*steered) != 1 {
		t.Fatalf("steer was not attempted: %v", *steered)
	}
	if len(*sent) != 1 {
		t.Fatalf("failed steer changed the escalation: %v", *sent)
	}
	if !strings.Contains(out.String(), `"behavior":"deny"`) {
		t.Errorf("failed steer changed the denial: %q", out.String())
	}
}

// TestPermissionRequestSteerIsBounded pins the hook-budget rule: the store
// call runs under the steer's deadline, so the wait that bound the call ends
// it too and nothing writes after the hook returns (gt-cg96w review, finding
// baac2cfec612). A zero timeout is a deadline already passed when the steer
// starts, which drives that path without a wall-clock wait.
func TestPermissionRequestSteerIsBounded(t *testing.T) {
	t.Parallel()
	var storeCtx context.Context
	unbounded := false
	blocked := func(ctx context.Context, _, _ string) error {
		storeCtx = ctx
		done := ctx.Done()
		if done == nil {
			// No deadline at all: record it and return instead of blocking, so
			// the assertion below is the failure rather than a hung test.
			unbounded = true
			return nil
		}
		<-done
		return ctx.Err()
	}

	commentParkedPromptSteerWithin(context.Background(), "gastown/polecats/obsidian", "steer", blocked, 0)
	if unbounded {
		t.Fatalf("the steer ran with no deadline, so a slow store would hold the denial open")
	}
	if err := storeCtx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("steer context after the call = %v, want the deadline already passed", err)
	}
}

// TestPermissionRequestSteerSkipsWithoutASeatOrSteer pins the two inputs that
// leave no comment to write: a session with no seat, and a guard built with no
// steer at all. Neither may reach the store.
func TestPermissionRequestSteerSkipsWithoutASeatOrSteer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		seat    string
		noSteer bool
	}{
		{"no seat", "", false},
		{"no steer", "gastown/polecats/obsidian", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			steer := parkedPromptSteerFunc(func(context.Context, string, string) error {
				calls++
				return nil
			})
			if tt.noSteer {
				steer = nil
			}
			commentParkedPromptSteerWithin(context.Background(), tt.seat, "steer", steer, time.Hour)
			if calls != 0 {
				t.Fatalf("steer ran %d times, want 0", calls)
			}
		})
	}
}

// TestPermissionRequestSteerReportsAnUnreadableTown pins the last failure
// path: a town lookup that fails surfaces as an error for the caller to drop.
func TestPermissionRequestSteerReportsAnUnreadableTown(t *testing.T) {
	t.Parallel()
	townFailure := errors.New("no town root")
	err := steerParkedPromptInTown(context.Background(), "gastown/polecats/obsidian", "steer",
		func() (string, error) { return "", townFailure },
		func(context.Context, string, string) parkedPromptStore {
			t.Fatal("an unreadable town still opened a store")
			return parkedPromptStore{}
		})
	if !errors.Is(err, townFailure) {
		t.Fatalf("steer in an unreadable town = %v, want %v", err, townFailure)
	}
}

// TestPermissionRequestSteerResolvesTheSeatRigStore pins the store the steer
// opens: the seat's own rig under the town, opened within the steer's context,
// and the comment on the bead that read named (gt-cg96w review, finding
// 845391f18744).
func TestPermissionRequestSteerResolvesTheSeatRigStore(t *testing.T) {
	t.Parallel()
	type deadlineKey struct{}
	ctx := context.WithValue(context.Background(), deadlineKey{}, "steer deadline")

	var openedTown, openedRig string
	var openedCtx context.Context
	var commented []string
	open := func(ctx context.Context, townRoot, rig string) parkedPromptStore {
		openedTown, openedRig, openedCtx = townRoot, rig, ctx
		return parkedPromptStore{
			active: func(seat string) string {
				if seat != "beads/polecats/obsidian" {
					t.Errorf("active bead read for seat %q, want the session's seat", seat)
				}
				return "gt-abc"
			},
			comment: func(id, text string) error {
				commented = append(commented, id+"\n"+text)
				return nil
			},
		}
	}
	if err := steerParkedPromptInTown(ctx, "beads/polecats/obsidian", "steer text",
		func() (string, error) { return "/home/u/gt", nil }, open); err != nil {
		t.Fatalf("steer: %v", err)
	}
	if openedTown != "/home/u/gt" || openedRig != "beads" {
		t.Errorf("store opened at town %q rig %q, want /home/u/gt and beads", openedTown, openedRig)
	}
	if openedCtx.Value(deadlineKey{}) != "steer deadline" {
		t.Errorf("store opened outside the steer's context, so its deadline does not bound bd")
	}
	if len(commented) != 1 || commented[0] != "gt-abc\nsteer text" {
		t.Errorf("comments = %v, want the steer on gt-abc", commented)
	}
}

// TestPermissionRequestSteerBindsTheDeadlineToTheStore pins the line that
// makes the bound real: the store's bd calls run under the steer's context, so
// the deadline that ends the wait also kills the write still in flight
// (gt-cg96w review, finding baac2cfec612).
func TestPermissionRequestSteerBindsTheDeadlineToTheStore(t *testing.T) {
	t.Parallel()
	type ctxKey struct{}

	ctx := context.WithValue(context.Background(), ctxKey{}, "steer deadline")
	var ranUnder context.Context
	var argv []string
	runner := func(ctx context.Context, args []string) ([]byte, []byte, error) {
		ranUnder, argv = ctx, args
		return []byte(`{}`), nil, nil
	}

	store := openParkedPromptStore(ctx, t.TempDir(), "gastown", beads.WithRunner(runner))
	if err := store.comment("gt-abc", "steer text"); err != nil {
		t.Fatalf("comment through the fake runner: %v", err)
	}
	if ranUnder == nil || ranUnder.Value(ctxKey{}) != "steer deadline" {
		t.Fatalf("bd ran outside the steer's context, so the deadline does not bound the write")
	}
	if got := strings.Join(argv, " "); !strings.Contains(got, "comments add gt-abc") {
		t.Errorf("bd argv = %q, want the comment on gt-abc", got)
	}
}

// TestPermissionRequestSteerRejectsASeatOutsideTheTown pins the guard on the
// one input that becomes a path: GT_ROLE is the session's environment, so a
// seat that would resolve the store outside the town must open nothing
// (gt-cg96w review, finding c5e71b55f417).
func TestPermissionRequestSteerRejectsASeatOutsideTheTown(t *testing.T) {
	t.Parallel()
	seats := []string{
		"",
		"gastown",
		"gastown/polecats",
		"gastown/polecats/",
		"gastown/polecats/obsidian/x",
		"gastown/polecats/..",
		"gastown/polecats/../../../etc",
		"../poison/polecats/obsidian",
		"gastown/crew/sloan",
	}
	for _, seat := range seats {
		opened := false
		open := func(context.Context, string, string) parkedPromptStore {
			opened = true
			return parkedPromptStore{}
		}
		err := steerParkedPromptInTown(context.Background(), seat, "steer",
			func() (string, error) { return "/home/u/gt", nil }, open)
		if err == nil {
			t.Errorf("seat %q was accepted", seat)
		}
		if opened {
			t.Errorf("seat %q still opened a store", seat)
		}
	}
}

// TestPromptSeat pins how the seat is named: the role when it is a polecat,
// the worktree path otherwise, and nothing for a session that is not a polecat.
func TestPromptSeat(t *testing.T) {
	t.Parallel()
	worktree := "/home/u/gt/gastown/polecats/obsidian/gastown"
	tests := []struct {
		name string
		env  map[string]string
		cwd  string
		want string
	}{
		{"role names the seat", map[string]string{"GT_ROLE": "gastown/polecats/obsidian"}, worktree, "gastown/polecats/obsidian"},
		{"bare polecat role falls back to cwd", map[string]string{"GT_ROLE": "polecat"}, worktree, "gastown/polecats/obsidian"},
		{"no role reads the cwd", nil, worktree, "gastown/polecats/obsidian"},
		{"crew role is not a polecat seat", map[string]string{"GT_ROLE": "gastown/crew/sloan"}, worktree, ""},
		{"cwd outside the polecats tree names no seat", nil, "/home/u/gt/gastown/crew", ""},
		{"role escaping the town names no seat", map[string]string{"GT_ROLE": "../poison/polecats/obsidian"}, worktree, ""},
		{"cwd with a dot-dot rig names no seat", nil, "/home/u/gt/../polecats/obsidian/gastown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := promptSeat(tt.cwd, envGetter(tt.env)); got != tt.want {
				t.Errorf("promptSeat(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
		})
	}
}
