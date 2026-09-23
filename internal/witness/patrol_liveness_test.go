package witness

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/guard"
)

func TestPatrolAssignee(t *testing.T) {
	cases := []struct {
		role, rig, want string
	}{
		{"deacon", "", "deacon/"},
		{"deacon", "gastown", "deacon/"}, // deacon is town-level regardless of rig
		{"witness", "gastown", "gastown/witness"},
		{"refinery", "gastown", "gastown/refinery"},
		{"unknown-role", "gastown", "unknown-role"},
	}
	for _, c := range cases {
		if got := PatrolAssignee(c.role, c.rig); got != c.want {
			t.Errorf("PatrolAssignee(%q, %q) = %q, want %q", c.role, c.rig, got, c.want)
		}
	}
}

// TestEvaluatePatrolLiveness_StaleAndAlive_Fails drives the alarm: a live
// session whose last completed patrol cycle is well past N x cadence must
// come back Fail, not Pass or Unknown. This is the exact defect gt-4z3b7
// exists to catch — a role that answers nudges but is not patrolling.
func TestEvaluatePatrolLiveness_StaleAndAlive_Fails(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	lastCompleted := now.Add(-3 * 24 * time.Hour) // 3 days ago

	result := EvaluatePatrolLiveness(PatrolLivenessInput{
		Role:                "witness",
		Rig:                 "gastown",
		SessionAlive:        true,
		LastCompleted:       lastCompleted,
		LastCompletedResult: guard.Pass(),
		Cadence:             10 * time.Minute,
		Multiplier:          3,
		Now:                 now,
	})

	if !result.IsFail() {
		t.Fatalf("expected Fail for a stale patrol on a live session, got %s", result)
	}
	if result.Err() == nil {
		t.Fatal("Fail result must carry a reason")
	}
}

// TestEvaluatePatrolLiveness_NeverCompletedAndAlive_Fails drives the alarm
// for the callback-starvation shape: bd confirms the role has never closed a
// patrol wisp at all (Fail from LastCompletedPatrol), while the session is
// alive. This must also alarm, not read as "nothing to report".
func TestEvaluatePatrolLiveness_NeverCompletedAndAlive_Fails(t *testing.T) {
	now := time.Now()

	result := EvaluatePatrolLiveness(PatrolLivenessInput{
		Role:                "deacon",
		SessionAlive:        true,
		LastCompletedResult: guard.Fail("no closed mol-deacon-patrol wisp found for deacon/"),
		Cadence:             10 * time.Minute,
		Multiplier:          3,
		Now:                 now,
	})

	if !result.IsFail() {
		t.Fatalf("expected Fail when a live session has never completed a patrol cycle, got %s", result)
	}
}

func TestEvaluatePatrolLiveness_FreshAndAlive_Passes(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	lastCompleted := now.Add(-5 * time.Minute)

	result := EvaluatePatrolLiveness(PatrolLivenessInput{
		Role:                "refinery",
		Rig:                 "gastown",
		SessionAlive:        true,
		LastCompleted:       lastCompleted,
		LastCompletedResult: guard.Pass(),
		Cadence:             10 * time.Minute,
		Multiplier:          3,
		Now:                 now,
	})

	if !result.IsPass() {
		t.Fatalf("expected Pass for a fresh patrol on a live session, got %s", result)
	}
}

// TestEvaluatePatrolLiveness_DeadSession_Passes: a dead session is not this
// detector's job — zombie/stall detection covers it — so it must never
// alarm here even when the last completion is ancient.
func TestEvaluatePatrolLiveness_DeadSession_Passes(t *testing.T) {
	now := time.Now()

	result := EvaluatePatrolLiveness(PatrolLivenessInput{
		Role:                "witness",
		Rig:                 "gastown",
		SessionAlive:        false,
		LastCompleted:       now.Add(-30 * 24 * time.Hour),
		LastCompletedResult: guard.Pass(),
		Cadence:             10 * time.Minute,
		Multiplier:          3,
		Now:                 now,
	})

	if !result.IsPass() {
		t.Fatalf("expected Pass for a dead session regardless of staleness, got %s", result)
	}
}

// TestEvaluatePatrolLiveness_UnreadableReceipt_IsUnknownNeverHealthy is the
// requirement stated verbatim in gt-4z3b7: an unreadable receipt is Unknown,
// never healthy. A caller that only branches on IsFail() and treats
// everything else as fine would misread this as Pass — this test would catch
// that regression.
func TestEvaluatePatrolLiveness_UnreadableReceipt_IsUnknownNeverHealthy(t *testing.T) {
	now := time.Now()

	result := EvaluatePatrolLiveness(PatrolLivenessInput{
		Role:                "deacon",
		SessionAlive:        true,
		LastCompletedResult: guard.Unknown(errors.New("bd list --status=closed: connection refused")),
		Cadence:             10 * time.Minute,
		Multiplier:          3,
		Now:                 now,
	})

	if !result.IsUnknown() {
		t.Fatalf("expected Unknown when the receipt could not be read, got %s", result)
	}
	if result.IsPass() {
		t.Fatal("an unreadable receipt must never read as Pass")
	}
}

func TestEvaluatePatrolLiveness_MissingCadenceConfig_IsUnknown(t *testing.T) {
	now := time.Now()

	result := EvaluatePatrolLiveness(PatrolLivenessInput{
		Role:                "witness",
		Rig:                 "gastown",
		SessionAlive:        true,
		LastCompleted:       now.Add(-time.Hour),
		LastCompletedResult: guard.Pass(),
		Cadence:             0, // not configured
		Multiplier:          3,
		Now:                 now,
	})

	if !result.IsUnknown() {
		t.Fatalf("expected Unknown when cadence is not configured, got %s", result)
	}
}

func TestEvaluatePatrolLiveness_FutureCompletion_IsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	result := EvaluatePatrolLiveness(PatrolLivenessInput{
		Role:                "refinery",
		Rig:                 "gastown",
		SessionAlive:        true,
		LastCompleted:       now.Add(time.Hour), // clock skew or bad data
		LastCompletedResult: guard.Pass(),
		Cadence:             10 * time.Minute,
		Multiplier:          3,
		Now:                 now,
	})

	if !result.IsUnknown() {
		t.Fatalf("expected Unknown for a future-dated completion, got %s", result)
	}
}

// --- LastCompletedPatrol ---

func fakeBdCli(exec func(workDir string, args ...string) (string, error)) *BdCli {
	return &BdCli{Exec: exec}
}

// TestLastCompletedPatrol_QueriesTheWispsTable guards against the regression
// this function originally shipped with: patrol wisps are ephemeral
// (bd mol wisp create) and live in the wisps table, which `bd list` cannot
// see at all (internal/beads/beads.go's listEphemeral doc comment — "bd list
// only searches the issues table and does not support an --ephemeral flag").
// Reading via `bd list` finds nothing for every role and reports Fail
// ("never patrolled") even for a perfectly healthy one. The fix is `bd query`
// with an explicit ephemeral=true clause.
func TestLastCompletedPatrol_QueriesTheWispsTable(t *testing.T) {
	var gotArgs []string
	bd := fakeBdCli(func(workDir string, args ...string) (string, error) {
		gotArgs = args
		return `[]`, nil
	})

	_, _ = LastCompletedPatrol(bd, "/tmp/rig", "gastown/witness", "mol-witness-patrol")

	if len(gotArgs) == 0 || gotArgs[0] != "query" {
		t.Fatalf("expected bd invocation to start with \"query\" (wisps live outside `bd list`'s issues-table search), got %v", gotArgs)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "ephemeral=true") {
		t.Fatalf("expected the query expression to filter ephemeral=true, got %v", gotArgs)
	}
	if !strings.Contains(joined, "gastown/witness") {
		t.Fatalf("expected the query expression to filter by assignee, got %v", gotArgs)
	}
}

func TestLastCompletedPatrol_PicksMostRecentMatchingClosedWisp(t *testing.T) {
	bd := fakeBdCli(func(workDir string, args ...string) (string, error) {
		return `[
			{"id":"gt-1","title":"mol-witness-patrol: cycle 1","status":"closed","closed_at":"2026-09-20T05:20:00Z"},
			{"id":"gt-2","title":"mol-witness-patrol: cycle 2","status":"closed","closed_at":"2026-09-22T08:00:00Z"},
			{"id":"gt-3","title":"some other closed bead","status":"closed","closed_at":"2026-09-23T00:00:00Z"}
		]`, nil
	})

	got, result := LastCompletedPatrol(bd, "/tmp/rig", "gastown/witness", "mol-witness-patrol")
	if !result.IsPass() {
		t.Fatalf("expected Pass, got %s", result)
	}
	want := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestLastCompletedPatrol_FallsBackToUpdatedAt(t *testing.T) {
	bd := fakeBdCli(func(workDir string, args ...string) (string, error) {
		return `[{"id":"gt-1","title":"mol-deacon-patrol: cycle 1","status":"closed","updated_at":"2026-09-21T00:00:00Z"}]`, nil
	})

	got, result := LastCompletedPatrol(bd, "/tmp/rig", "deacon/", "mol-deacon-patrol")
	if !result.IsPass() {
		t.Fatalf("expected Pass, got %s", result)
	}
	want := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestLastCompletedPatrol_NoneFound_Fails(t *testing.T) {
	bd := fakeBdCli(func(workDir string, args ...string) (string, error) {
		return `[]`, nil
	})

	_, result := LastCompletedPatrol(bd, "/tmp/town", "gastown/refinery", "mol-refinery-patrol")
	if !result.IsFail() {
		t.Fatalf("expected Fail when bd confirms no closed patrol wisp exists, got %s", result)
	}
	// The Fail message must name the directory queried. A query aimed at the
	// wrong database returns the same bare [] as a genuine "never patrolled"
	// (hq-3h7ac), so the escalation text is the only place an operator can
	// tell the two apart.
	if err := result.Err(); err == nil || !strings.Contains(err.Error(), "/tmp/town") {
		t.Fatalf("expected the Fail reason to name the queried directory, got %v", err)
	}
}

func TestLastCompletedPatrol_BdError_IsUnknown(t *testing.T) {
	bd := fakeBdCli(func(workDir string, args ...string) (string, error) {
		return "", errors.New("connection refused")
	})

	_, result := LastCompletedPatrol(bd, "/tmp/rig", "gastown/witness", "mol-witness-patrol")
	if !result.IsUnknown() {
		t.Fatalf("expected Unknown on a bd execution error, got %s", result)
	}
}

func TestLastCompletedPatrol_UnparsableJSON_IsUnknown(t *testing.T) {
	bd := fakeBdCli(func(workDir string, args ...string) (string, error) {
		return "not json", nil
	})

	_, result := LastCompletedPatrol(bd, "/tmp/rig", "gastown/witness", "mol-witness-patrol")
	if !result.IsUnknown() {
		t.Fatalf("expected Unknown on unparsable bd output, got %s", result)
	}
}

func TestLastCompletedPatrol_EmptyOutput_IsUnknown(t *testing.T) {
	bd := fakeBdCli(func(workDir string, args ...string) (string, error) {
		return "", nil
	})

	_, result := LastCompletedPatrol(bd, "/tmp/rig", "gastown/witness", "mol-witness-patrol")
	if !result.IsUnknown() {
		t.Fatalf("expected Unknown on empty bd output, got %s", result)
	}
}
