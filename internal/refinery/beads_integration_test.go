//go:build integration

package refinery

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/testutil"
)

// The unit tier runs the Manager and Engineer against beadsfake. These tests
// run them against bd on a Dolt test database, through the exported
// constructors: they are the wiring guard for NewManager and NewEngineer
// (which database each opens), and they cover the path only real bd has,
// ListMergeRequests' SQL read of the wisps table. They need GT_TEST_DOCKER=1
// and Docker (run them under gt slot run) and fail, never skip, without them.

// newBDRig returns a rig directory holding a fresh bd database on the test
// Dolt container, and a client on it.
func newBDRig(t *testing.T) (string, *beads.Beads) {
	t.Helper()
	if !testutil.DockerTestsEnabled() {
		t.Fatal("needs the Dolt test container: run under gt slot run with GT_TEST_DOCKER=1")
	}
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		t.Fatalf("Dolt test container: %v", err)
	}
	port, err := strconv.Atoi(testutil.DoltContainerPort())
	if err != nil || port == 0 {
		t.Fatalf("no Dolt test container (port %q): %v", testutil.DoltContainerPort(), err)
	}
	rigPath := filepath.Join(t.TempDir(), "testrig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := beads.NewIsolatedWithPort(rigPath, port)
	if err := b.Init("gt"); err != nil {
		testutil.FailContainerInit(t, b, err)
	}
	return rigPath, b
}

func mustCreate(t *testing.T, b *beads.Beads, opts beads.CreateOptions) *beads.Issue {
	t.Helper()
	is, err := b.Create(opts)
	if err != nil {
		t.Fatalf("create %q: %v", opts.Title, err)
	}
	return is
}

// TestIntegrationManagerOnBD drives NewManager's own store: the queue reads
// a durable MR and a wisp MR (the SQL path) and leaves out a closed one, and
// PostMerge closes the MR and its source and clears the agent's active_mr.
func TestIntegrationManagerOnBD(t *testing.T) {
	setupTestRegistry(t)
	rigPath, b := newBDRig(t)
	mgr := NewManager(&rig.Rig{Name: "testrig", Path: rigPath})
	mgr.SetOutput(io.Discard)

	src := mustCreate(t, b, beads.CreateOptions{Title: "Implement feature X", Labels: []string{"gt:task"}})
	agent := mustCreate(t, b, beads.CreateOptions{
		Title: "Polecat nux", Labels: []string{"gt:agent"},
		Description: "role_type: polecat\nrig: testrig\nagent_state: working\nactive_mr: null",
	})
	mr := mustCreate(t, b, beads.CreateOptions{
		Title: "MR for feature X", Labels: []string{"gt:merge-request"}, Ephemeral: true,
		Description: "branch: polecat/nux/" + src.ID + "\nsource_issue: " + src.ID + "\nworker: nux\ntarget: main\nagent_bead: " + agent.ID,
	})
	durable := mustCreate(t, b, beads.CreateOptions{
		Title: "durable MR", Labels: []string{"gt:merge-request"},
		Description: "branch: polecat/toast/gt-other\nworker: toast\ntarget: main",
	})
	done := mustCreate(t, b, beads.CreateOptions{
		Title: "merged MR", Labels: []string{"gt:merge-request"}, Ephemeral: true,
		Description: "branch: polecat/old/gt-old\nworker: old\ntarget: main",
	})
	if err := b.CloseWithReason("merged", done.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateAgentActiveMR(agent.ID, mr.ID); err != nil {
		t.Fatal(err)
	}

	queue, err := mgr.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	inQueue := map[string]bool{}
	for _, item := range queue {
		inQueue[item.MR.ID] = true
	}
	if !inQueue[mr.ID] || !inQueue[durable.ID] || inQueue[done.ID] || len(queue) != 2 {
		t.Fatalf("queue = %v, want exactly the wisp MR %s and the durable MR %s", inQueue, mr.ID, durable.ID)
	}

	result, err := mgr.PostMerge(mr.ID)
	if err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	if !result.MRClosed || !result.SourceIssueClosed || result.SourceIssueID != src.ID {
		t.Fatalf("PostMerge = %+v, want MR and %s closed", result, src.ID)
	}
	assertIssueStatus(t, b, src.ID, string(beads.StatusClosed))
	assertMRCloseReason(t, b, mr.ID, string(CloseReasonMerged))
	assertAgentActiveMR(t, b, agent.ID, "")
}

// TestIntegrationEngineerCloseMROnBD drives NewEngineer's own store through
// a rejecting close.
func TestIntegrationEngineerCloseMROnBD(t *testing.T) {
	rigPath, b := newBDRig(t)
	e := NewEngineer(&rig.Rig{Name: "testrig", Path: rigPath})
	e.output = io.Discard

	src := mustCreate(t, b, beads.CreateOptions{Title: "Implement feature X", Labels: []string{"gt:task"}})
	agent := mustCreate(t, b, beads.CreateOptions{
		Title: "Polecat nux", Labels: []string{"gt:agent"},
		Description: "role_type: polecat\nrig: testrig\nagent_state: working\nactive_mr: null",
	})
	mr := mustCreate(t, b, beads.CreateOptions{
		Title: "MR for feature X", Labels: []string{"gt:merge-request"},
		Description: "branch: polecat/test/gt-xyz\nsource_issue: " + src.ID + "\nworker: test\ntarget: main\nagent_bead: " + agent.ID,
	})
	if err := b.UpdateAgentActiveMR(agent.ID, mr.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.closeMRWithReason(&MRInfo{ID: mr.ID, AgentBead: agent.ID}, "rejected: policy failed"); err != nil {
		t.Fatalf("closeMRWithReason: %v", err)
	}
	assertIssueStatus(t, b, mr.ID, string(beads.StatusClosed))
	assertMRCloseReason(t, b, mr.ID, string(CloseReasonRejected))
	assertAgentActiveMR(t, b, agent.ID, "")
}
