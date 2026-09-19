package witness

import (
	"strings"
	"testing"
	"time"
)

// base is a healthy live observation: transcript written recently, pane hashing
// to a known signature.
func base(observedAt time.Time) RealActivity {
	return RealActivity{
		Session:         "gt-gastown-opal",
		Polecat:         "opal",
		AgentAlive:      true,
		ObservedAt:      observedAt,
		LastActivity:    observedAt.Add(-2 * time.Minute),
		ActivitySource:  ActivitySourceTranscript,
		TranscriptPath:  "/tmp/transcript.jsonl",
		TranscriptBytes: 900_000,
		PaneSignature:   "9f3a1c2b4d5e6f70",
	}
}

// TestAssessStall_LongTurnIsNotAStall is the gt-xb27 regression: the opal case.
// A polecat deep inside one long turn — huge pane elapsed label, but its
// transcript advanced inside the window — is working, and must never be
// reported as stalled.
func TestAssessStall_LongTurnIsNotAStall(t *testing.T) {
	start := time.Now().Add(-40 * time.Minute)
	prev := base(start)

	cur := base(start.Add(40 * time.Minute))
	cur.TranscriptBytes = prev.TranscriptBytes + 8192 // 408 tool calls, a write 17m ago
	cur.LastActivity = start.Add(35 * time.Minute)

	verdict := AssessStall(prev, cur, 30*time.Minute)
	if verdict.Stalled {
		t.Errorf("a working polecat was reported stalled: %s", verdict.Reason)
	}
}

// TestAssessStall_ShortWindowIsInsufficient pins the two-sample rule: a stall
// verdict requires observations at least a full window apart.
func TestAssessStall_ShortWindowIsInsufficient(t *testing.T) {
	start := time.Now().Add(-3 * time.Minute)
	prev := base(start)

	cur := base(start.Add(3 * time.Minute))
	cur.LastActivity = prev.LastActivity // nothing has happened

	verdict := AssessStall(prev, cur, 30*time.Minute)
	if verdict.Stalled {
		t.Errorf("verdict from a 3m window with a 30m requirement: %s", verdict.Reason)
	}
}

// TestAssessStall_DeadAgentIsNotAStall keeps the stall path and the zombie
// path from overlapping: a dead agent is restarted, not "nudged".
func TestAssessStall_DeadAgentIsNotAStall(t *testing.T) {
	start := time.Now().Add(-45 * time.Minute)
	prev := base(start)

	cur := base(start.Add(45 * time.Minute))
	cur.AgentAlive = false

	if verdict := AssessStall(prev, cur, 30*time.Minute); verdict.Stalled {
		t.Errorf("dead agent reported as a stall: %s", verdict.Reason)
	}
}

// TestAssessStall_MissingTranscriptIsNotAStall: absence of evidence is not
// evidence of a stall.
func TestAssessStall_MissingTranscriptIsNotAStall(t *testing.T) {
	start := time.Now().Add(-45 * time.Minute)
	prev := base(start)
	prev.ActivitySource = ActivitySourceNone
	prev.LastActivity = time.Time{}

	cur := base(start.Add(45 * time.Minute))
	cur.ActivitySource = ActivitySourceNone
	cur.LastActivity = time.Time{}

	if verdict := AssessStall(prev, cur, 30*time.Minute); verdict.Stalled {
		t.Errorf("stall verdict with no transcript baseline: %s", verdict.Reason)
	}
}

// TestAssessStall_PaneChangeIsNotAStall covers the agent that is working but
// has not yet flushed a transcript record.
func TestAssessStall_PaneChangeIsNotAStall(t *testing.T) {
	start := time.Now().Add(-45 * time.Minute)
	prev := base(start)

	cur := base(start.Add(45 * time.Minute))
	cur.LastActivity = prev.LastActivity
	cur.PaneSignature = "0011223344556677"

	if verdict := AssessStall(prev, cur, 30*time.Minute); verdict.Stalled {
		t.Errorf("pane change ignored: %s", verdict.Reason)
	}
}

// TestAssessStall_TranscriptRewriteIsAChange: size, not mtime, is compared, so
// a same-second rewrite cannot masquerade as inactivity.
func TestAssessStall_TranscriptRewriteIsAChange(t *testing.T) {
	start := time.Now().Add(-45 * time.Minute)
	prev := base(start)

	cur := base(start.Add(45 * time.Minute))
	cur.LastActivity = prev.LastActivity // mtime unchanged...
	cur.TranscriptBytes = prev.TranscriptBytes + 1

	if verdict := AssessStall(prev, cur, 30*time.Minute); verdict.Stalled {
		t.Errorf("transcript size change ignored: %s", verdict.Reason)
	}
}

// TestAssessStall_PositiveSignal is the case the function exists to certify:
// agreeing evidence from both signals over a full window.
func TestAssessStall_PositiveSignal(t *testing.T) {
	start := time.Now().Add(-45 * time.Minute)
	prev := base(start)

	cur := base(start.Add(45 * time.Minute))
	cur.LastActivity = prev.LastActivity // transcript frozen

	verdict := AssessStall(prev, cur, 30*time.Minute)
	if !verdict.Stalled {
		t.Fatalf("expected a positive stall signal, got: %s", verdict.Reason)
	}
	if verdict.Reason == "" {
		t.Error("positive verdict must explain itself")
	}
}

// TestAssessStall_ZeroWindow guards against a caller that forgets to configure
// a window, which would otherwise make any two samples agree.
func TestAssessStall_ZeroWindow(t *testing.T) {
	now := time.Now()
	prev := base(now.Add(-time.Hour))
	cur := base(now)
	cur.LastActivity = prev.LastActivity

	if verdict := AssessStall(prev, cur, 0); verdict.Stalled {
		t.Errorf("zero window produced a verdict: %s", verdict.Reason)
	}
}

func TestRealActivity_AgeAndStaleCandidate(t *testing.T) {
	now := time.Now()
	act := base(now)
	act.LastActivity = now.Add(-45 * time.Minute)

	age, ok := act.Age(now)
	if !ok || age != 45*time.Minute {
		t.Fatalf("Age() = %v, %v; want 45m, true", age, ok)
	}
	if !act.IsStaleCandidate(now, 30*time.Minute) {
		t.Error("45m of silence should be a stale candidate at a 30m threshold")
	}
	if act.IsStaleCandidate(now, time.Hour) {
		t.Error("45m of silence is not a candidate at a 1h threshold")
	}
}

// TestRealActivity_UnknownActivityIsNeverACandidate: a polecat with no
// transcript must not be nominated for a restart it cannot be judged for.
func TestRealActivity_UnknownActivityIsNeverACandidate(t *testing.T) {
	now := time.Now()
	act := base(now)
	act.LastActivity = time.Time{}
	act.ActivitySource = ActivitySourceNone

	if _, ok := act.Age(now); ok {
		t.Error("Age() reported an age for an unknown last activity")
	}
	if act.IsStaleCandidate(now, time.Minute) {
		t.Error("unknown activity was treated as staleness")
	}
	if got := act.Describe(now); got == "" {
		t.Error("Describe() should say the activity is unknown")
	}
}

func TestRealActivity_DescribeMentionsEvidence(t *testing.T) {
	now := time.Now()
	act := base(now)

	got := act.Describe(now)
	for _, want := range []string{"transcript", act.PaneSignature} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}

	dead := base(now)
	dead.AgentAlive = false
	if got := dead.Describe(now); !strings.Contains(got, "not alive") {
		t.Errorf("Describe() on a dead session = %q", got)
	}
}

// TestTranscriptDatesSession pins the reused-worktree guard: a transcript left
// behind by an earlier run in the same worktree must not be read as age, or a
// polecat dispatched moments ago becomes a stale candidate (gt-xb27).
func TestTranscriptDatesSession(t *testing.T) {
	sessionStart := time.Now()

	if transcriptDatesSession(sessionStart.Add(-time.Hour), sessionStart) {
		t.Error("a transcript older than the session was attributed to it")
	}
	if !transcriptDatesSession(sessionStart.Add(time.Second), sessionStart) {
		t.Error("a transcript written after the session started was not attributed to it")
	}
	if !transcriptDatesSession(sessionStart, sessionStart) {
		t.Error("a transcript written at session start was not attributed to it")
	}
}

// TestAssessStall_VanishedTranscriptIsNotAStall: losing the baseline mid-window
// is an observation failure, not proof that work stopped.
func TestAssessStall_VanishedTranscriptIsNotAStall(t *testing.T) {
	start := time.Now().Add(-45 * time.Minute)
	prev := base(start)

	cur := base(start.Add(45 * time.Minute))
	cur.ActivitySource = ActivitySourceNone
	cur.LastActivity = time.Time{}
	cur.TranscriptBytes = 0
	cur.PaneSignature = prev.PaneSignature

	if verdict := AssessStall(prev, cur, 30*time.Minute); verdict.Stalled {
		t.Errorf("stall verdict after the transcript disappeared: %s", verdict.Reason)
	}
}
