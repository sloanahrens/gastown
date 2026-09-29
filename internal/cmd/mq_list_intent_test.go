package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/slot"
)

// fakeMRLister serves a fixed merge queue to listMergeQueue.
type fakeMRLister struct{ issues []*beads.Issue }

func (f fakeMRLister) ListMergeRequests(beads.ListOptions) ([]*beads.Issue, error) {
	return f.issues, nil
}

// pendingIntent is the gate intent the town's pool reports, or nil.
func pendingIntent(t *testing.T, town string) *slot.GateIntent {
	t.Helper()
	rep, err := slot.StatusPoolLocksOnly(town, slot.Pool{Slots: 4, ReservedForGate: 2, YieldToGate: true})
	if err != nil {
		t.Fatal(err)
	}
	return rep.GatePending
}

func mqListEnvFor(t *testing.T, town, role string, issues ...*beads.Issue) mqListEnv {
	t.Helper()
	return mqListEnv{
		rig:        &rig.Rig{Name: "gastown", Path: t.TempDir()},
		lister:     fakeMRLister{issues: issues},
		townRoot:   town,
		callerRole: role,
	}
}

// TestListMergeQueue_RefineryRegistersGateIntent is gt-22hdp.37: the live
// refinery found its MR with gt mq list and never ran gt mq next, so no intent
// was ever registered. gt mq list by a merge-gate role with a ready MR now
// registers it. This drives the gt mq list code path itself, so it fails if
// that call site loses its sync.
func TestListMergeQueue_RefineryRegistersGateIntent(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	mr := &beads.Issue{ID: "gt-wisp-gig", Status: "open", Description: "branch: crew/x\nrig: gastown"}

	if err := listMergeQueue(mqListEnvFor(t, town, "gastown/refinery", mr), "gastown", mqListFlags{}); err != nil {
		t.Fatal(err)
	}
	if p := pendingIntent(t, town); p == nil || p.Role != "gastown/refinery" || p.Ref != "gt-wisp-gig" {
		t.Fatalf("refinery gt mq list with a ready MR: intent = %+v, want gastown/refinery on gt-wisp-gig", p)
	}
}

// TestListMergeQueue_CrewNeverRegisters: anyone but a merge gate listing a
// non-empty queue registers nothing.
func TestListMergeQueue_CrewNeverRegisters(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	mr := &beads.Issue{ID: "gt-wisp-gig", Status: "open", Description: "rig: gastown"}
	for _, role := range []string{"gastown/crew/sloan", "gastown/main-branch-test", ""} {
		if err := listMergeQueue(mqListEnvFor(t, town, role, mr), "gastown", mqListFlags{}); err != nil {
			t.Fatal(err)
		}
		if p := pendingIntent(t, town); p != nil {
			t.Fatalf("role %q registered an intent from gt mq list: %+v", role, p)
		}
	}
}

// TestListMergeQueue_EmptyQueueClears: a whole-queue listing with no ready MR
// clears a registered intent, whoever lists.
func TestListMergeQueue_EmptyQueueClears(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := slot.RegisterGateIntent(town, "gastown", "gt-wisp-old"); err != nil {
		t.Fatal(err)
	}
	blocked := &beads.Issue{ID: "gt-wisp-b", Status: "open", BlockedByCount: 1, Description: "rig: gastown"}
	if err := listMergeQueue(mqListEnvFor(t, town, "gastown/crew/sloan", blocked), "gastown", mqListFlags{}); err != nil {
		t.Fatal(err)
	}
	if p := pendingIntent(t, town); p != nil {
		t.Fatalf("empty queue left the intent registered: %+v", p)
	}
}

// TestListMergeQueue_FilteredListingLeavesIntent: a --worker view is not the
// whole queue, so it neither registers nor clears.
func TestListMergeQueue_FilteredListingLeavesIntent(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := slot.RegisterGateIntent(town, "gastown", "gt-wisp-old"); err != nil {
		t.Fatal(err)
	}
	if err := listMergeQueue(mqListEnvFor(t, town, "gastown/refinery"), "gastown", mqListFlags{worker: "amber"}); err != nil {
		t.Fatal(err)
	}
	if p := pendingIntent(t, town); p == nil || p.Ref != "gt-wisp-old" {
		t.Fatalf("filtered listing changed the intent: %+v", p)
	}
}

// TestFirstReadyMR: the intent is labelled with a ready MR of this rig, never
// a blocked one or another rig's.
func TestFirstReadyMR(t *testing.T) {
	t.Parallel()
	blocked := &beads.Issue{ID: "gt-b", Status: "open", BlockedByCount: 1, Description: "rig: gastown"}
	other := &beads.Issue{ID: "hm-1", Status: "open", Description: "rig: hm"}
	ready := &beads.Issue{ID: "gt-r", Status: "open", Description: "rig: gastown"}
	if got := firstReadyMR([]*beads.Issue{blocked, other, ready}, "gastown"); got != "gt-r" {
		t.Fatalf("firstReadyMR = %q, want gt-r", got)
	}
	if got := firstReadyMR([]*beads.Issue{blocked, other}, "gastown"); got != "" {
		t.Fatalf("firstReadyMR with none ready = %q", got)
	}
}
