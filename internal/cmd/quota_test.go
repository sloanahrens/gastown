package cmd

import (
	"encoding/json"
	"testing"

	"github.com/steveyegge/gastown/internal/quota"
)

// gt-omg1: quota_dog's sentinel check treats a bare "null" as a non-empty
// rotation result and logs it every cycle. With no resume candidates,
// runResumeNudges must return a non-nil slice so it marshals to "[]".
func TestRunResumeNudges_EmptyCandidatesMarshalsToEmptyArray(t *testing.T) {
	results := runResumeNudges(nil, nil, true)
	if results == nil {
		t.Fatal("runResumeNudges(nil candidates) returned nil, want non-nil empty slice")
	}
	if len(results) != 0 {
		t.Fatalf("runResumeNudges(nil candidates) = %v, want empty", results)
	}

	out, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}
	if string(out) != "[]" {
		t.Errorf("json.Marshal(results) = %q, want %q", out, "[]")
	}
}

func TestRunResumeNudges_WithCandidatesDryRun(t *testing.T) {
	candidates := []quota.ResumeCandidate{
		{Session: "rig/witness", ResetsAt: "5pm"},
	}
	results := runResumeNudges(nil, candidates, true)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Session != "rig/witness" || !results[0].Resumed {
		t.Errorf("unexpected result: %+v", results[0])
	}
}
