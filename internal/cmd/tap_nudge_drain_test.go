package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/nudge"
)

// TestTapNudgeDrainSurfacesQueuedNudgeOnce is the gt-drwph regression: a nudge
// queued for a running polecat (the overrun guard's split prompt) must reach
// the model at the PostToolUse boundary as additionalContext, and must not
// come back on the next tool call.
func TestTapNudgeDrainSurfacesQueuedNudgeOnce(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-furiosa"
	townRoot := fakeTownRoot(t)

	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		Sender:  "daemon",
		Message: "write a split proposal",
	}); err != nil {
		t.Fatalf("enqueue nudge: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := tapNudgeDrain(&stdout, &stderr, townRoot, session); err != nil {
		t.Fatalf("tapNudgeDrain: %v", err)
	}

	var out nudgeDrainOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not the PostToolUse JSON the harness reads: %v\n%s", err, stdout.String())
	}
	if got := out.HookSpecificOutput.HookEventName; got != "PostToolUse" {
		t.Errorf("hookEventName = %q, want PostToolUse", got)
	}
	if got := out.HookSpecificOutput.AdditionalContext; !strings.Contains(got, "write a split proposal") {
		t.Errorf("additionalContext missing the nudge: %q", got)
	}
	if left := nudge.QueueLen(townRoot, session); left != 0 {
		t.Errorf("nudge queue still holds %d entr(ies) after the drain", left)
	}

	// The next tool call finds an empty queue and says nothing.
	stdout.Reset()
	if err := tapNudgeDrain(&stdout, &stderr, townRoot, session); err != nil {
		t.Fatalf("second tapNudgeDrain: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("a delivered nudge was shown again: %q", stdout.String())
	}
}

// TestTapNudgeDrainLeavesOtherSessionsQueued pins the scoping: a seat drains
// only its own session's queue.
func TestTapNudgeDrainLeavesOtherSessionsQueued(t *testing.T) {
	t.Parallel()
	townRoot := fakeTownRoot(t)

	if err := nudge.Enqueue(townRoot, "gt-gastown-nux", nudge.QueuedNudge{
		Sender:  "daemon",
		Message: "for nux",
	}); err != nil {
		t.Fatalf("enqueue nudge: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := tapNudgeDrain(&stdout, &stderr, townRoot, "gt-gastown-furiosa"); err != nil {
		t.Fatalf("tapNudgeDrain: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("drained another session's nudge: %q", stdout.String())
	}
	if left := nudge.QueueLen(townRoot, "gt-gastown-nux"); left != 1 {
		t.Errorf("nux queue holds %d entr(ies), want 1", left)
	}
}

// TestTapNudgeDrainSilentWhenQueueEmpty pins the common case: the hook runs
// after every tool call and must print nothing when nothing is queued.
func TestTapNudgeDrainSilentWhenQueueEmpty(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := tapNudgeDrain(&stdout, &stderr, fakeTownRoot(t), "gt-gastown-furiosa"); err != nil {
		t.Fatalf("tapNudgeDrain: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("empty queue produced output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
