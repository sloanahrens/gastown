package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/done"
)

// updateCountingClient counts the Update calls that reach one bead. Every
// agent-bead field write is one Update and so one Dolt commit.
type updateCountingClient struct {
	beads.Client
	id      string
	updates *atomic.Int32
}

func (c updateCountingClient) Update(id string, opts beads.UpdateOptions) error {
	if id == c.id {
		c.updates.Add(1)
	}
	return c.Client.Update(id, opts)
}

// pendingCompletion is what gt done's reportDone hands the state write: the
// completion metadata and cleanup_status.
func pendingCompletion() beads.AgentFieldUpdates {
	pending := beads.CompletionUpdates(&beads.CompletionMetadata{
		ExitType:       done.ExitCompleted,
		Branch:         "polecat/nux/gt-base-123+abc",
		HookBead:       "gt-base-123",
		CompletionTime: "2026-10-13T12:00:00Z",
	})
	clean := "clean"
	pending.CleanupStatus = &clean
	return pending
}

func doneBatchTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	return townRoot
}

// TestDoneAgentStateWritesTheAgentBeadOnce: completion metadata, cleanup_status,
// the hook clear and the final agent_state are one write, not four. Each
// agent-bead write is a Dolt commit, and gt done's four were most of a polecat
// run's agent-bead churn (gt-f77fr).
func TestDoneAgentStateWritesTheAgentBeadOnce(t *testing.T) {
	t.Parallel()
	townRoot := doneBatchTown(t)
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead(), beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked)})

	var updates atomic.Int32
	agentDB := updateCountingClient{Client: db, id: "gt-gastown-polecat-nux", updates: &updates}
	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return agentDB },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
	}

	consumed, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", pendingCompletion())
	if err != nil {
		t.Fatalf("gt done: %v", err)
	}
	if !consumed {
		t.Error("consumed = false: the caller would write the completion metadata a second time")
	}
	if got := updates.Load(); got != 1 {
		t.Errorf("agent bead written %d times, want 1", got)
	}

	agent, err := db.Show("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("show agent bead: %v", err)
	}
	fields := beads.ParseAgentFields(agent.Description)
	if fields.AgentState != string(beads.AgentStateDone) {
		t.Errorf("agent_state = %q, want done", fields.AgentState)
	}
	if fields.HookBead != "" {
		t.Errorf("hook_bead = %q, want it cleared", fields.HookBead)
	}
	if fields.CleanupStatus != "clean" {
		t.Errorf("cleanup_status = %q, want clean", fields.CleanupStatus)
	}
	if fields.ExitType != done.ExitCompleted || fields.Branch != "polecat/nux/gt-base-123+abc" || fields.LastSourceIssue != "gt-base-123" || fields.CompletionTime != "2026-10-13T12:00:00Z" {
		t.Errorf("completion metadata not recorded: %+v", fields)
	}
}

// TestDoneAgentStateStillRecordsCompletionWhenTheCloseIsRefused: a gt done that
// stops before the final write (the hooked bead cannot be read) still owes the
// completion metadata and cleanup_status, the evidence reclaim reads. They go
// out alone, in one write, without clearing the hook or marking the agent done.
func TestDoneAgentStateStillRecordsCompletionWhenTheCloseIsRefused(t *testing.T) {
	t.Parallel()
	townRoot := doneBatchTown(t)
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead(), beads.Issue{ID: "gt-base-123", Title: "Base bead", Status: string(beads.StatusHooked)})

	var updates atomic.Int32
	agentDB := updateCountingClient{Client: db, id: "gt-gastown-polecat-nux", updates: &updates}
	readErr := errors.New("dial tcp 127.0.0.1:3307: connection refused")
	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return agentDB },
		source: func(string, string) beads.Client {
			return showFailsClient{Client: db, id: "gt-base-123", err: readErr}
		},
		purge: func(string, string) {},
	}

	consumed, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", pendingCompletion())
	if !errors.Is(err, readErr) {
		t.Fatalf("error = %v, want the read failure", err)
	}
	if !consumed {
		t.Error("consumed = false: the caller would write the completion metadata a second time")
	}
	if got := updates.Load(); got != 1 {
		t.Errorf("agent bead written %d times, want 1", got)
	}

	agent, err := db.Show("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("show agent bead: %v", err)
	}
	fields := beads.ParseAgentFields(agent.Description)
	if fields.CleanupStatus != "clean" || fields.ExitType != done.ExitCompleted {
		t.Errorf("completion metadata not recorded: %+v", fields)
	}
	if fields.HookBead != "gt-base-123" {
		t.Errorf("hook_bead = %q, want it left at gt-base-123", fields.HookBead)
	}
	if fields.AgentState == string(beads.AgentStateDone) {
		t.Error("gt done marked the agent done over a bead it could not read")
	}
}

// TestDoneAgentStateLeavesPendingToTheCallerWhenItCannotAddressTheBead: with no
// role to find the agent bead by, nothing is written here and consumed says so,
// so gt done writes the completion metadata itself.
func TestDoneAgentStateLeavesPendingToTheCallerWhenItCannotAddressTheBead(t *testing.T) {
	t.Parallel()
	townRoot := doneBatchTown(t)
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead())
	env := doneStateEnv{
		getenv: envMap(nil),
		routed: func(string) beads.Client { return db },
		purge:  func(string, string) {},
	}

	consumed, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", pendingCompletion())
	if err != nil {
		t.Fatalf("gt done: %v", err)
	}
	if consumed {
		t.Error("consumed = true with nothing written")
	}
}
