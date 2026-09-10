package cmd

import (
	"encoding/json"
	"testing"

	"github.com/steveyegge/gastown/internal/quota"
	ttmux "github.com/steveyegge/gastown/internal/tmux"
)

// TestScanForResume_NoAccountsConfigured guards gt-749e: resume scanning
// must work with a nil accounts config — the situation on a town with no
// accounts.json at all (never ran 'gt account add'). ttmux.NewTmux() with
// no running tmux server degrades to zero sessions rather than erroring
// (see ttmux.Tmux.ListSessions' ErrNoServer handling), so this exercises
// the real code path end to end without needing a live tmux server.
func TestScanForResume_NoAccountsConfigured(t *testing.T) {
	tmux := ttmux.NewTmuxWithSocket("gt-749e-test-no-such-socket")
	results, candidates, err := scanForResume(tmux, nil)
	if err != nil {
		t.Fatalf("scanForResume(nil accounts) returned error: %v", err)
	}
	if len(results) != 0 || len(candidates) != 0 {
		t.Errorf("expected no results/candidates against an empty tmux server, got results=%v candidates=%v", results, candidates)
	}
}

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
